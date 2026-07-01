package storageclasscontroller

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	csiscc "github.com/openshift/library-go/pkg/operator/csi/csistorageclasscontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
)

const (
	testScControllerName      = "test-sc-controller"
	testScControllerNamespace = "test-sc-namespace"
)

type fakeStoragePolicyAPI struct {
	vCenterInterface
	apiCallCount int
	ret          string
	err          error
}

func (v *fakeStoragePolicyAPI) createStoragePolicy(ctx context.Context) (string, error) {
	return v.ret, v.err
}

func newFakeStoragePolicyAPISuccess(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
	return &fakeStoragePolicyAPI{ret: "fake-return-value"}
}

func newFakeStoragePolicyAPIFailure(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
	return &fakeStoragePolicyAPI{ret: "fake-return-value", err: fmt.Errorf("fake-error")}
}

func newStorageClassController(apiClients *utils.APIClient, storageclassfile string, storagePolicyAPIfailing bool) *StorageClassController {
	rc := events.NewInMemoryRecorder(testScControllerName, clock.RealClock{})
	scBytes, err := testlib.ReadFile(storageclassfile)
	if err != nil {
		panic("unable to read storageclass file")
	}

	spFunc := newFakeStoragePolicyAPISuccess
	if storagePolicyAPIfailing {
		spFunc = newFakeStoragePolicyAPIFailure
	}

	evaluator := csiscc.NewStorageClassStateEvaluator(
		apiClients.KubeClient,
		apiClients.ClusterCSIDriverInformer.Lister(),
		rc,
	)

	c := &StorageClassController{
		name:                 testScControllerName,
		targetNamespace:      testScControllerNamespace,
		manifest:             scBytes,
		kubeClient:           apiClients.KubeClient,
		operatorClient:       apiClients.OperatorClient,
		storageClassLister:   apiClients.KubeInformers.InformersFor("").Storage().V1().StorageClasses().Lister(),
		recorder:             rc,
		makeStoragePolicyAPI: spFunc,
		scStateEvaluator:     evaluator,
		vCenterStoragePolicy: make(map[string]string),
		backoffStates:        make(map[string]*vCenterBackoffState),
		pendingOrphans:       make(map[string]int),
	}

	return c
}

func getCheckAPIDependency(apiClients *utils.APIClient) checks.KubeAPIInterface {
	kubeInformers := apiClients.KubeInformers

	csiDriverLister := kubeInformers.InformersFor("").Storage().V1().CSIDrivers().Lister()
	clusterCSIDriverLister := apiClients.ClusterCSIDriverInformer.Lister()
	csiNodeLister := kubeInformers.InformersFor("").Storage().V1().CSINodes().Lister()
	nodeLister := apiClients.NodeInformer.Lister()
	i := &checks.KubeAPIInterfaceImpl{
		Infrastructure:         testlib.GetInfraObject(),
		CSINodeLister:          csiNodeLister,
		CSIDriverLister:        csiDriverLister,
		ClusterCSIDriverLister: clusterCSIDriverLister,
		NodeLister:             nodeLister,
	}

	return i
}

func assertPanic(t *testing.T) {
	if r := recover(); r == nil {
		t.Errorf("Test should have panicked but did not.")
	}
}

func TestSync(t *testing.T) {
	tests := []struct {
		name                   string
		clusterCSIDriverObject *testlib.FakeDriverInstance
		initialObjects         []runtime.Object
		configObjects          runtime.Object
		storageClass           string
		expectError            error
		expectedConditions     []opv1.OperatorCondition
		scConstructor          interface{}
		StoragePolicyAPIfails  bool
		shouldPanic            bool
	}{
		{
			name:                   "sync succeeds with valid storage class",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			storageClass:           "storageclass1.yaml",
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeDegraded,
					Status: opv1.ConditionFalse,
				},
			},
		},
		{
			name:                   "sync degrades on storage policy api error",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			storageClass:           "storageclass1.yaml",
			StoragePolicyAPIfails:  true,
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionFalse,
				},
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeDegraded,
					Status: opv1.ConditionTrue,
				},
			},
		},
		{
			name:                   "sync panics with invalid storage class object",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			storageClass:           "storageclass2.yaml",
			shouldPanic:            true,
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			commonApiClient := testlib.NewFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)

			apiDeps := getCheckAPIDependency(commonApiClient)
			var conn vclib.VSphereConnection
			conns := []*vclib.VSphereConnection{&conn}
			scController := newStorageClassController(commonApiClient, test.storageClass, test.StoragePolicyAPIfails)

			if test.shouldPanic {
				defer assertPanic(t)
			}
			// err will be nil on even on failure, need to check conditions instead
			err := scController.Sync(context.TODO(), conns, apiDeps)
			if err != nil {
				t.Errorf("failed to sync controller: %+v", err)
			}

			_, status, _, err := scController.operatorClient.GetOperatorState()
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

		})
	}
}

