package vspherecontroller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	opv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	iniv1 "gopkg.in/ini.v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/component-base/metrics/testutil"
)

const (
	testControllerName = "VMwareVSphereController"
)

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

	cloudConfigBytes, _ := assets.ReadFile("vsphere_cloud_config.yaml")

	csiConfigBytes, _ := assets.ReadFile("csi_cloud_config.ini")

	c := &VSphereController{
		name:                   testControllerName,
		targetNamespace:        defaultNamespace,
		kubeClient:             apiClients.KubeClient,
		operatorClient:         apiClients.OperatorClient,
		configMapLister:        configMapInformer.Lister(),
		secretLister:           apiClients.SecretInformer.Lister(),
		csiNodeLister:          csiNodeLister,
		scLister:               scInformer.Lister(),
		csiDriverLister:        csiDriverLister,
		nodeLister:             nodeLister,
		manifest:               cloudConfigBytes,
		csiConfigManifest:      csiConfigBytes,
		apiClients:             *apiClients,
		clusterCSIDriverLister: apiClients.ClusterCSIDriverInformer.Lister(),
		eventRecorder:          rc,
		vSphereChecker:         newVSphereEnvironmentChecker(),
		infraLister:            infraInformer.Lister(),
	}
	c.controllers = []conditionalController{}
	c.storageClassController = &dummyStorageClassController{syncCalled: 0}
	return c
}

type dummyStorageClassController struct {
	syncCalled int
}

func (c *dummyStorageClassController) Sync(ctx context.Context, connection *vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) error {
	c.syncCalled += 1
	return nil
}

