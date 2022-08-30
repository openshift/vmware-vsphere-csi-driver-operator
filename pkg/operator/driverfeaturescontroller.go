package operator

import (
	"context"
	opv1 "github.com/openshift/api/operator/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	clustercsidriverinformer "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	clustercsidriverlister "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"strings"
	"time"
)

const (
	infraGlobalName = "cluster"
	driverName      = "csi.vsphere.vmware.com"
)

type DriverFeaturesController struct {
	name                   string
	targetNamespace        string
	manifest               []byte
	kubeClient             kubernetes.Interface
	operatorClient         v1helpers.OperatorClient
	configMapLister        corelister.ConfigMapLister
	infraLister            infralister.InfrastructureLister
	clusterCSIDriverLister clustercsidriverlister.ClusterCSIDriverLister
}

func NewDriverFeaturesController(
	name string,
	namespace string,
	manifest []byte,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	operatorClient v1helpers.OperatorClient,
	configInformer configinformers.SharedInformerFactory,
	clusterCSIDriverInformer clustercsidriverinformer.ClusterCSIDriverInformer,
	recorder events.Recorder,
) factory.Controller {
	configMapInformer := kubeInformers.InformersFor(namespace).Core().V1().ConfigMaps()
	infraInformer := configInformer.Config().V1().Infrastructures()
	c := &DriverFeaturesController{
		name:                   name,
		targetNamespace:        namespace,
		manifest:               manifest,
		kubeClient:             kubeClient,
		configMapLister:        configMapInformer.Lister(),
		operatorClient:         operatorClient,
		infraLister:            infraInformer.Lister(),
		clusterCSIDriverLister: clusterCSIDriverInformer.Lister(),
	}
	return factory.New().WithInformers(
		configMapInformer.Informer(),
		infraInformer.Informer(),
		clusterCSIDriverInformer.Informer(),
	).WithSync(
		c.Sync,
	).ResyncEvery(
		time.Minute,
	).WithSyncDegradedOnError(
		operatorClient,
	).ToController(
		c.name,
		recorder.WithComponentSuffix("feature-config-controlller-"+strings.ToLower(name)),
	)
}

func (d DriverFeaturesController) Sync(ctx context.Context, controllerContext factory.SyncContext) error {
	opSpec, _, _, err := d.operatorClient.GetOperatorState()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if opSpec.ManagementState != opv1.Managed {
		return nil
	}

	infra, err := d.infraLister.Get(infraGlobalName)
	if err != nil {
		return err
	}
	// We will need eventually infra object to use defaults
	klog.Infof("got infra object: %s", infra.Name)

	clusterCSIDriver, err := d.clusterCSIDriverLister.Get(driverName)
	if err != nil {
		return err
	}

	defaultFeatureConfigMap := resourceread.ReadConfigMapV1OrDie(d.manifest)

	driverConfig := clusterCSIDriver.Spec.DriverConfig
	if driverConfig != nil {
		vsphereConfig := driverConfig.VSphere
		if vsphereConfig != nil && len(vsphereConfig.TopologyCategories) > 0 {
			existingData := defaultFeatureConfigMap.Data
			existingData["improved-volume-topology"] = "true"
			defaultFeatureConfigMap.Data = existingData
		}
	}

	_, _, err = resourceapply.ApplyConfigMap(ctx, d.kubeClient.CoreV1(), controllerContext.Recorder(), defaultFeatureConfigMap)
	if err != nil {
		return err
	}
	availableCondition := opv1.OperatorCondition{
		Type:   d.name + opv1.OperatorStatusTypeAvailable,
		Status: opv1.ConditionTrue,
	}

	progressingCondition := opv1.OperatorCondition{
		Type:   d.name + opv1.OperatorStatusTypeProgressing,
		Status: opv1.ConditionFalse,
	}

	_, _, err = v1helpers.UpdateStatus(
		ctx,
		d.operatorClient,
		v1helpers.UpdateConditionFn(availableCondition),
		v1helpers.UpdateConditionFn(progressingCondition),
	)
	return err
}