func TestSyncMultiple(t *testing.T) {
	tests := []struct {
		name                   string
		storagePolicySyncFails bool
		expectError            bool
	}{
		{
			name:                   "when policy sync is successful",
			storagePolicySyncFails: false,
			expectError:            false,
		},
		{
			name:                   "when policy sync failed",
			storagePolicySyncFails: true,
			expectError:            true,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
			clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
			configObjects := runtime.Object(testlib.GetInfraObject())
			commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
			storageClass := "storageclass1.yaml"
			apiDeps := getCheckAPIDependency(commonApiClient)
			conn := vclib.VSphereConnection{
				Hostname: "test",
			}
			scController := newStorageClassController(commonApiClient, storageClass, test.storagePolicySyncFails)

			// err will be nil on even on failure, need to check conditions instead
			policyName, clusterCheckResult := scController.syncStoragePolicy(context.TODO(), &conn, apiDeps, opv1.ManagedStorageClass)
			scController.sharedPolicyName = policyName

			if test.expectError {
				if clusterCheckResult.CheckError == nil {
					t.Errorf("Expected error got none")
				}
				if len(policyName) > 0 {
					t.Errorf("Unexpected policy name")
				}
			} else {
				if clusterCheckResult.CheckError != nil {
					t.Errorf("Expected no error got: %v", clusterCheckResult.CheckError)
				}
			}

			policyName, clusterCheckResult = scController.syncStoragePolicy(context.TODO(), &conn, apiDeps, opv1.ManagedStorageClass)
			if test.expectError {
				if clusterCheckResult.CheckError == nil {
					t.Errorf("Expected error got none")
				}
				if len(policyName) > 0 {
					t.Errorf("Unexpected policy name")
				}
			} else {
				if clusterCheckResult.CheckError != nil {
					t.Errorf("Expected no error got: %v", clusterCheckResult.CheckError)
				}
			}
		})
	}
}

func TestBackoffReset(t *testing.T) {
	tests := []struct {
		name string
		// sequence of sync results: true=success, false=failure
		syncSequence []bool
		// expected backoff duration after each sync (approximate, considering jitter)
		expectedDurations []time.Duration
	}{
		{
			name:         "success uses 10-minute interval",
			syncSequence: []bool{true, true, true},
			// Success always uses successBackoff.Duration (10 min)
			expectedDurations: []time.Duration{10 * time.Minute, 10 * time.Minute, 10 * time.Minute},
		},
		{
			name:         "fail 3x then succeed resets error backoff",
			syncSequence: []bool{false, false, false, true, false},
			// fail: 1m, fail: 2m, fail: 4m, success: 10m, fail: 1m (error backoff reset by success)
			expectedDurations: []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 10 * time.Minute, time.Minute},
		},
		{
			name:         "continuous failure caps at 30 minutes",
			syncSequence: []bool{false, false, false, false, false, false, false},
			expectedDurations: []time.Duration{
				time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
				16 * time.Minute, 30 * time.Minute, 30 * time.Minute,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
			clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
			configObjects := runtime.Object(testlib.GetInfraObject())
			commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
			apiDeps := getCheckAPIDependency(commonApiClient)
			conn := &vclib.VSphereConnection{Hostname: "test-vcenter"}

			scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)

			for i, shouldSucceed := range test.syncSequence {
				if shouldSucceed {
					scController.makeStoragePolicyAPI = newFakeStoragePolicyAPISuccess
				} else {
					scController.makeStoragePolicyAPI = newFakeStoragePolicyAPIFailure
				}

				bs := scController.getBackoffState(conn.Hostname)
				bs.nextCheck = time.Time{}
				scController.vCenterStoragePolicy[conn.Hostname] = ""

				beforeSync := time.Now()
				policyName, clusterCheckResult := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass)
				scController.vCenterStoragePolicy[conn.Hostname] = policyName

				if shouldSucceed {
					if clusterCheckResult.CheckError != nil {
						t.Fatalf("step %d: expected success, got error: %v", i, clusterCheckResult.CheckError)
					}
				} else {
					if clusterCheckResult.CheckError == nil {
						t.Fatalf("step %d: expected error, got none", i)
					}
				}

				bs = scController.getBackoffState(conn.Hostname)
				actualDelay := bs.nextCheck.Sub(beforeSync)
				expectedDelay := test.expectedDurations[i]
				tolerance := float64(expectedDelay) * 0.05
				if actualDelay < expectedDelay-time.Duration(tolerance) || actualDelay > expectedDelay+time.Duration(tolerance) {
					t.Errorf("step %d: expected delay ~%v, got %v", i, expectedDelay, actualDelay)
				}
			}
		})
	}
}

