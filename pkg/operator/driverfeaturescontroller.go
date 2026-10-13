package operator

import (
	"context"
	"strings"
	"time"

	cfgv1 "github.com/openshift/api/config/v1"
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
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
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
		recorder.WithComponentSuffix("feature-config-controller-"+strings.ToLower(name)),
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

	clusterCSIDriver, err := d.clusterCSIDriverLister.Get(utils.VSphereDriverName)
	if err != nil {
		return err
	}

	infra, err := d.infraLister.Get(utils.InfraGlobalName)
	if err != nil {
		return err
	}

	defaultFeatureConfigMap := resourceread.ReadConfigMapV1OrDie(d.manifest)

	topologyCategories := utils.GetTopologyCategories(clusterCSIDriver, infra)
	if len(topologyCategories) > 0 {
		defaultFeatureConfigMap.Data["improved-volume-topology"] = "true"
	}

	if !isCSIMigrationSupported(infra) {
		klog.V(4).Infof("Disabling CSI migration")
		defaultFeatureConfigMap.Data["csi-migration"] = "false"
	}

	_, _, err = resourceapply.ApplyConfigMap(ctx, d.kubeClient.CoreV1(), controllerContext.Recorder(), defaultFeatureConfigMap)
	return err
}

func isCSIMigrationSupported(infra *cfgv1.Infrastructure) bool {
	// CSI migration is supported on all vSphere clusters unless they have 2 or more vCenters
	if infra.Spec.PlatformSpec.VSphere == nil {
		// Assume a default vSphere configuration with 1 vCenter
		return true
	}
	if len(infra.Spec.PlatformSpec.VSphere.VCenters) > 1 {
		return false
	}
	return true
}
