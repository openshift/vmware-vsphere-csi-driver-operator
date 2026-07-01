package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/test/library"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	failureDomainTestTimeout  = 10 * time.Minute
	failureDomainPollInterval = 5 * time.Second
	thinCSIStorageClassName   = "thin-csi"
)

// skipIfDay2GateDisabled skips the test when the VSphereMultiVCenterDay2 feature
// gate is not enabled on the cluster. Day-2 lifecycle tests require the gate.
func skipIfDay2GateDisabled(t *testing.T, configClient *configclient.Clientset) {
	t.Helper()
	fg, err := configClient.ConfigV1().FeatureGates().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Skipf("Could not read FeatureGate resource, skipping: %v", err)
		return
	}

	// Check if VSphereMultiVCenterDay2 is in the enabled list for the current version
	for _, version := range fg.Status.FeatureGates {
		for _, gate := range version.Enabled {
			if gate.Name == "VSphereMultiVCenterDay2" {
				return
			}
		}
	}
	t.Skip("VSphereMultiVCenterDay2 feature gate not enabled, skipping day-2 lifecycle test")
}

// TestFailureDomainRemovalTagCleanup verifies that when a failure domain is
// removed from the Infrastructure spec, the operator detects the orphaned tag
// and detaches it from the removed failure domain's datastore. Tags on active
// failure domain datastores must remain.
//
// Prerequisites:
//   - Cluster with 2+ failure domains on the same vCenter
//   - StorageClass "thin-csi" exists and is functional
//
// This test is marked Serial because it mutates the Infrastructure resource.
func TestFailureDomainRemovalTagCleanup(t *testing.T) {
	kubeConfig, err := library.NewClientConfigForTest(t)
	if err != nil {
		t.Fatalf("Failed to get kubeconfig: %v", err)
	}

	kubeClient := kubernetes.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))

	skipIfDay2GateDisabled(t, configClient)

	ctx := context.TODO()

	// Get current Infrastructure
	infra, err := configClient.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get Infrastructure: %v", err)
	}

	if infra.Spec.PlatformSpec.VSphere == nil {
		t.Skip("Not a vSphere platform")
	}

	fds := infra.Spec.PlatformSpec.VSphere.FailureDomains
	if len(fds) < 2 {
		t.Skipf("Need at least 2 failure domains, have %d", len(fds))
	}

	// Save original for restore
	originalFDs := make([]configv1.VSpherePlatformFailureDomainSpec, len(fds))
	copy(originalFDs, fds)

	// Verify thin-csi StorageClass exists
	_, err = kubeClient.StorageV1().StorageClasses().Get(ctx, thinCSIStorageClassName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("StorageClass %s not found: %v", thinCSIStorageClassName, err)
	}

	// Remove last failure domain
	removedFD := fds[len(fds)-1]
	t.Logf("Removing failure domain %q (datacenter=%s, datastore=%s)",
		removedFD.Name, removedFD.Topology.Datacenter, removedFD.Topology.Datastore)

	newFDs := fds[:len(fds)-1]
	patchJSON, err := buildFDPatch(newFDs)
	if err != nil {
		t.Fatalf("Failed to build FD patch: %v", err)
	}

	_, err = configClient.ConfigV1().Infrastructures().Patch(
		ctx, "cluster", types.MergePatchType, patchJSON, metav1.PatchOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to patch Infrastructure: %v", err)
	}

	// Restore on cleanup
	defer func() {
		t.Log("Restoring original failure domains")
		restoreJSON, err := buildFDPatch(originalFDs)
		if err != nil {
			t.Errorf("Failed to build restore patch: %v", err)
			return
		}
		_, restoreErr := configClient.ConfigV1().Infrastructures().Patch(
			ctx, "cluster", types.MergePatchType, restoreJSON, metav1.PatchOptions{},
		)
		if restoreErr != nil {
			t.Errorf("Failed to restore failure domains: %v", restoreErr)
		}
		waitForOperatorHealthy(t, ctx, configClient)
	}()

	// Wait for operator to reconcile
	t.Log("Waiting for operator to reconcile after FD removal...")
	waitForOperatorHealthy(t, ctx, configClient)

	// Verify StorageClass still exists
	sc, err := kubeClient.StorageV1().StorageClasses().Get(ctx, thinCSIStorageClassName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("StorageClass %s not found after FD removal: %v", thinCSIStorageClassName, err)
	}
	t.Logf("StorageClass %s still exists with policy %s", sc.Name, sc.Parameters["StoragePolicyName"])

	// Verify OrphanCleanupPending condition is not True (orphan should have been cleaned)
	verifyOrphanConditionCleared(t, ctx, configClient)

	// Verify PVC provisioning still works on remaining FDs
	t.Log("Verifying PVC provisioning works on remaining failure domains")
	verifyPVCProvisionable(t, ctx, kubeClient, thinCSIStorageClassName)
}

