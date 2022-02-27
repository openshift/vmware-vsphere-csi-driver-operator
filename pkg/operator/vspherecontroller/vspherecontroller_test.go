package vspherecontroller

import (
	"context"
	"fmt"
	"testing"
	"time"

	ocpv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	fakeconfig "github.com/openshift/client-go/config/clientset/versioned/fake"
	cfginformers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakecore "k8s.io/client-go/kubernetes/fake"
)

const (
	testControllerName = "VMwareVSphereController"
)

// fakeInstance is a fake CSI driver instance that also fullfils the OperatorClient interface
type fakeDriverInstance struct {
	metav1.ObjectMeta
	Spec   opv1.OperatorSpec
	Status opv1.OperatorStatus
}

func waitForSync(clients *utils.APIClient, stopCh <-chan struct{}) {
	clients.KubeInformers.InformersFor(defaultNamespace).WaitForCacheSync(stopCh)
	clients.KubeInformers.InformersFor("").WaitForCacheSync(stopCh)
	clients.KubeInformers.InformersFor(cloudConfigNamespace).WaitForCacheSync(stopCh)
	clients.ConfigInformers.WaitForCacheSync(stopCh)
}

func startFakeInformer(clients *utils.APIClient, stopCh <-chan struct{}) {
	for _, informer := range []interface {
		Start(stopCh <-chan struct{})
	}{
		clients.KubeInformers,
		clients.ConfigInformers,
	} {
		informer.Start(stopCh)
	}
}

func newFakeClients(coreObjects []runtime.Object, operatorObject *fakeDriverInstance, configObject runtime.Object) *utils.APIClient {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	kubeClient := fakecore.NewSimpleClientset(coreObjects...)
	kubeInformers := v1helpers.NewKubeInformersForNamespaces(kubeClient, defaultNamespace, cloudConfigNamespace, "")
	nodeInformer := kubeInformers.InformersFor("").Core().V1().Nodes()
	secretInformer := kubeInformers.InformersFor(defaultNamespace).Core().V1().Secrets()

	apiClient := &utils.APIClient{}
	apiClient.KubeClient = kubeClient
	apiClient.KubeInformers = kubeInformers
	apiClient.NodeInformer = nodeInformer
	apiClient.SecretInformer = secretInformer
	apiClient.DynamicClient = dynamicClient

	operatorClient := v1helpers.NewFakeOperatorClientWithObjectMeta(&operatorObject.ObjectMeta, &operatorObject.Spec, &operatorObject.Status, nil)
	apiClient.OperatorClient = operatorClient

	configClient := fakeconfig.NewSimpleClientset(configObject)
	configInformerFactory := cfginformers.NewSharedInformerFactory(configClient, 0)
	configInformer := configInformerFactory.Config().V1().Infrastructures().Informer()
	configInformer.GetIndexer().Add(configObject)

	apiClient.ConfigClientSet = configClient
	apiClient.ConfigInformers = configInformerFactory
	return apiClient
}

func newVsphereController(apiClients *utils.APIClient) *VSphereController {
	kubeInformers := apiClients.KubeInformers
	ocpConfigInformer := apiClients.ConfigInformers
	configMapInformer := kubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()

	infraInformer := ocpConfigInformer.Config().V1().Infrastructures()
	scInformer := kubeInformers.InformersFor("").Storage().V1().StorageClasses()
	csiDriverLister := kubeInformers.InformersFor("").Storage().V1().CSIDrivers().Lister()
	csiNodeLister := kubeInformers.InformersFor("").Storage().V1().CSINodes().Lister()
	nodeLister := apiClients.NodeInformer.Lister()
	rc := events.NewInMemoryRecorder(testControllerName)

	c := &VSphereController{
		name:            testControllerName,
		targetNamespace: defaultNamespace,
		kubeClient:      apiClients.KubeClient,
		operatorClient:  apiClients.OperatorClient,
		configMapLister: configMapInformer.Lister(),
		secretLister:    apiClients.SecretInformer.Lister(),
		csiNodeLister:   csiNodeLister,
		scLister:        scInformer.Lister(),
		csiDriverLister: csiDriverLister,
		nodeLister:      nodeLister,
		apiClients:      *apiClients,
		eventRecorder:   rc,
		vSphereChecker:  newVSphereEnvironmentChecker(),
		infraLister:     infraInformer.Lister(),
	}
	c.controllers = []conditionalController{}
	return c
}

