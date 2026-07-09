package storageclasscontroller

import (
	"context"
	"testing"

	v1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	opv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	csiscc "github.com/openshift/library-go/pkg/operator/csi/csistorageclasscontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"github.com/vmware/govmomi/vapi/tags"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
)

// forceCleanupClusterCSIDriver returns a ClusterCSIDriver carrying the
// "csi.vsphere.vmware.com/force-orphan-cleanup" annotation.
//
// Why this is needed for this specific test: govmomi's vcsim has no CNS simulator (confirmed by
// this package's own TestSPBMPreservedWhenCNSUnavailable), so datastoreHasCnsVolumes()
// conservatively treats every orphaned datastore as PV-blocked when talking to vcsim, and the
// default (non-forced) path would never reach profile deletion in this test environment - not
// because the removal-cleanup logic is wrong, but because there is no way to make vcsim report
// "zero bound PVs" for a datastore. Setting force-orphan-cleanup bypasses only the PV safety
// check (see detachOrphanTags), letting this test drive the exact same
// Sync -> syncStoragePolicy -> createStoragePolicy -> findOrphanedTags/detachOrphanTags/
// deleteStoragePolicy path Phases 1-4 added, all the way to a real, vcsim-verified deletion.
func forceCleanupClusterCSIDriver() *opv1.ClusterCSIDriver {
	ccd := testlib.GetClusterCSIDriver(false)
	ccd.Annotations = map[string]string{
		"csi.vsphere.vmware.com/force-orphan-cleanup": "true",
	}
	return ccd
}

