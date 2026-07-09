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
			err := scController.Sync(context.TODO(), conns, nil, apiDeps)
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

func TestUpdateConditionsUsesGenericOrphanCleanupMessage(t *testing.T) {
	commonApiClient := testlib.NewFakeClients(
		[]runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
		testlib.MakeFakeDriverInstance(),
		testlib.GetInfraObject(),
	)
	scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)

	err := scController.updateConditions(
		context.TODO(),
		checks.MakeClusterCheckResultPass(),
		checks.ClusterCheckAllGood,
		2,
	)
	if err != nil {
		t.Fatalf("failed to update conditions: %+v", err)
	}

	_, status, _, err := scController.operatorClient.GetOperatorState()
	if err != nil {
		t.Fatalf("failed to get operator state: %+v", err)
	}

	condition := testlib.GetMatchingCondition(status.Conditions, testScControllerName+"OrphanCleanupPending")
	if condition == nil {
		t.Fatal("expected orphan cleanup pending condition to be present")
	}
	if condition.Status != opv1.ConditionTrue {
		t.Fatalf("expected orphan cleanup pending condition true, got %v", condition.Status)
	}
	expectedMessage := "2 orphaned datastore tag(s) could not be cleaned up"
	if condition.Message != expectedMessage {
		t.Fatalf("expected message %q, got %q", expectedMessage, condition.Message)
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
			policyName, clusterCheckResult := scController.syncStoragePolicy(context.TODO(), &conn, apiDeps, opv1.ManagedStorageClass, false)
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

			policyName, clusterCheckResult = scController.syncStoragePolicy(context.TODO(), &conn, apiDeps, opv1.ManagedStorageClass, false)
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
				policyName, clusterCheckResult := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass, false)
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
		scController.syncStoragePolicy(context.TODO(), connA, apiDeps, opv1.ManagedStorageClass, false)
	}

	// vcenter-b succeeds on first try
	scController.makeStoragePolicyAPI = newFakeStoragePolicyAPISuccess
	bsB := scController.getBackoffState(connB.Hostname)
	bsB.nextCheck = time.Time{}
	scController.vCenterStoragePolicy[connB.Hostname] = ""
	beforeB := time.Now()
	scController.syncStoragePolicy(context.TODO(), connB, apiDeps, opv1.ManagedStorageClass, false)

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

// TestCleanupConnectionsNeverDegradeCluster verifies that a failing cleanup connection (best-effort
// reconnect to a removed vCenter) never flips overallClusterStatus/checkResult to degraded, even
// though the same failure on a normal `connections` entry would.
func TestCleanupConnectionsNeverDegradeCluster(t *testing.T) {
	initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
	clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
	configObjects := runtime.Object(testlib.GetInfraObject())
	commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
	apiDeps := getCheckAPIDependency(commonApiClient)

	scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)
	scController.makeStoragePolicyAPI = newFakeStoragePolicyAPISuccess

	healthyConn := &vclib.VSphereConnection{Hostname: "healthy-vcenter"}
	failingCleanupConn := &vclib.VSphereConnection{Hostname: "removed-vcenter"}

	callCount := 0
	scController.makeStoragePolicyAPI = func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
		callCount++
		if connection.Hostname == failingCleanupConn.Hostname {
			return &fakeStoragePolicyAPI{err: fmt.Errorf("cleanup connection failed")}
		}
		return &fakeStoragePolicyAPI{ret: "fake-policy"}
	}

	err := scController.Sync(context.TODO(), []*vclib.VSphereConnection{healthyConn}, []*vclib.VSphereConnection{failingCleanupConn}, apiDeps)
	if err != nil {
		t.Fatalf("Sync should never return an error just because a cleanup connection failed: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected both the healthy connection and the cleanup connection to be synced, got %d calls", callCount)
	}

	_, status, _, err := scController.operatorClient.GetOperatorState()
	if err != nil {
		t.Fatalf("failed to get operator state: %v", err)
	}
	degraded := testlib.GetMatchingCondition(status.Conditions, testScControllerName+opv1.OperatorStatusTypeDegraded)
	if degraded == nil || degraded.Status != opv1.ConditionFalse {
		t.Errorf("expected cluster to not be degraded by a failing cleanup connection, got: %+v", degraded)
	}
}

