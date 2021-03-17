package targetconfigcontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/legacy-cloud-providers/vsphere"

	opv1 "github.com/openshift/api/operator/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"gopkg.in/gcfg.v1"
)

const (
	infraGlobalName      = "cluster"
	cloudConfigNamespace = "openshift-config"
)

type TargetConfigController struct {
	name            string
	targetNamespace string
	manifest        []byte
	kubeClient      kubernetes.Interface
	operatorClient  v1helpers.OperatorClient
	configMapLister corelister.ConfigMapLister
	infraLister     infralister.InfrastructureLister
}

func NewTargetConfigController(
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
	infraInformer := configInformer.Config().V1().Infrastructures()
	c := &TargetConfigController{
		name:            name,
		targetNamespace: targetNamespace,
		manifest:        manifest,
		kubeClient:      kubeClient,
		operatorClient:  operatorClient,
		configMapLister: configMapInformer.Lister(),
		infraLister:     infraInformer.Lister(),
	}
	return factory.New().WithInformers(
		configMapInformer.Informer(),
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

func (c TargetConfigController) sync(ctx context.Context, syncContext factory.SyncContext) error {
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

	// TODO: none of our CSI operators check whether they are running in the correct cloud. Is
	// this something we want to change? These operators are supposed to be deployed by CSO, which
	// already does this checking for us.

	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get cloud config: %w", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}

	var cfg vsphere.VSphereConfig
	err = gcfg.ReadStringInto(&cfg, cfgString)
	if err != nil {
		return err
	}

	cmString := string(c.manifest)
	for pattern, value := range map[string]string{
		"${CLUSTER_ID}":  infra.Status.InfrastructureName,
		"${VCENTER}":     cfg.Workspace.VCenterIP,
		"${DATACENTERS}": cfg.Workspace.Datacenter, // TODO: datacenters?
	} {
		cmString = strings.ReplaceAll(cmString, pattern, value)
	}

	requiredCM := resourceread.ReadConfigMapV1OrDie([]byte(cmString))

	// TODO: check if configMap has been deployed and set appropriate conditions
	_, _, err = resourceapply.ApplyConfigMap(c.kubeClient.CoreV1(), syncContext.Recorder(), requiredCM)
	if err != nil {
		return err
	}

	availableCondition := opv1.OperatorCondition{
		Type:   c.name + opv1.OperatorStatusTypeAvailable,
		Status: opv1.ConditionTrue,
	}

	progressingCondition := opv1.OperatorCondition{
		Type:   c.name + opv1.OperatorStatusTypeProgressing,
		Status: opv1.ConditionFalse,
	}

	_, _, err = v1helpers.UpdateStatus(
		c.operatorClient,
		v1helpers.UpdateConditionFn(availableCondition),
		v1helpers.UpdateConditionFn(progressingCondition),
	)
	return err
}