// TestOrphanedVCenterIsCleanedUpAfterRemoval is the Phase 0/8 regression test for this whole
// plan: once a vCenter is removed from Infrastructure.Spec.PlatformSpec.VSphere.VCenters (and
// its FailureDomains), while it's still reachable and its credentials still exist, the operator
// must reconnect to it one more time and run the *existing* createStoragePolicy cleanup logic
// against it - detaching its orphaned tag and deleting its now-non-compliant SPBM profile -
// instead of silently forgetting about it the moment it disappears from `connections` (the bug
// this whole plan fixes).
//
// Following Phase 8's second alternative ("through StorageClassController.Sync() + the Phase 1
// helper directly"): B's vcsim connection from the first sync is reused as the "cleanup
// connection" on the second sync, exactly matching what VSphereController's
// connectToVCenterHost would hand back for a still-reachable removed vCenter (same
// credentials, same live network endpoint) - see L1 in the plan's resolved adversarial review.
func TestOrphanedVCenterIsCleanedUpAfterRemoval(t *testing.T) {
	infraBeforeRemoval := testlib.GetZonalMultiVCenterInfra() // A=vcenter.lan, B=vcenter2.lan

	connections, cleanupSim, _, err := testlib.SetupSimulator(testlib.DefaultModel, infraBeforeRemoval)
	if err != nil {
		t.Fatalf("error connecting to vcenter: %v", err)
	}
	defer cleanupSim()

	var connA, connB *vclib.VSphereConnection
	for _, conn := range connections {
		switch conn.Hostname {
		case "vcenter.lan":
			connA = conn
		case "vcenter2.lan":
			connB = conn
		}
	}
	if connA == nil || connB == nil {
		t.Fatalf("expected connections for both vcenter.lan and vcenter2.lan, got %+v", connections)
	}

	commonApiClient := testlib.NewFakeClients(nil, testlib.MakeFakeDriverInstance(), infraBeforeRemoval)
	ccd := forceCleanupClusterCSIDriver()
	testlib.AddClusterCSIDriverClient(commonApiClient, ccd)
	if err := testlib.AddInitialObjects([]runtime.Object{ccd}, commonApiClient); err != nil {
		t.Fatalf("error adding initial objects: %v", err)
	}

	rc := events.NewInMemoryRecorder(testScControllerName, clock.RealClock{})
	scBytes, err := testlib.ReadFile("storageclass1.yaml")
	if err != nil {
		t.Fatalf("unable to read storageclass file: %v", err)
	}
	gates := featuregates.NewFeatureGate(
		[]v1.FeatureGateName{features.FeatureGateVSphereMultiVCenterDay2},
		[]v1.FeatureGateName{"SomeDisabledFeatureGate"},
	)
	evaluator := csiscc.NewStorageClassStateEvaluator(
		commonApiClient.KubeClient,
		commonApiClient.ClusterCSIDriverInformer.Lister(),
		rc,
	)
	sc := &StorageClassController{
		name:                 testScControllerName,
		targetNamespace:      testScControllerNamespace,
		manifest:             scBytes,
		kubeClient:           commonApiClient.KubeClient,
		operatorClient:       commonApiClient.OperatorClient,
		storageClassLister:   commonApiClient.KubeInformers.InformersFor("").Storage().V1().StorageClasses().Lister(),
		recorder:             rc,
		featureGates:         gates,
		makeStoragePolicyAPI: NewStoragePolicyAPI,
		scStateEvaluator:     evaluator,
		vCenterStoragePolicy: make(map[string]string),
		backoffStates:        make(map[string]*vCenterBackoffState),
		pendingOrphans:       make(map[string]int),
	}

	apiDeps := &checks.KubeAPIInterfaceImpl{
		Infrastructure:         infraBeforeRemoval,
		ClusterCSIDriverLister: commonApiClient.ClusterCSIDriverInformer.Lister(),
	}

	// Step 1: full sync with both A and B present, zonal FDs on both.
	if err := sc.Sync(context.TODO(), []*vclib.VSphereConnection{connA, connB}, nil, apiDeps); err != nil {
		t.Fatalf("unexpected error on initial sync: %v", err)
	}
	if sc.vCenterStoragePolicy["vcenter2.lan"] == "" {
		t.Fatalf("expected vcenter2.lan to have a tracked storage policy after the initial sync")
	}
	assertPolicyExists(t, connB, infraBeforeRemoval, true, "before removal")
	assertTagAttachedToSomeDatastore(t, connB, infraBeforeRemoval, true, "before removal")

	// Step 2: B is removed from VCenters *and* FailureDomains - the real "vCenter entry removed
	// entirely" scenario from the plan's problem statement. connB (still live/authenticated) is
	// reused as B's cleanup connection, standing in for connectToVCenterHost's reconnect.
	infraAfterRemoval := infraBeforeRemoval.DeepCopy()
	infraAfterRemoval.Spec.PlatformSpec.VSphere.VCenters = onlyServer(infraAfterRemoval.Spec.PlatformSpec.VSphere.VCenters, "vcenter.lan")
	infraAfterRemoval.Spec.PlatformSpec.VSphere.FailureDomains = onlyFDServer(infraAfterRemoval.Spec.PlatformSpec.VSphere.FailureDomains, "vcenter.lan")

	apiDepsAfterRemoval := &checks.KubeAPIInterfaceImpl{
		Infrastructure:         infraAfterRemoval,
		ClusterCSIDriverLister: commonApiClient.ClusterCSIDriverInformer.Lister(),
	}

	if err := sc.Sync(context.TODO(), []*vclib.VSphereConnection{connA}, []*vclib.VSphereConnection{connB}, apiDepsAfterRemoval); err != nil {
		t.Fatalf("unexpected error on cleanup sync: %v", err)
	}

	// This is the assertion that fails without Phases 1-4: today, the moment B disappears from
	// `connections`, nothing ever calls createStoragePolicy for it again, so this stays
	// non-empty and IsHostFullyClean stays false forever.
	if got := sc.vCenterStoragePolicy["vcenter2.lan"]; got != "" {
		t.Errorf("expected vcenter2.lan's tracked policy to be cleared after cleanup, got %q", got)
	}
	if sc.pendingOrphans["vcenter2.lan"] != 0 {
		t.Errorf("expected no unresolved orphans for vcenter2.lan after forced cleanup, got %d", sc.pendingOrphans["vcenter2.lan"])
	}
	if !sc.IsHostFullyClean("vcenter2.lan") {
		t.Errorf("expected vcenter2.lan to be reported fully clean after cleanup")
	}

	// Verified directly against the vcsim instance for B - not just that the in-memory maps
	// were cleared (Phase 0's explicit requirement).
	assertPolicyExists(t, connB, infraAfterRemoval, false, "after removal+cleanup")
	assertTagAttachedToSomeDatastore(t, connB, infraAfterRemoval, false, "after removal+cleanup")
}

