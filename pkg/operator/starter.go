package operator

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/targetconfigcontroller"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller"

	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
)

const (
	// Operand and operator run in the same namespace
	cloudConfigNamespace              = "openshift-config"
	operatorName                      = "vmware-vsphere-csi-driver-operator"
	operandName                       = "vmware-vsphere-csi-driver"
	secretName                        = "vmware-vsphere-cloud-credentials"
	envVMWareVsphereDriverSyncerImage = "VMWARE_VSPHERE_SYNCER_IMAGE"
	managedConfigNamespace            = "openshift-config-managed"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, utils.DefaultNamespace, cloudConfigNamespace, managedConfigNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(utils.DefaultNamespace).Core().V1().Secrets()
	configMapInformer := kubeInformersForNamespaces.InformersFor(utils.DefaultNamespace).Core().V1().ConfigMaps()
	nodeInformer := kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes()

	// Create config clientset and informer. This is used to get the cluster ID
	apiExtClient, err := apiextclient.NewForConfig(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	if err != nil {
		return err
	}

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, 20*time.Minute)

	ocpOperatorClient := operatorclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	ocpOperatorInformer := operatorinformers.NewSharedInformerFactory(ocpOperatorClient, 20*time.Minute)
	clusterCSIDriverInformer := ocpOperatorInformer.Operator().V1().ClusterCSIDrivers()

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

	commonAPIClient := utils.APIClient{
		OperatorClient:           operatorClient,
		KubeClient:               kubeClient,
		ApiExtClient:             apiExtClient,
		KubeInformers:            kubeInformersForNamespaces,
		SecretInformer:           secretInformer,
		ConfigMapInformer:        configMapInformer,
		NodeInformer:             nodeInformer,
		ConfigClientSet:          configClient,
		ConfigInformers:          configInformers,
		DynamicClient:            dynamicClient,
		ClusterCSIDriverInformer: clusterCSIDriverInformer,
	}

	cloudConfigBytes, err := assets.ReadFile("vsphere_cloud_config.yaml")
	if err != nil {
		return err
	}

	csiConfigBytes, err := assets.ReadFile("csi_cloud_config.ini")
	if err != nil {
		return err
	}

	vSphereController := vspherecontroller.NewVSphereController(
		"VMwareVSphereController",
		utils.DefaultNamespace,
		commonAPIClient,
		csiConfigBytes,
		cloudConfigBytes,
		controllerConfig.EventRecorder)

	// targetConfigController is only used for maintaining conditions which
	// were previously added by the operator
	targetConfigController := targetconfigcontroller.NewTargetConfigController(
		"VMwareVSphereDriverTargetConfigController",
		utils.DefaultNamespace,
		kubeClient,
		kubeInformersForNamespaces,
		operatorClient,
		configInformers,
		clusterCSIDriverInformer,
		controllerConfig.EventRecorder,
	)

	featureConfigBytes, err := assets.ReadFile("vsphere_features_config.yaml")
	if err != nil {
		return err
	}

	driverFeatureConfigController := NewDriverFeaturesController(
		"VMwareVSphereDriverFeatureConfigController",
		utils.DefaultNamespace,
		featureConfigBytes,
		kubeClient,
		kubeInformersForNamespaces,
		operatorClient,
		configInformers,
		clusterCSIDriverInformer,
		controllerConfig.EventRecorder,
	)

	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())
	go ocpOperatorInformer.Start(ctx.Done())

	klog.Info("Starting targetconfigcontroller")
	go targetConfigController.Run(ctx, 1)

	klog.Infof("Starting feature config controller")
	go driverFeatureConfigController.Run(ctx, 1)

	klog.Info("Starting environment check controller")
	go vSphereController.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}
