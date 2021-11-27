package operator

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/environmentchecker"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"

	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/targetconfigcontroller"
)

const (
	// Operand and operator run in the same namespace
	defaultNamespace                  = "openshift-cluster-csi-drivers"
	cloudConfigNamespace              = "openshift-config"
	operatorName                      = "vmware-vsphere-csi-driver-operator"
	operandName                       = "vmware-vsphere-csi-driver"
	secretName                        = "vmware-vsphere-cloud-credentials"
	envVMWareVsphereDriverSyncerImage = "VMWARE_VSPHERE_SYNCER_IMAGE"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, defaultNamespace, cloudConfigNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(defaultNamespace).Core().V1().Secrets()
	nodeInformer := kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes()

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, 20*time.Minute)

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(
		controllerConfig.KubeConfig,
		gvr,
		string(opv1.VSphereCSIDriver),
	)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	commonApiClient := utils.ApiClients{
		OperatorClient:  operatorClient,
		KubeClient:      kubeClient,
		KubeInformers:   kubeInformersForNamespaces,
		SecretInformer:  secretInformer,
		NodeInformer:    nodeInformer,
		ConfigClientSet: configClient,
		ConfigInformers: configInformers,
		DynamicClient:   dynamicClient,
	}

	environmentCheckController := environmentchecker.NewEnvironmentCheckController(
		"VMwareVSphereEnvironmentCheckController",
		defaultNamespace,
		commonApiClient,
		controllerConfig.EventRecorder)

	if err != nil {
		return err
	}

	cloudConfigBytes, err := assets.ReadFile("vsphere_cloud_config.yaml")
	if err != nil {
		return err
	}
	targetConfigController := targetconfigcontroller.NewTargetConfigController(
		"VMwareVSphereDriverTargetConfigController",
		defaultNamespace,
		cloudConfigBytes,
		kubeClient,
		kubeInformersForNamespaces,
		operatorClient,
		configInformers,
		controllerConfig.EventRecorder,
	)

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())

	klog.Info("Starting targetconfigcontroller")
	go targetConfigController.Run(ctx, 1)

	klog.Info("Starting environment check controller")
	go environmentCheckController.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}