// TestIsHostFullyClean verifies the semantics IsHostFullyClean must have for VSphereController's
// Phase 2 retry bookkeeping to work: true only once the host has no tracked policy AND no
// pending (PV-blocked) orphans.
func TestIsHostFullyClean(t *testing.T) {
	tests := []struct {
		name         string
		policyName   string
		policyExists bool
		pending      int
		expected     bool
	}{
		{name: "never synced", expected: true},
		{name: "policy still tracked", policyExists: true, policyName: "some-policy", expected: false},
		{name: "empty policy name, no pending orphans", policyExists: true, policyName: "", expected: true},
		{name: "empty policy name but pending orphans", policyExists: true, policyName: "", pending: 2, expected: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &StorageClassController{
				vCenterStoragePolicy: make(map[string]string),
				pendingOrphans:       make(map[string]int),
			}
			if test.policyExists {
				c.vCenterStoragePolicy["host"] = test.policyName
			}
			if test.pending > 0 {
				c.pendingOrphans["host"] = test.pending
			}
			if got := c.IsHostFullyClean("host"); got != test.expected {
				t.Errorf("expected %v, got %v", test.expected, got)
			}
		})
	}
}

// TestPurgeVCenterState verifies PurgeVCenterState drops every map entry tracked for a host.
func TestPurgeVCenterState(t *testing.T) {
	c := &StorageClassController{
		vCenterStoragePolicy: map[string]string{"host": "policy"},
		backoffStates:        map[string]*vCenterBackoffState{"host": {}},
		pendingOrphans:       map[string]int{"host": 3},
	}
	c.PurgeVCenterState("host")
	if _, ok := c.vCenterStoragePolicy["host"]; ok {
		t.Error("expected vCenterStoragePolicy entry to be purged")
	}
	if _, ok := c.backoffStates["host"]; ok {
		t.Error("expected backoffStates entry to be purged")
	}
	if _, ok := c.pendingOrphans["host"]; ok {
		t.Error("expected pendingOrphans entry to be purged")
	}
}

// TestActiveHostNeverPurgedMidSync guards against accidentally purging a host that is still
// present in `connections` - Sync() must never call PurgeVCenterState itself; only
// VSphereController does so, and only once cleanup is confirmed complete or abandoned.
func TestActiveHostNeverPurgedMidSync(t *testing.T) {
	initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
	clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
	configObjects := runtime.Object(testlib.GetInfraObject())
	commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
	apiDeps := getCheckAPIDependency(commonApiClient)

	scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)
	scController.makeStoragePolicyAPI = newFakeStoragePolicyAPISuccess

	conn := &vclib.VSphereConnection{Hostname: "vcenter-a"}
	if err := scController.Sync(context.TODO(), []*vclib.VSphereConnection{conn}, nil, apiDeps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := scController.vCenterStoragePolicy[conn.Hostname]; !ok {
		t.Fatalf("expected vcenter-a's policy to be tracked after a successful sync")
	}

	// A second Sync() call where vcenter-a happens to be temporarily absent from `connections`
	// (e.g. a transient vSphereConnections build failure elsewhere) must not purge its state -
	// only VSphereController decides that, via PurgeVCenterState, once it has confirmed cleanup
	// is complete or abandoned.
	if err := scController.Sync(context.TODO(), nil, nil, apiDeps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := scController.vCenterStoragePolicy[conn.Hostname]; !ok {
		t.Errorf("expected vcenter-a's policy to survive a sync where it was merely absent from connections")
	}
}

// TestCleanupConnectionRefreshesTrackedPolicyState is a regression test for a bug where
// syncStoragePolicy only recorded the returned policy name in c.vCenterStoragePolicy for the
// `connections` loop of Sync(), never for `cleanupConnections` (whose returned name is
// discarded). That left a removed vCenter's stale, pre-removal (non-empty) policy name in the
// map forever, so IsHostFullyClean could never report it clean even after the profile was
// deleted - starving Phase 2's retry loop of a way to detect completion.
func TestCleanupConnectionRefreshesTrackedPolicyState(t *testing.T) {
	initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
	clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
	configObjects := runtime.Object(testlib.GetInfraObject())
	commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
	apiDeps := getCheckAPIDependency(commonApiClient)

	scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)
	cleanupConn := &vclib.VSphereConnection{Hostname: "removed-vcenter"}

	// Simulate this host having been an active connection before removal: it still has a
	// non-empty policy tracked, as if the profile has not been deleted yet.
	scController.vCenterStoragePolicy[cleanupConn.Hostname] = "openshift-storage-policy-vsphere"
	scController.makeStoragePolicyAPI = func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
		return &fakeStoragePolicyAPI{ret: "openshift-storage-policy-vsphere"}
	}
	if err := scController.Sync(context.TODO(), nil, []*vclib.VSphereConnection{cleanupConn}, apiDeps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scController.IsHostFullyClean(cleanupConn.Hostname) {
		t.Fatalf("expected host to NOT be fully clean while the profile still exists")
	}

	// Second sync: the zero-FD branch deletes the profile and returns "".
	bs := scController.getBackoffState(cleanupConn.Hostname)
	bs.nextCheck = time.Time{}
	scController.makeStoragePolicyAPI = func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
		return &fakeStoragePolicyAPI{ret: ""}
	}
	if err := scController.Sync(context.TODO(), nil, []*vclib.VSphereConnection{cleanupConn}, apiDeps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scController.IsHostFullyClean(cleanupConn.Hostname) {
		t.Errorf("expected host to be fully clean once detection succeeds and the profile is deleted")
	}
}