func TestSync(t *testing.T) {
	metricsHeader := `
        # HELP vsphere_csi_driver_error [ALPHA] vSphere driver installation error
        # TYPE vsphere_csi_driver_error gauge
        `

	tests := []struct {
		name                         string
		clusterCSIDriverObject       *testlib.FakeDriverInstance
		initialObjects               []runtime.Object
		initialErrorMetricValue      float64
		initialErrorMetricLabels     map[string]string
		skipCheck                    bool
		configObjects                runtime.Object
		vcenterVersion               string
		hostVersion                  string
		startingNodeHardwareVersions []string
		finalNodeHardwareVersions    []string
		expectedConditions           []opv1.OperatorCondition
		expectedMetrics              string
		expectError                  error
		failVCenterConnection        bool
		operandStarted               bool
		storageClassCreated          bool
	}{
		{
			name:                         "when all configuration is right",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			operandStarted:      true,
			storageClassCreated: true,
		},
		{
			name:                         "when we can't connect to vcenter",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			expectedMetrics:     `vsphere_csi_driver_error{condition="upgrade_unknown",failure_reason="vsphere_connection_failed"} 1`,
			operandStarted:      false,
			storageClassCreated: false,
		},
		{
			name:                         "when we can't connect to vcenter but CSI driver was installed previously, degrade cluster",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret(), testlib.GetCSIDriver(true /*withOCPAnnotation*/)},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
			failVCenterConnection:        true,
			expectError:                  fmt.Errorf("can't talk to vcenter"),
			operandStarted:               true,
			storageClassCreated:          false,
		},
		{
			name:                         "when vcenter version is older, block upgrades",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			operandStarted:      true,
			storageClassCreated: true,
		},
		{
			name:                         "when host version is older, block upgrades",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.1",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			expectedMetrics:     `vsphere_csi_driver_error{condition="upgrade_blocked",failure_reason="check_deprecated_esxi_version"} 1`,
			operandStarted:      true,
			storageClassCreated: true,
		},
		{
			name:                         "when vcenter version is older but csi driver exists, degrade cluster",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret(), testlib.GetCSIDriver(true)},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
			expectError:                  fmt.Errorf("found older vcenter version, expected is 6.7.3"),
			operandStarted:               true,
			storageClassCreated:          false,
		},
		{
			name:                         "when all configuration is right, but an existing upstream CSI driver exists",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret(), testlib.GetCSIDriver(false)},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			expectedMetrics: `vsphere_csi_driver_error{condition="install_blocked",failure_reason="existing_driver_found"} 1
vsphere_csi_driver_error{condition="upgrade_blocked",failure_reason="existing_driver_found"} 1`,
			operandStarted:      false,
			storageClassCreated: false,
		},
		{
			name:                         "when all configuration is right, but an existing upstream CSI node object exists",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret(), testlib.GetCSINode()},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			expectedMetrics: `vsphere_csi_driver_error{condition="install_blocked",failure_reason="existing_driver_found"} 1
vsphere_csi_driver_error{condition="upgrade_blocked",failure_reason="existing_driver_found"} 1`,
			operandStarted:      false,
			storageClassCreated: false,
		},
		{
			name:                         "when node hw-version was old first and got upgraded",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.2",
			startingNodeHardwareVersions: []string{"vmx-13", "vmx-15"},
			finalNodeHardwareVersions:    []string{"vmx-15", "vmx-15"},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			operandStarted:      true,
			storageClassCreated: true,
		},
		{
			name:                         "sync before the next recheck interval",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			skipCheck:                    true,
			initialErrorMetricValue:      1,
			initialErrorMetricLabels:     map[string]string{"condition": "install_blocked", "failure_reason": "existing_driver_found"},
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			vcenterVersion:               "7.0.2",
			configObjects:                runtime.Object(testlib.GetInfraObject()),
			operandStarted:               false,
			// The metrics is not reset when no checks actually run.
			expectedMetrics: `vsphere_csi_driver_error{condition="install_blocked",failure_reason="existing_driver_found"} 1`,
		},
		{
			name:                         "when vcenter version is 7.0.1 and csi driver exists, mark upgradeable: false",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			vcenterVersion:               "7.0.1", // Minimum for upgrade is 7.0.2
			hostVersion:                  "7.0.2",
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret(), testlib.GetCSIDriver(true)},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			operandStarted:      true,
			storageClassCreated: true,
		},
		{
			name:                         "when host version is 7.0.1 and csi driver exists, mark upgradeable: false",
			clusterCSIDriverObject:       testlib.MakeFakeDriverInstance(),
			startingNodeHardwareVersions: []string{"vmx-15", "vmx-15"},
			vcenterVersion:               "7.0.2",
			hostVersion:                  "7.0.1", // Minimum for upgrade is 7.0.2
			initialObjects:               []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret(), testlib.GetCSIDriver(true)},
			configObjects:                runtime.Object(testlib.GetInfraObject()),
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
			operandStarted:      true,
			storageClassCreated: true,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			// These tests can't run in parallel!
			utils.InstallErrorMetric.Reset()
			utils.InitGlobalState()

			if test.initialErrorMetricLabels != nil {
				utils.InstallErrorMetric.With(test.initialErrorMetricLabels).Set(test.initialErrorMetricValue)
			}

			nodes := defaultNodes()
			for _, node := range nodes {
				test.initialObjects = append(test.initialObjects, runtime.Object(node))
			}
			commonApiClient := testlib.NewFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)
			clusterCSIDriver := testlib.GetClusterCSIDriver(false)
			testlib.AddClusterCSIDriverClient(commonApiClient, clusterCSIDriver)

			test.initialObjects = append(test.initialObjects, runtime.Object(clusterCSIDriver))

			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects(test.initialObjects, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			testlib.WaitForSync(commonApiClient, stopCh)

			ctrl := newVsphereController(commonApiClient)
			scController := ctrl.storageClassController.(*dummyStorageClassController)

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

			hostVersion := test.hostVersion
			if hostVersion == "" {
				hostVersion = "7.0.2"
			}
			err = customizeHostVersion(defaultHostId, hostVersion)
			if err != nil {
				t.Fatalf("Failed to customize host: %s", err)
			}

			if test.skipCheck {
				ctrl.vSphereChecker = newSkippingChecker()
			}

			err = ctrl.sync(context.TODO(), factory.NewSyncContext("vsphere-controller", ctrl.eventRecorder))
			if test.expectError == nil && err != nil {
				t.Fatalf("Unexpected error that could degrade cluster: %+v", err)
			}

			// check storageclass results
			if test.storageClassCreated && scController.syncCalled == 0 {
				t.Fatalf("expected storageclass to be created")
			}

			if !test.storageClassCreated && scController.syncCalled > 0 {
				t.Fatalf("unexpected storageclass created")
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
				matchingCondition := testlib.GetMatchingCondition(status.Conditions, expectedCondition.Type)
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

			if test.expectedMetrics != "" {
				if err := testutil.CollectAndCompare(utils.InstallErrorMetric, strings.NewReader(metricsHeader+test.expectedMetrics+"\n"), utils.InstallErrorMetric.Name); err != nil {
					t.Errorf("wrong metrics: %s", err)
				}
			}
		})
	}
}