func TestSync(t *testing.T) {
	tests := []struct {
		name                         string
		clusterCSIDriverObject       *fakeDriverInstance
		initialObjects               []runtime.Object
		configObjects                runtime.Object
		vcenterVersion               string
		startingNodeHardwareVersions []string
		finalNodeHardwareVersions    []string
		expectedConditions           []opv1.OperatorCondition
		expectError                  error
		failVCenterConnection        bool
		operandStarted               bool
	}{
		{
			name:                         "when all configuration is right",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{getConfigMap(), getSecret()},
			configObjects:                runtime.Object(getInfraObject()),
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testControllerName + opv1.OperatorStatusTypeUpgradeable,
					Status: opv1.ConditionTrue,
				},
			},
			operandStarted: true,
		},
		{
			name:                         "when we can't connect to vcenter",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{getConfigMap(), getSecret()},
			configObjects:                runtime.Object(getInfraObject()),
			failVCenterConnection:        true,
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testControllerName + opv1.OperatorStatusTypeUpgradeable,
					Status: opv1.ConditionUnknown,
				},
			},
			operandStarted: false,
		},
		{
			name:                         "when we can't connect to vcenter but CSI driver was installed previously, degrade cluster",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{getConfigMap(), getSecret(), getCSIDriver(true /*withOCPAnnotation*/)},
			configObjects:                runtime.Object(getInfraObject()),
			failVCenterConnection:        true,
			expectError:                  fmt.Errorf("can't talk to vcenter"),
			operandStarted:               true,
		},
		{
			name:                         "when vcenter version is older, block upgrades",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{getConfigMap(), getSecret()},
			configObjects:                runtime.Object(getInfraObject()),
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testControllerName + opv1.OperatorStatusTypeUpgradeable,
					Status: opv1.ConditionFalse,
				},
			},
			operandStarted: false,
		},
		{
			name:                         "when vcenter version is older but csi driver exists, degrade cluster",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{getConfigMap(), getSecret(), getCSIDriver(true)},
			configObjects:                runtime.Object(getInfraObject()),
			expectError:                  fmt.Errorf("found older vcenter version, expected is 6.7.3"),
			operandStarted:               true,
		},
		{
			name:                         "when all configuration is right, but an existing upstream CSI driver exists",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{getConfigMap(), getSecret(), getCSIDriver(false)},
			configObjects:                runtime.Object(getInfraObject()),
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testControllerName + opv1.OperatorStatusTypeUpgradeable,
					Status: opv1.ConditionFalse,
				},
			},
			operandStarted: false,
		},
		{
			name:                         "when all configuration is right, but an existing upstream CSI node object exists",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{getConfigMap(), getSecret(), getCSINode()},
			configObjects:                runtime.Object(getInfraObject()),
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testControllerName + opv1.OperatorStatusTypeUpgradeable,
					Status: opv1.ConditionFalse,
				},
			},
			operandStarted: false,
		},
		{
			name:                         "when node hw-version was old first and got upgraded",
			clusterCSIDriverObject:       makeFakeDriverInstance(),
			initialObjects:               []runtime.Object{getConfigMap(), getSecret()},
			vcenterVersion:               "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-13", "vmx-15"},
			finalNodeHardwareVersions:    []string{"vmx-15", "vmx-15"},
			configObjects:                runtime.Object(getInfraObject()),
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testControllerName + opv1.OperatorStatusTypeUpgradeable,
					Status: opv1.ConditionTrue,
				},
			},
			operandStarted: true,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			nodes := defaultNodes()
			for _, node := range nodes {
				test.initialObjects = append(test.initialObjects, runtime.Object(node))
			}

			commonApiClient := newFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)
			stopCh := make(chan struct{})
			defer close(stopCh)

			go startFakeInformer(commonApiClient, stopCh)
			if err := addInitialObjects(test.initialObjects, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			waitForSync(commonApiClient, stopCh)

			ctrl := newVsphereController(commonApiClient)

			var cleanUpFunc func()
			var conn *vclib.VSphereConnection
			var connError error
			conn, cleanUpFunc, connError = setupSimulator(defaultModel)
			if test.vcenterVersion != "" {
				customizeVCenterVersion(test.vcenterVersion, test.vcenterVersion, conn)
			}
			ctrl.vsphereConnectionFunc = makeVsphereConnectionFunc(conn, test.failVCenterConnection, connError)
			defer func() {
				if cleanUpFunc != nil {
					cleanUpFunc()
				}
			}()
			err := setHardwareVersionsFunc(nodes, conn, test.startingNodeHardwareVersions)()
			if err != nil {
				t.Fatalf("error setting hardware version for node %s", nodes[0].Name)
			}

			// Set esxi version of the only host.
			err = customizeHostVersion(defaultHostId, "7.0.2")
			if err != nil {
				t.Fatalf("Failed to customize host: %s", err)
			}

			err = ctrl.sync(context.TODO(), factory.NewSyncContext("vsphere-controller", ctrl.eventRecorder))
			if test.expectError == nil && err != nil {
				t.Fatalf("Unexpected error that could degrade cluster: %+v", err)
			}

			if test.expectError != nil && err == nil {
				t.Fatalf("expected cluster to be degraded with: %v, got none", test.expectError)
			}
			// if hardware version changes between the syncs lets rerun sync again
			if len(test.finalNodeHardwareVersions) > 0 {
				err = adjustConditionsAndResync(setHardwareVersionsFunc(nodes, conn, test.finalNodeHardwareVersions), ctrl)
			}

			_, status, _, err := ctrl.operatorClient.GetOperatorState()
			if err != nil {
				t.Errorf("failed to get operator state: %+v", err)
			}
			for i := range test.expectedConditions {
				expectedCondition := test.expectedConditions[i]
				matchingCondition := getMatchingCondition(status.Conditions, expectedCondition.Type)
				if matchingCondition == nil {
					t.Fatalf("found no matching condition for: %s", expectedCondition.Type)
				}
				if matchingCondition.Status != expectedCondition.Status {
					t.Fatalf("for condition %s: expected status: %v, got: %v", expectedCondition.Type, expectedCondition.Status, matchingCondition.Status)
				}
			}

			if test.operandStarted != ctrl.operandControllerStarted {
				t.Fatalf("expected operandStarted to be %v, got %v", test.operandStarted, ctrl.operandControllerStarted)
			}
		})
	}
}