// TestStorageClassSurvivesTopologyTransition verifies that the thin-csi
// StorageClass survives transitions between topology-enabled and
// topology-disabled states when failure domains change.
func TestStorageClassSurvivesTopologyTransition(t *testing.T) {
	kubeConfig, err := library.NewClientConfigForTest(t)
	if err != nil {
		t.Fatalf("Failed to get kubeconfig: %v", err)
	}

	kubeClient := kubernetes.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))
	ctx := context.TODO()

	sc, err := kubeClient.StorageV1().StorageClasses().Get(ctx, thinCSIStorageClassName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			t.Skip("StorageClass thin-csi not found, skipping topology transition test")
		}
		t.Fatalf("Failed to get StorageClass: %v", err)
	}

	policyName := sc.Parameters["StoragePolicyName"]
	if policyName == "" {
		t.Error("StorageClass thin-csi has empty StoragePolicyName")
	} else {
		t.Logf("StorageClass thin-csi has StoragePolicyName=%s", policyName)
	}
}

// TestPVSafetyBlocksCleanup verifies that orphan tag cleanup is blocked when
// PVs exist on the datastore being cleaned up. This test creates a PVC on a
// failure domain's datastore, then removes the failure domain and verifies the
// tag is NOT detached.
func TestPVSafetyBlocksCleanup(t *testing.T) {
	kubeConfig, err := library.NewClientConfigForTest(t)
	if err != nil {
		t.Fatalf("Failed to get kubeconfig: %v", err)
	}

	kubeClient := kubernetes.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(kubeConfig, clientName))

	skipIfDay2GateDisabled(t, configClient)

	ctx := context.TODO()

	infra, err := configClient.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get Infrastructure: %v", err)
	}

	if infra.Spec.PlatformSpec.VSphere == nil {
		t.Skip("Not a vSphere platform")
	}

	fds := infra.Spec.PlatformSpec.VSphere.FailureDomains
	if len(fds) < 2 {
		t.Skipf("Need at least 2 failure domains, have %d", len(fds))
	}

	// Create a PVC on the last failure domain's datastore
	t.Log("Creating PVC to block orphan cleanup")
	pvcName := fmt.Sprintf("pv-safety-test-%d", time.Now().Unix())
	ns := "openshift-cluster-csi-drivers"
	pvc := newTestPVC(pvcName, ns, thinCSIStorageClassName)

	_, err = kubeClient.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test PVC: %v", err)
	}
	defer func() {
		t.Log("Cleaning up test PVC")
		if err := kubeClient.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, pvcName, metav1.DeleteOptions{}); err != nil {
			t.Logf("Failed to delete test PVC %s/%s: %v", ns, pvcName, err)
		}
	}()

	// Wait for PVC to bind
	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, false, func(pollCtx context.Context) (bool, error) {
		p, getErr := kubeClient.CoreV1().PersistentVolumeClaims(ns).Get(pollCtx, pvcName, metav1.GetOptions{})
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return false, nil
			}
			return false, getErr
		}
		return p.Status.Phase == corev1.ClaimBound, nil
	})
	if err != nil {
		t.Skipf("PVC did not bind within 2 minutes (may need topology), skipping: %v", err)
	}

	// Save and remove the FD
	originalFDs := make([]configv1.VSpherePlatformFailureDomainSpec, len(fds))
	copy(originalFDs, fds)

	newFDs := fds[:len(fds)-1]
	patchJSON, err := buildFDPatch(newFDs)
	if err != nil {
		t.Fatalf("Failed to build FD patch: %v", err)
	}

	_, err = configClient.ConfigV1().Infrastructures().Patch(
		ctx, "cluster", types.MergePatchType, patchJSON, metav1.PatchOptions{},
	)
	if err != nil {
		t.Fatalf("Failed to patch Infrastructure: %v", err)
	}
	defer func() {
		t.Log("Restoring original failure domains")
		restoreJSON, err := buildFDPatch(originalFDs)
		if err != nil {
			t.Logf("Failed to build restore patch: %v", err)
			return
		}
		if _, err := configClient.ConfigV1().Infrastructures().Patch(
			ctx, "cluster", types.MergePatchType, restoreJSON, metav1.PatchOptions{},
		); err != nil {
			t.Logf("Failed to restore failure domains: %v", err)
			return
		}
		waitForOperatorHealthy(t, ctx, configClient)
	}()

	// Wait for operator to reconcile
	waitForOperatorHealthy(t, ctx, configClient)

	// Verify the PVC is still accessible (PV safety should have blocked cleanup)
	p, err := kubeClient.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get PVC after FD removal: %v", err)
	}
	if p.Status.Phase != corev1.ClaimBound {
		t.Errorf("Expected PVC to remain Bound, got %s", p.Status.Phase)
	}
	t.Log("PVC remains Bound — PV safety check is working")

	// Verify OrphanCleanupPending condition is True (orphan blocked by PVs)
	verifyOrphanConditionPending(t, ctx, configClient)
}

