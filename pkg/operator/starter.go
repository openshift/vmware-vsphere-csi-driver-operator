package operator

import (
	"context"
	"fmt"
	"time"

	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/generated"
)

const (
	// Operand and operator run in the same namespace
	defaultNamespace = "openshift-cluster-csi-drivers"
	operatorName     = "vmware-vsphere-csi-driver-operator"
	operandName      = "vmware-vsphere-csi-driver"
	secretName       = "vmware-vsphere-cloud-credentials"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, defaultNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(defaultNamespace).Core().V1().Secrets()

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, 20*time.Minute)

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	// TODO: use const from opv1 once it's there
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, "csi.vsphere.vmware.com")
	if err != nil {
		return err
	}

	csiControllerSet := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		operandName,
		false,
	).WithStaticResourcesController(
		"VMwareVSphereDriverStaticResourcesController",
		kubeClient,
		kubeInformersForNamespaces,
		generated.Asset,
		[]string{
			"storageclass.yaml",
			"controller_sa.yaml",
			"node_sa.yaml",
			"rbac/attacher_role.yaml",
			"rbac/attacher_binding.yaml",
			"rbac/privileged_role.yaml",
			"rbac/controller_privileged_binding.yaml",
			"rbac/node_privileged_binding.yaml",
			"rbac/provisioner_role.yaml",
			"rbac/provisioner_binding.yaml",
			"rbac/resizer_role.yaml",
			"rbac/resizer_binding.yaml",
			"rbac/snapshotter_role.yaml",
			"rbac/snapshotter_binding.yaml",
		},
	).WithCSIConfigObserverController(
		"VMwareVSphereDriverCSIConfigObserverController",
		configInformers,
	).WithCSIDriverControllerService(
		"VMwareVSphereDriverControllerServiceController",
		generated.MustAsset,
		"controller.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		configInformers,
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			secretName,
			secretInformer,
		),
	).WithCSIDriverNodeService(
		"VMwareVSphereDriverNodeServiceController",
		generated.MustAsset,
		"node.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		csidrivernodeservicecontroller.WithObservedProxyDaemonSetHook(),
	).WithExtraInformers(secretInformer.Informer())

	if err != nil {
		return err
	}

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())

	klog.Info("Starting controllerset")
	go csiControllerSet.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}