func setHardwareVersionsFunc(nodes []*v1.Node, conn *vclib.VSphereConnection, hardwareVersions []string) func() error {
	return func() error {
		for i := range nodes {
			err := setHWVersion(conn, nodes[i], hardwareVersions[i])
			if err != nil {
				return err
			}
		}
		return nil
	}
}

func adjustConditionsAndResync(modifierFunc func() error, ctrl *VSphereController) error {
	err := modifierFunc()
	if err != nil {
		return err
	}
	envChecker, _ := ctrl.vSphereChecker.(*vSphereEnvironmentCheckerComposite)
	envChecker.nextCheck = time.Now()
	return ctrl.sync(context.TODO(), factory.NewSyncContext("vsphere-controller", ctrl.eventRecorder))
}

func makeVsphereConnectionFunc(conn *vclib.VSphereConnection, failConnection bool, connError error) func() (*vclib.VSphereConnection, checks.ClusterCheckResult, bool) {
	return func() (*vclib.VSphereConnection, checks.ClusterCheckResult, bool) {
		if failConnection {
			err := fmt.Errorf("connection to vcenter failed")
			result := checks.ClusterCheckResult{
				CheckError:   err,
				BlockUpgrade: true,
				CheckStatus:  checks.CheckStatusVSphereConnectionFailed,
				Reason:       fmt.Sprintf("Failed to connect to vSphere: %v", err),
			}
			return nil, result, false
		} else {
			if connError != nil {
				return nil, checks.MakeGenericVCenterAPIError(connError), false
			}
			return conn, checks.MakeClusterCheckResultPass(), false
		}
	}

}

