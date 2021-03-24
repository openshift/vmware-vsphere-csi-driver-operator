package storageclasscontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/gcfg.v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/legacy-cloud-providers/vsphere"

	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/vmware/govmomi/pbm"
	"github.com/vmware/govmomi/pbm/types"
)

const (
	infraGlobalName      = "cluster"
	cloudConfigNamespace = "openshift-config"
)

type StorageClassController struct {
	name            string
	targetNamespace string
	manifest        []byte
	kubeClient      kubernetes.Interface
	operatorClient  v1helpers.OperatorClient
	configMapLister corelister.ConfigMapLister
	secretLister    corelister.SecretLister
	infraLister     infralister.InfrastructureLister
}

func NewStorageClassController(
	name,
	targetNamespace string,
	manifest []byte,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	operatorClient v1helpers.OperatorClient,
	configInformer configinformers.SharedInformerFactory,
	recorder events.Recorder,
) factory.Controller {
	configMapInformer := kubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
	secretInformer := kubeInformers.InformersFor(targetNamespace).Core().V1().Secrets()
	infraInformer := configInformer.Config().V1().Infrastructures()
	c := &StorageClassController{
		name:            name,
		targetNamespace: targetNamespace,
		manifest:        manifest,
		kubeClient:      kubeClient,
		operatorClient:  operatorClient,
		configMapLister: configMapInformer.Lister(),
		secretLister:    secretInformer.Lister(),
		infraLister:     infraInformer.Lister(),
	}
	return factory.New().WithInformers(
		configMapInformer.Informer(),
		secretInformer.Informer(),
		infraInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		time.Minute, // TODO: figure out what's the idead resync time for this controller
	).WithSyncDegradedOnError(
		operatorClient,
	).ToController(
		c.name,
		recorder.WithComponentSuffix("target-config-controller-"+strings.ToLower(name)),
	)
}

func (c *StorageClassController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	_, _, _, err := c.operatorClient.GetOperatorState()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	infra, err := c.infraLister.Get(infraGlobalName)
	if err != nil {
		return err
	}

	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get cloud config: %w", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}

	cfg := new(vsphere.VSphereConfig)
	err = gcfg.ReadStringInto(cfg, cfgString)
	if err != nil {
		return err
	}

	username, password, err := c.getCredentials(cfg)
	if err != nil {
		klog.Errorf("fjb: getCredentials: %v", err)
		return fmt.Errorf("failed to get credentials: %w", err)
	}

	client, err := c.newClient(ctx, cfg, username, password)
	if err != nil {
		klog.Errorf("fjb: newClient: %v", err)
		return fmt.Errorf("failed to create client: %w", err)
	}

	// category := "mycategory"
	var spec types.PbmCapabilityProfileCreateSpec
	spec.Name = "mystoragepolicy"
	spec.ResourceType.ResourceType = string(types.PbmProfileResourceTypeEnumSTORAGE)

	spec.Constraints = &types.PbmCapabilitySubProfileConstraints{
		SubProfiles: []types.PbmCapabilitySubProfile{
			{
				Name: "Tag based placement",
				// Capability: []types.PbmCapabilityInstance{instance},
				Capability: []types.PbmCapabilityInstance{},
			},
		},
	}

	pbmClient, err := pbm.NewClient(ctx, client.Client)
	if err != nil {
		klog.Errorf("fjb: NewClient: %v", err)
		return fmt.Errorf("failed to get pbmClient: %w", err)
	}

	pid, err := pbmClient.CreateProfile(ctx, spec)
	if err != nil {
		klog.Errorf("fjb: CreateProfile: %v", err)
		return fmt.Errorf("failed to create profile: %w", err)
	}

	klog.Infof("PBMProfileID: %+v", pid)
	return nil
}