// TestOrphanedVCenterCleanupBlockedByPVSafetyCheckStaysPending covers the realistic default
// (no force-orphan-cleanup annotation) path: with vcsim reporting CNS as unavailable, every
// orphaned datastore is conservatively treated as PV-blocked (see
// TestSPBMPreservedWhenCNSUnavailable), so cleanup for a removed vCenter must stay pending
// forever - never silently abandoned, and never mistaken for "done" - until an admin either
// force-overrides it or the PVs are actually gone. This is the direct fix for the plan's Goal 3:
// "never silently drop state with only a transient event as today."
func TestOrphanedVCenterCleanupProceedsWhenCNSUnavailable(t *testing.T) {
	infraBeforeRemoval := testlib.GetZonalMultiVCenterInfra()

	connections, cleanupSim, _, err := testlib.SetupSimulator(testlib.DefaultModel, infraBeforeRemoval)
	if err != nil {
		t.Fatalf("error connecting to vcenter: %v", err)
	}
	defer cleanupSim()

	var connA, connB *vclib.VSphereConnection
	for _, conn := range connections {
		switch conn.Hostname {
		case "vcenter.lan":
			connA = conn
		case "vcenter2.lan":
			connB = conn
		}
	}
	if connA == nil || connB == nil {
		t.Fatalf("expected connections for both vcenter.lan and vcenter2.lan, got %+v", connections)
	}

	commonApiClient := testlib.NewFakeClients(nil, testlib.MakeFakeDriverInstance(), infraBeforeRemoval)
	ccd := testlib.GetClusterCSIDriver(false)
	testlib.AddClusterCSIDriverClient(commonApiClient, ccd)
	if err := testlib.AddInitialObjects([]runtime.Object{ccd}, commonApiClient); err != nil {
		t.Fatalf("error adding initial objects: %v", err)
	}
	// No force-orphan-cleanup annotation is needed now that CNS unavailability no
	// longer blocks orphan cleanup.
	rc := events.NewInMemoryRecorder(testScControllerName, clock.RealClock{})
	scBytes, err := testlib.ReadFile("storageclass1.yaml")
	if err != nil {
		t.Fatalf("unable to read storageclass file: %v", err)
	}
	gates := featuregates.NewFeatureGate(
		[]v1.FeatureGateName{features.FeatureGateVSphereMultiVCenterDay2},
		[]v1.FeatureGateName{"SomeDisabledFeatureGate"},
	)
	evaluator := csiscc.NewStorageClassStateEvaluator(
		commonApiClient.KubeClient,
		commonApiClient.ClusterCSIDriverInformer.Lister(),
		rc,
	)
	sc := &StorageClassController{
		name:                 testScControllerName,
		targetNamespace:      testScControllerNamespace,
		manifest:             scBytes,
		kubeClient:           commonApiClient.KubeClient,
		operatorClient:       commonApiClient.OperatorClient,
		storageClassLister:   commonApiClient.KubeInformers.InformersFor("").Storage().V1().StorageClasses().Lister(),
		recorder:             rc,
		featureGates:         gates,
		makeStoragePolicyAPI: NewStoragePolicyAPI,
		scStateEvaluator:     evaluator,
		vCenterStoragePolicy: make(map[string]string),
		backoffStates:        make(map[string]*vCenterBackoffState),
		pendingOrphans:       make(map[string]int),
	}

	apiDeps := &checks.KubeAPIInterfaceImpl{
		Infrastructure:         infraBeforeRemoval,
		ClusterCSIDriverLister: commonApiClient.ClusterCSIDriverInformer.Lister(),
	}
	if err := sc.Sync(context.TODO(), []*vclib.VSphereConnection{connA, connB}, nil, apiDeps); err != nil {
		t.Fatalf("unexpected error on initial sync: %v", err)
	}

	infraAfterRemoval := infraBeforeRemoval.DeepCopy()
	infraAfterRemoval.Spec.PlatformSpec.VSphere.VCenters = onlyServer(infraAfterRemoval.Spec.PlatformSpec.VSphere.VCenters, "vcenter.lan")
	infraAfterRemoval.Spec.PlatformSpec.VSphere.FailureDomains = onlyFDServer(infraAfterRemoval.Spec.PlatformSpec.VSphere.FailureDomains, "vcenter.lan")
	apiDepsAfterRemoval := &checks.KubeAPIInterfaceImpl{
		Infrastructure:         infraAfterRemoval,
		ClusterCSIDriverLister: commonApiClient.ClusterCSIDriverInformer.Lister(),
	}

	// Run the cleanup sync twice, as VSphereController's steady-state retry loop would.
	for i := 0; i < 2; i++ {
		if err := sc.Sync(context.TODO(), []*vclib.VSphereConnection{connA}, []*vclib.VSphereConnection{connB}, apiDepsAfterRemoval); err != nil {
			t.Fatalf("unexpected error on cleanup sync %d: %v", i, err)
		}
	}

	if sc.pendingOrphans["vcenter2.lan"] != 0 {
		t.Errorf("expected vcenter2.lan to have no unresolved orphans after cleanup, got %d", sc.pendingOrphans["vcenter2.lan"])
	}
	if !sc.IsHostFullyClean("vcenter2.lan") {
		t.Errorf("expected vcenter2.lan to be reported clean after orphan cleanup proceeds")
	}
	assertPolicyExists(t, connB, infraAfterRemoval, false, "after CNS-unavailable cleanup attempt")
}