func TestApplyClusterCSIDriver(t *testing.T) {
	tests := []struct {
		name               string
		clusterCSIDriver   *opv1.ClusterCSIDriver
		operatorObj        *testlib.FakeDriverInstance
		expectedTopology   string
		configFileName     string
		expectedDatacenter string
		expectError        bool
	}{
		{
			name:               "when driver does not have topology enabled",
			clusterCSIDriver:   testlib.GetClusterCSIDriver(false),
			operatorObj:        testlib.MakeFakeDriverInstance(),
			expectedDatacenter: "Datacenter",
			expectedTopology:   "",
		},
		{
			name:               "when driver does have topology enabled",
			clusterCSIDriver:   testlib.GetClusterCSIDriver(true),
			operatorObj:        testlib.MakeFakeDriverInstance(),
			expectedDatacenter: "Datacenter",
			expectedTopology:   "k8s-zone,k8s-region",
		},
		{
			name:             "when configuration has more than one vcenter",
			clusterCSIDriver: testlib.GetClusterCSIDriver(true),
			operatorObj:      testlib.MakeFakeDriverInstance(),
			configFileName:   "multiple_vc.ini",
			expectError:      true,
		},
		{
			name:               "when configuration has more than one datacenter",
			clusterCSIDriver:   testlib.GetClusterCSIDriver(true),
			operatorObj:        testlib.MakeFakeDriverInstance(),
			configFileName:     "multiple_dc.ini",
			expectedDatacenter: "Datacentera, DatacenterB",
			expectedTopology:   "k8s-zone,k8s-region",
		},
	}

	for i := range tests {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			infra := testlib.GetInfraObject()
			commonApiClient := testlib.NewFakeClients([]runtime.Object{}, tc.operatorObj, infra)
			testlib.AddClusterCSIDriverClient(commonApiClient, tc.clusterCSIDriver)
			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects([]runtime.Object{tc.clusterCSIDriver}, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			testlib.WaitForSync(commonApiClient, stopCh)
			ctrl := newVsphereController(commonApiClient)

			legacyVsphereConfig, err := testlib.GetLegacyVSphereConfig(tc.configFileName)
			if err != nil {
				t.Fatalf("error loading legacy vsphere config: %v", err)
			}

			configMap, err := ctrl.applyClusterCSIDriverChange(infra, legacyVsphereConfig, tc.clusterCSIDriver, "foobar")

			// if we expected error and we got some, we should stop running this test
			if tc.expectError && err != nil {
				return
			}

			if tc.expectError && err == nil {
				t.Fatal("Expected error got none")
			}
			if err != nil {
				t.Fatalf("error creating configmap: %v", err)
			}

			configMapIni := configMap.Data["cloud.conf"]
			csiConfig, err := iniv1.Load([]byte(configMapIni))
			if err != nil {
				t.Fatalf("error loading result ini: %v", err)
			}

			labelSection, _ := csiConfig.Section("Labels").GetKey("topology-categories")
			if tc.expectedTopology == "" && labelSection != nil {
				t.Fatalf("unexpected topology found %v", labelSection)
			}
			if tc.expectedTopology != "" {
				if labelSection == nil || labelSection.String() != tc.expectedTopology {
					t.Fatalf("expected topology %v, unexpected topology found %v", tc.expectedTopology, labelSection)
				}
			}
			datacenters, err := csiConfig.Section("VirtualCenter \"foobar.lan\"").GetKey("datacenters")
			if err != nil {
				t.Fatalf("error getting datacenters: %v", err)
			}
			if datacenters.String() != tc.expectedDatacenter {
				t.Fatalf("expected datacenter to be %s, got %s", tc.expectedDatacenter, datacenters.String())
			}

			datastoreURL, err := csiConfig.Section("VirtualCenter \"foobar.lan\"").GetKey("migration-datastore-url")
			if err != nil {
				t.Fatalf("error getting datasore url: %v", err)
			}
			if datastoreURL.String() != "foobar" {
				t.Fatalf("expected datastoreURL to be %s got %s", "foobar", datastoreURL)
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
				CheckError:  err,
				Action:      checks.CheckActionBlockUpgradeOrDegrade,
				CheckStatus: checks.CheckStatusVSphereConnectionFailed,
				Reason:      fmt.Sprintf("Failed to connect to vSphere: %v", err),
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
		clusterCSIDriver  *testlib.FakeDriverInstance
		clusterResult     checks.ClusterCheckResult
		expectedCondition opv1.OperatorCondition
		conditionModified bool
	}{
		{
			name:             "when no existing condition is found, should add condition",
			clusterCSIDriver: testlib.MakeFakeDriverInstance(),
			clusterResult:    testlib.GetTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
			expectedCondition: opv1.OperatorCondition{
				Type:   conditionType,
				Status: opv1.ConditionFalse,
				Reason: string(checks.CheckStatusVSphereConnectionFailed),
			},
			conditionModified: true,
		},
		{
			name: "when an existing condition is found, should not modify condition",
			clusterCSIDriver: testlib.MakeFakeDriverInstance(func(instance *testlib.FakeDriverInstance) *testlib.FakeDriverInstance {
				instance.Status.Conditions = []opv1.OperatorCondition{
					{
						Type:   conditionType,
						Status: opv1.ConditionFalse,
						Reason: string(checks.CheckStatusVSphereConnectionFailed),
					},
				}
				return instance
			}),
			clusterResult: testlib.GetTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
			expectedCondition: opv1.OperatorCondition{
				Type:   conditionType,
				Status: opv1.ConditionFalse,
				Reason: string(checks.CheckStatusVSphereConnectionFailed),
			},
			conditionModified: false,
		},
		{
			name: "when an existing condition is found not has different reason, should modify condition",
			clusterCSIDriver: testlib.MakeFakeDriverInstance(func(instance *testlib.FakeDriverInstance) *testlib.FakeDriverInstance {
				instance.Status.Conditions = []opv1.OperatorCondition{
					{
						Type:   conditionType,
						Status: opv1.ConditionFalse,
						Reason: string(checks.CheckStatusDeprecatedVCenter),
					},
				}
				return instance
			}),
			clusterResult: testlib.GetTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
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
			commonApiClient := testlib.NewFakeClients([]runtime.Object{}, test.clusterCSIDriver, testlib.GetInfraObject())
			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects([]runtime.Object{}, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			testlib.WaitForSync(commonApiClient, stopCh)

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

// This dummy vSphereEnvironmentCheckInterface implementation never runs any platform checks.
type skippingChecker struct{}

func (*skippingChecker) Check(ctx context.Context, connection checks.CheckArgs) (time.Duration, checks.ClusterCheckResult, bool) {
	return 0, checks.ClusterCheckResult{}, false
}

func newSkippingChecker() *skippingChecker {
	return &skippingChecker{}
}
