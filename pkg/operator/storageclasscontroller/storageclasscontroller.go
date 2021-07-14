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

	v1 "github.com/openshift/api/operator/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
)

const (
	infraGlobalName       = "cluster"
	cloudConfigNamespace  = "openshift-config"
	DatastoreInfoProperty = "info"
	SummaryProperty       = "summary"
	resyncDuration        = 20 * time.Minute
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

type vSphereConnection struct {
	client     *govmomi.Client
	config     *vsphere.VSphereConfig
	restClient *rest.Client
	tagManager *tags.Manager
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
	scInformer := kubeInformers.InformersFor("").Storage().V1().StorageClasses()
	rc := recorder.WithComponentSuffix("vmware-" + strings.ToLower(name))
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
		// This informer isn't used anywhere else in the code, but it's added here
		// to make this controller resyncs when users change StorageClasses.
		scInformer.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		resyncDuration,
	).WithSyncDegradedOnError(
		operatorClient,
	).ToController(
		c.name,
		rc,
	)
}

func (c *StorageClassController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if opSpec.ManagementState != v1.Managed {
		return nil
	}

	controllerAvailableCondition := v1.OperatorCondition{
		Type:   c.name + v1.OperatorStatusTypeAvailable,
		Status: v1.ConditionTrue,
	}

	policyName, err := c.syncStoragePolicy(ctx)
	if err != nil {
		klog.Errorf("error syncing storage policy: %v", err)
		return err
	}

	err = c.syncStorageClass(ctx, syncContext, policyName)
	if err != nil {
		klog.Errorf("error syncing storage class: %v", err)
		return err
	}
	_, _, err = v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(controllerAvailableCondition),
	)
	return err
}

func (c *StorageClassController) syncStoragePolicy(ctx context.Context) (string, error) {
	infra, err := c.infraLister.Get(infraGlobalName)
	if err != nil {
		return "", err
	}

	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get cloud config: %v", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return "", fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}

	cfg := new(vsphere.VSphereConfig)
	err = gcfg.ReadStringInto(cfg, cfgString)
	if err != nil {
		return "", err
	}

	username, password, err := c.getCredentials(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to get credentials: %v", err)
	}
	apiClient, err := newVCenterAPI(ctx, cfg, username, password, infra)

	if err != nil {
		return "", fmt.Errorf("error connecting to vcenter API: %v", err)
	}

	// we expect all API calls to finish within apiTimeout or else operator might be stuck
	tctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	return apiClient.createStoragePolicy(tctx)
}

func (c *StorageClassController) syncStorageClass(ctx context.Context, syncContext factory.SyncContext, policyName string) error {
	scString := string(c.manifest)
	pairs := []string{
		"${STORAGE_POLICY_NAME}", policyName,
	}

	policyReplacer := strings.NewReplacer(pairs...)
	scString = policyReplacer.Replace(scString)

	sc := resourceread.ReadStorageClassV1OrDie([]byte(scString))
	_, _, err := resourceapply.ApplyStorageClass(c.kubeClient.StorageV1(), syncContext.Recorder(), sc)
	return err
}