func TestAddUpgradeableBlockCondition(t *testing.T) {
	controllerName := "VSphereController"
	conditionType := controllerName + opv1.OperatorStatusTypeUpgradeable

	tests := []struct {
		name              string
		clusterCSIDriver  *fakeDriverInstance
		clusterResult     checks.ClusterCheckResult
		expectedCondition opv1.OperatorCondition
		conditionModified bool
	}{
		{
			name:             "when no existing condition is found, should add condition",
			clusterCSIDriver: makeFakeDriverInstance(),
			clusterResult:    getTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
			expectedCondition: opv1.OperatorCondition{
				Type:   conditionType,
				Status: opv1.ConditionFalse,
				Reason: string(checks.CheckStatusVSphereConnectionFailed),
			},
			conditionModified: true,
		},
		{
			name: "when an existing condition is found, should not modify condition",
			clusterCSIDriver: makeFakeDriverInstance(func(instance *fakeDriverInstance) *fakeDriverInstance {
				instance.Status.Conditions = []opv1.OperatorCondition{
					{
						Type:   conditionType,
						Status: opv1.ConditionFalse,
						Reason: string(checks.CheckStatusVSphereConnectionFailed),
					},
				}
				return instance
			}),
			clusterResult: getTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
			expectedCondition: opv1.OperatorCondition{
				Type:   conditionType,
				Status: opv1.ConditionFalse,
				Reason: string(checks.CheckStatusVSphereConnectionFailed),
			},
			conditionModified: false,
		},
		{
			name: "when an existing condition is found not has different reason, should modify condition",
			clusterCSIDriver: makeFakeDriverInstance(func(instance *fakeDriverInstance) *fakeDriverInstance {
				instance.Status.Conditions = []opv1.OperatorCondition{
					{
						Type:   conditionType,
						Status: opv1.ConditionFalse,
						Reason: string(checks.CheckStatusDeprecatedVCenter),
					},
				}
				return instance
			}),
			clusterResult: getTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
			expectedCondition: opv1.OperatorCondition{
				Type:   conditionType,
				Status: opv1.ConditionFalse,
				Reason: string(checks.CheckStatusVSphereConnectionFailed),
			},
			conditionModified: true,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			commonApiClient := newFakeClients([]runtime.Object{}, test.clusterCSIDriver, getInfraObject())
			stopCh := make(chan struct{})
			defer close(stopCh)

			go startFakeInformer(commonApiClient, stopCh)
			if err := addInitialObjects([]runtime.Object{}, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			waitForSync(commonApiClient, stopCh)

			ctrl := newVsphereController(commonApiClient)
			condition, modified := ctrl.addUpgradeableBlockCondition(test.clusterResult, controllerName, &test.clusterCSIDriver.Status, opv1.ConditionFalse)
			if modified != test.conditionModified {
				t.Fatalf("expected modified condition to be %v, got %v", test.conditionModified, modified)
			}
			if condition.Type != test.expectedCondition.Type ||
				condition.Status != test.expectedCondition.Status ||
				condition.Reason != test.expectedCondition.Reason {
				t.Fatalf("expected condition to be %+v, got %+v", test.expectedCondition, condition)
			}
		})

	}
}