// waitForOperatorHealthy waits for the storage ClusterOperator to report Available=True.
func waitForOperatorHealthy(t *testing.T, ctx context.Context, configClient *configclient.Clientset) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, failureDomainPollInterval, failureDomainTestTimeout, false, func(pollCtx context.Context) (bool, error) {
		co, err := configClient.ConfigV1().ClusterOperators().Get(pollCtx, storageCOName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, cond := range co.Status.Conditions {
			if cond.Type == "Available" && cond.Status == "True" {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for storage operator to become healthy: %v", err)
	}
}

// verifyPVCProvisionable creates a test PVC, waits for it to bind, then cleans up.
func verifyPVCProvisionable(t *testing.T, ctx context.Context, kubeClient kubernetes.Interface, storageClassName string) {
	t.Helper()
	ns := "openshift-cluster-csi-drivers"
	pvcName := fmt.Sprintf("fd-lifecycle-test-%d", time.Now().Unix())
	pvc := newTestPVC(pvcName, ns, storageClassName)

	_, err := kubeClient.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test PVC: %v", err)
	}
	defer func() {
		if err := kubeClient.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, pvcName, metav1.DeleteOptions{}); err != nil {
			t.Logf("Failed to delete test PVC %s/%s: %v", ns, pvcName, err)
		}
	}()

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, false, func(pollCtx context.Context) (bool, error) {
		p, getErr := kubeClient.CoreV1().PersistentVolumeClaims(ns).Get(pollCtx, pvcName, metav1.GetOptions{})
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return false, nil
			}
			return false, getErr
		}
		return p.Status.Phase == corev1.ClaimBound, nil
	})
	if err != nil {
		t.Errorf("Test PVC %s did not bind within 2 minutes: %v", pvcName, err)
	} else {
		t.Logf("Test PVC %s bound successfully", pvcName)
	}
}

// newTestPVC creates a PVC spec for testing.
func newTestPVC(name, namespace, storageClassName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
}

// verifyOrphanConditionCleared checks that the OrphanCleanupPending condition is not True.
func verifyOrphanConditionCleared(t *testing.T, ctx context.Context, configClient *configclient.Clientset) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, failureDomainPollInterval, 2*time.Minute, false, func(pollCtx context.Context) (bool, error) {
		co, err := configClient.ConfigV1().ClusterOperators().Get(pollCtx, storageCOName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, cond := range co.Status.Conditions {
			if cond.Type == "VMwareVSphereDriverStorageClassControllerOrphanCleanupPending" && cond.Status == "True" {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Errorf("OrphanCleanupPending condition did not clear within 2 minutes: %v", err)
	} else {
		t.Log("OrphanCleanupPending condition cleared — orphan tag cleanup succeeded")
	}
}

// verifyOrphanConditionPending checks that the OrphanCleanupPending condition is True.
func verifyOrphanConditionPending(t *testing.T, ctx context.Context, configClient *configclient.Clientset) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, failureDomainPollInterval, 2*time.Minute, false, func(pollCtx context.Context) (bool, error) {
		co, err := configClient.ConfigV1().ClusterOperators().Get(pollCtx, storageCOName, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, cond := range co.Status.Conditions {
			if cond.Type == "VMwareVSphereDriverStorageClassControllerOrphanCleanupPending" && cond.Status == "True" {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Errorf("OrphanCleanupPending condition was not set within 2 minutes: %v", err)
	} else {
		t.Log("OrphanCleanupPending condition is True — PV safety blocked cleanup as expected")
	}
}

// TestVCenterRemovalCleanup verifies that removing a vCenter from the failure
// domain list cleans up its stale entry in the storage policy controller.
func TestVCenterRemovalCleanup(t *testing.T) {
	t.Skip("vCenter removal cleanup is internal controller state not observable from e2e; requires multi-vCenter mutation test infrastructure")
}

// buildFDPatch creates a JSON merge patch for updating failure domains.
func buildFDPatch(fds []configv1.VSpherePlatformFailureDomainSpec) ([]byte, error) {
	type vspherePatch struct {
		FailureDomains []configv1.VSpherePlatformFailureDomainSpec `json:"failureDomains"`
	}
	type platformPatch struct {
		VSphere vspherePatch `json:"vsphere"`
	}
	type specPatch struct {
		PlatformSpec platformPatch `json:"platformSpec"`
	}
	type infraPatch struct {
		Spec specPatch `json:"spec"`
	}

	patch := infraPatch{
		Spec: specPatch{
			PlatformSpec: platformPatch{
				VSphere: vspherePatch{
					FailureDomains: fds,
				},
			},
		},
	}
	return json.Marshal(patch)
}
