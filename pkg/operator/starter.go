package operator

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	applyopv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
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
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, utils.DefaultNamespace, cloudConfigNamespace, "")
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
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(
		clock.RealClock{},
		controllerConfig.KubeConfig,
		opv1.SchemeGroupVersion.WithResource("clustercsidrivers"),
		opv1.SchemeGroupVersion.WithKind("ClusterCSIDriver"),
		string(opv1.VSphereCSIDriver),
		extractOperatorSpec,
		extractOperatorStatus,
	)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	desiredVersion := getReleaseVersion()
	missingVersion := "0.0.1-snapshot"

	// By default, this will exit(0) if the featuregates change
	featureGateAccessor := featuregates.NewFeatureGateAccess(
		desiredVersion, missingVersion,
		configInformers.Config().V1().ClusterVersions(),
		configInformers.Config().V1().FeatureGates(),
		events.NewLoggingEventRecorder("vspherecontroller"),
	)
	go featureGateAccessor.Run(context.Background())
	go configInformers.Start(context.Background().Done())

	select {
	case <-featureGateAccessor.InitialFeatureGatesObserved():
		featureGates, _ := featureGateAccessor.CurrentFeatureGates()
		klog.Infof("FeatureGates initialized: %v", featureGates.KnownFeatures())
	case <-time.After(1 * time.Minute):
		klog.Fatal("timed out waiting for FeatureGate detection")
	}

	featureGates, err := featureGateAccessor.CurrentFeatureGates()
	if err != nil {
		klog.Fatalf("unable to retrieve current feature gates: %v", err)
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

	cloudConfigBytes, err := assets.ReadFile("vsphere_cloud_config_secret.yaml")
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
		controllerConfig.EventRecorder,
		featureGates)

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

	klog.Infof("Starting feature config controller")
	go driverFeatureConfigController.Run(ctx, 1)

	klog.Info("Starting environment check controller")
	go vSphereController.Run(ctx, 1)

	<-ctx.Done()

	return nil
}

func getReleaseVersion() string {
	releaseVersion := os.Getenv("RELEASE_VERSION")
	if len(releaseVersion) == 0 {
		return "0.0.1-snapshot"
	}
	return releaseVersion
}

func extractOperatorSpec(obj *unstructured.Unstructured, fieldManager string) (*applyopv1.OperatorSpecApplyConfiguration, error) {
	castObj := &opv1.ClusterCSIDriver{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, castObj); err != nil {
		return nil, fmt.Errorf("unable to convert to OpenShiftAPIServer: %w", err)
	}
	ret, err := applyopv1.ExtractClusterCSIDriver(castObj, fieldManager)
	if err != nil {
		return nil, fmt.Errorf("unable to extract fields for %q: %w", fieldManager, err)
	}
	if ret.Spec == nil {
		return nil, nil
	}
	return &ret.Spec.OperatorSpecApplyConfiguration, nil
}

func extractOperatorStatus(obj *unstructured.Unstructured, fieldManager string) (*applyopv1.OperatorStatusApplyConfiguration, error) {
	castObj := &opv1.ClusterCSIDriver{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, castObj); err != nil {
		return nil, fmt.Errorf("unable to convert to OpenShiftAPIServer: %w", err)
	}
	ret, err := applyopv1.ExtractClusterCSIDriverStatus(castObj, fieldManager)
	if err != nil {
		return nil, fmt.Errorf("unable to extract fields for %q: %w", fieldManager, err)
	}

	if ret.Status == nil {
		return nil, nil
	}
	return &ret.Status.OperatorStatusApplyConfiguration, nil
}