func addInitialObjects(objects []runtime.Object, clients *utils.APIClient) error {
	for _, obj := range objects {
		switch obj.(type) {
		case *v1.ConfigMap:
			configMapInformer := clients.KubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps().Informer()
			configMapInformer.GetStore().Add(obj)
		case *v1.Secret:
			secretInformer := clients.SecretInformer.Informer()
			secretInformer.GetStore().Add(obj)
		case *storagev1.CSIDriver:
			csiDriverInformer := clients.KubeInformers.InformersFor("").Storage().V1().CSIDrivers().Informer()
			csiDriverInformer.GetStore().Add(obj)
		case *storagev1.CSINode:
			csiNodeInformer := clients.KubeInformers.InformersFor("").Storage().V1().CSINodes().Informer()
			csiNodeInformer.GetStore().Add(obj)
		case *v1.Node:
			nodeInformer := clients.NodeInformer
			nodeInformer.Informer().GetStore().Add(obj)
		default:
			return fmt.Errorf("Unknown initalObject type: %+v", obj)
		}
	}
	return nil
}

func getMatchingCondition(status []opv1.OperatorCondition, conditionType string) *opv1.OperatorCondition {
	for _, condition := range status {
		if condition.Type == conditionType {
			return &condition
		}
	}
	return nil
}

type driverModifier func(*fakeDriverInstance) *fakeDriverInstance

func makeFakeDriverInstance(modifiers ...driverModifier) *fakeDriverInstance {
	instance := &fakeDriverInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster",
			Generation: 0,
		},
		Spec: opv1.OperatorSpec{
			ManagementState: opv1.Managed,
		},
		Status: opv1.OperatorStatus{},
	}
	for _, modifier := range modifiers {
		instance = modifier(instance)
	}
	return instance
}

func getCSIDriver(withOCPAnnotation bool) *storagev1.CSIDriver {
	driver := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: utils.VSphereDriverName,
		},
		Spec: storagev1.CSIDriverSpec{},
	}
	if withOCPAnnotation {
		driver.Annotations = map[string]string{
			utils.OpenshiftCSIDriverAnnotationKey: "true",
		}
	}
	return driver
}

func getCSINode() *storagev1.CSINode {
	return &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-abcd",
		},
		Spec: storagev1.CSINodeSpec{
			Drivers: []storagev1.CSINodeDriver{
				{
					Name: utils.VSphereDriverName,
				},
			},
		},
	}
}

func getInfraObject() *ocpv1.Infrastructure {
	return &ocpv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: infraGlobalName,
		},
		Spec: ocpv1.InfrastructureSpec{
			CloudConfig: ocpv1.ConfigMapFileReference{
				Name: "cloud-provider-config",
				Key:  "config",
			},
			PlatformSpec: ocpv1.PlatformSpec{
				Type: ocpv1.VSpherePlatformType,
			},
		},
		Status: ocpv1.InfrastructureStatus{
			InfrastructureName: "vsphere",
			PlatformStatus: &ocpv1.PlatformStatus{
				Type: ocpv1.VSpherePlatformType,
			},
		},
	}
}

func getConfigMap() *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloud-provider-config",
			Namespace: cloudConfigNamespace,
		},
		Data: map[string]string{
			"config": `
[Global]
secret-name = "vsphere-creds"
secret-namespace = "kube-system"
insecure-flag = "1"

[Workspace]
server = "localhost"
datacenter = "DC0"
default-datastore = "LocalDS_0"
folder = "/DC0/vm"

[VirtualCenter "dc0"]
datacenters = "DC0"
`,
		},
	}
}

func getSecret() *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: defaultNamespace,
		},
		Data: map[string][]byte{
			"localhost.password": []byte("vsphere-user"),
			"localhost.username": []byte("vsphere-password"),
		},
	}
}

func getTestClusterResult(statusType checks.CheckStatusType) checks.ClusterCheckResult {
	return checks.ClusterCheckResult{
		CheckError:  fmt.Errorf("some error"),
		CheckStatus: statusType,
	}
}