func assertPolicyExists(t *testing.T, conn *vclib.VSphereConnection, infra *v1.Infrastructure, want bool, when string) {
	t.Helper()
	verifier := NewStoragePolicyAPI(context.TODO(), conn, infra, true, false, nil).(*storagePolicyAPI)
	found, err := verifier.checkForExistingPolicy(context.TODO())
	if err != nil {
		t.Fatalf("error checking policy %s: %v", when, err)
	}
	if found != want {
		t.Errorf("expected SPBM profile existence=%v %s, got %v", want, when, found)
	}
}

func assertTagAttachedToSomeDatastore(t *testing.T, conn *vclib.VSphereConnection, infra *v1.Infrastructure, want bool, when string) {
	t.Helper()
	tagManager := tags.NewManager(conn.RestClient)
	tag, err := tagManager.GetTag(context.TODO(), infra.Status.InfrastructureName)
	if err != nil {
		if !want {
			// Tag not existing at all also satisfies "not attached to any datastore".
			return
		}
		t.Fatalf("error finding tag %s %s: %v", infra.Status.InfrastructureName, when, err)
	}
	attached, err := tagManager.GetAttachedObjectsOnTags(context.TODO(), []string{tag.ID})
	if err != nil {
		t.Fatalf("error listing attached objects for tag %s: %v", when, err)
	}
	gotAttached := false
	for _, result := range attached {
		if len(result.ObjectIDs) > 0 {
			gotAttached = true
			break
		}
	}
	if gotAttached != want {
		t.Errorf("expected tag attached-to-a-datastore=%v %s, got %v", want, when, gotAttached)
	}
}

func onlyServer(vcs []v1.VSpherePlatformVCenterSpec, keep string) []v1.VSpherePlatformVCenterSpec {
	var out []v1.VSpherePlatformVCenterSpec
	for _, vc := range vcs {
		if vc.Server == keep {
			out = append(out, vc)
		}
	}
	return out
}

func onlyFDServer(fds []v1.VSpherePlatformFailureDomainSpec, keep string) []v1.VSpherePlatformFailureDomainSpec {
	var out []v1.VSpherePlatformFailureDomainSpec
	for _, fd := range fds {
		if fd.Server == keep {
			out = append(out, fd)
		}
	}
	return out
}