// TestCleanupConnectionBypassesBackoffThrottle is a regression test for a bug where a
// vCenter's normal (pre-removal) successful sync sets its backoff state's nextCheck up to
// successCheckInterval (10m) into the future. Once that same hostname later shows up as a
// cleanupConnection (the vCenter was removed), syncStoragePolicy's within-backoff-window skip
// check ran for cleanup connections too, silently no-op'ing the cleanup sync - never calling
// createStoragePolicy, never detecting/deleting the orphaned tag or profile - until the stale
// 10-minute window from the vCenter's last *active* sync happened to elapse. Cleanup
// connections are rare, bounded (cleanupApiTimeout) and best-effort, so they must always run
// the real check regardless of the throttle set while the vCenter was still active.
func TestCleanupConnectionBypassesBackoffThrottle(t *testing.T) {
	initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
	clusterCSIDriverObject := testlib.MakeFakeDriverInstance()
	configObjects := runtime.Object(testlib.GetInfraObject())
	commonApiClient := testlib.NewFakeClients(initialObjects, clusterCSIDriverObject, configObjects)
	apiDeps := getCheckAPIDependency(commonApiClient)

	scController := newStorageClassController(commonApiClient, "storageclass1.yaml", false)
	cleanupConn := &vclib.VSphereConnection{Hostname: "removed-vcenter"}

	// Simulate this host having been an active connection very recently: its backoff state's
	// nextCheck is far in the future, exactly as it would be right after a successful sync.
	scController.vCenterStoragePolicy[cleanupConn.Hostname] = "openshift-storage-policy-vsphere"
	bs := scController.getBackoffState(cleanupConn.Hostname)
	bs.nextCheck = time.Now().Add(successCheckInterval)

	apiCalled := false
	scController.makeStoragePolicyAPI = func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
		apiCalled = true
		return &fakeStoragePolicyAPI{ret: ""}
	}
	if err := scController.Sync(context.TODO(), nil, []*vclib.VSphereConnection{cleanupConn}, apiDeps); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !apiCalled {
		t.Error("expected cleanup connection sync to bypass the stale backoff window and call createStoragePolicy")
	}
	if !scController.IsHostFullyClean(cleanupConn.Hostname) {
		t.Errorf("expected host to be reported fully clean once the cleanup sync actually ran")
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
	policyName, result := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass, false)
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
	policyName2, result2 := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass, false)
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
	policyName3, result3 := scController.syncStoragePolicy(context.TODO(), conn, apiDeps, opv1.ManagedStorageClass, false)
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