func TestPerVCenterBackoff(t *testing.T) {
	initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
	clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
	configObjects := runtime.Object(testlib.GetInfraObject())
	commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
	apiDeps := getCheckAPIDependency(commonApiClient)

	connA := &vclib.VSphereConnection{Hostname: "vcenter-a"}
	connB := &vclib.VSphereConnection{Hostname: "vcenter-b"}

	scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)

	// Fail vcenter-a 3 times
	scController.makeStoragePolicyAPI = newFakeStoragePolicyAPIFailure
	for i := 0; i < 3; i++ {
		bs := scController.getBackoffState(connA.Hostname)
		bs.nextCheck = time.Time{}
		scController.vCenterStoragePolicy[connA.Hostname] = ""
		scController.syncStoragePolicy(context.TODO(), connA, apiDeps, opv1.ManagedStorageClass)
	}

	// vcenter-b succeeds on first try
	scController.makeStoragePolicyAPI = newFakeStoragePolicyAPISuccess
	bsB := scController.getBackoffState(connB.Hostname)
	bsB.nextCheck = time.Time{}
	scController.vCenterStoragePolicy[connB.Hostname] = ""
	beforeB := time.Now()
	scController.syncStoragePolicy(context.TODO(), connB, apiDeps, opv1.ManagedStorageClass)

	bsA := scController.getBackoffState(connA.Hostname)
	bsB = scController.getBackoffState(connB.Hostname)

	// vcenter-a should have escalated backoff (~4m after 3 failures)
	aDelay := bsA.nextCheck.Sub(bsA.lastCheck)
	if aDelay < 3*time.Minute || aDelay > 5*time.Minute {
		t.Errorf("vcenter-a: expected backoff ~4m after 3 failures, got %v", aDelay)
	}

	// vcenter-b should have success interval (10m)
	bDelay := bsB.nextCheck.Sub(beforeB)
	tolerance := float64(successCheckInterval) * 0.05
	if bDelay < successCheckInterval-time.Duration(tolerance) || bDelay > successCheckInterval+time.Duration(tolerance) {
		t.Errorf("vcenter-b: expected success interval ~%v, got %v", successCheckInterval, bDelay)
	}
}

func TestPendingOrphansForceResync(t *testing.T) {
	initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
	clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
	configObjects := runtime.Object(testlib.GetInfraObject())
	commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
	apiDeps := getCheckAPIDependency(commonApiClient)

	conn := &vclib.VSphereConnection{Hostname: "test-vcenter"}

	scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)

	// First sync: success, stores policy, sets backoff to successCheckInterval
	scController.makeStoragePolicyAPI = newFakeStoragePolicyAPISuccess
	bs := scController.getBackoffState(conn.Hostname)
	bs.nextCheck = time.Time{}
	policyName, result := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass)
	if result.CheckError != nil {
		t.Fatalf("initial sync failed: %v", result.CheckError)
	}
	scController.vCenterStoragePolicy[conn.Hostname] = policyName

	// Now within backoff window — a normal sync would skip
	// Verify skip works when no pending orphans
	apiCalled := false
	scController.makeStoragePolicyAPI = func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
		apiCalled = true
		return &fakeStoragePolicyAPI{ret: "updated-policy"}
	}
	policyName2, result2 := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass)
	if result2.CheckError != nil {
		t.Fatalf("skip sync failed: %v", result2.CheckError)
	}
	if apiCalled {
		t.Error("expected sync to skip within backoff window when no pending orphans")
	}
	if policyName2 != policyName {
		t.Errorf("expected cached policy %q, got %q", policyName, policyName2)
	}

	// Set pending orphans — sync should NOT skip even within backoff window
	scController.pendingOrphans[conn.Hostname] = 3
	apiCalled = false
	policyName3, result3 := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass)
	if result3.CheckError != nil {
		t.Fatalf("forced resync failed: %v", result3.CheckError)
	}
	if !apiCalled {
		t.Error("expected sync to run despite backoff window when pendingOrphans > 0")
	}
	if policyName3 != "updated-policy" {
		t.Errorf("expected updated policy from full sync, got %q", policyName3)
	}
}
