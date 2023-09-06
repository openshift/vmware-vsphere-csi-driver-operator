package checks

import (
	"fmt"
	"strings"

	v1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

// CheckStatusType stores exact error that was observed during performing various checks
type CheckStatusType string

const (
	CheckStatusPass                    CheckStatusType = "pass"
	CheckStatusVSphereConnectionFailed CheckStatusType = "vsphere_connection_failed"
	CheckStatusOpenshiftAPIError       CheckStatusType = "openshift_api_error"
	CheckStatusExistingDriverFound     CheckStatusType = "existing_driver_found"
	CheckStatusDeprecatedVCenter       CheckStatusType = "check_deprecated_vcenter"
	CheckStatusDeprecatedHWVersion     CheckStatusType = "check_deprecated_hw_version"
	CheckStatusDeprecatedESXIVersion   CheckStatusType = "check_deprecated_esxi_version"
	CheckStatusBuggyMigrationPlatform  CheckStatusType = "buggy_vsphere_migration_version"
	CheckStatusVcenterAPIError         CheckStatusType = "vcenter_api_error"
	CheckStatusGenericError            CheckStatusType = "generic_error"
	CheckStatusCSIMigrationDisabled    CheckStatusType = "csi_migration_disabled"
)

// CheckAction stores what a single failing check would do.
type CheckAction int

// Ordered by severity, Pass must be 0 (for struct initialization).
const (
	CheckActionPass                      = iota
	CheckActionRequiresAdminAck          // blocks upgrades via admin-ack
	CheckActionBlockUpgrade              // Only block upgrade
	CheckActionBlockUpgradeDriverInstall // Block both upgrade and driver install
	CheckActionBlockUpgradeOrDegrade     // Degrade if the driver is installed, block upgrade otherwise
	CheckActionDegrade
)

func ActionToString(a CheckAction) string {
	switch a {
	case CheckActionPass:
		return "Pass"
	case CheckActionBlockUpgradeDriverInstall:
		return "BlockUpgradeDriverInstall"
	case CheckActionBlockUpgrade:
		return "BlockUpgrade"
	case CheckActionBlockUpgradeOrDegrade:
		return "BlockUpgradeOrDegrade"
	case CheckActionDegrade:
		return "Degrade"
	case CheckActionRequiresAdminAck:
		return "UpgradeRequiresAdminAck"
	default:
		return "Unknown"
	}
}

// ClusterCheckStatus stores what is the status of overall cluster after checking everything and then applying
// additional logic which may not be included in checks themselves.
type ClusterCheckStatus string

const (
	ClusterCheckAllGood                    ClusterCheckStatus = "pass"
	ClusterCheckBlockUpgradeDriverInstall  ClusterCheckStatus = "installation_blocked"
	ClusterCheckBlockUpgrade               ClusterCheckStatus = "upgrades_blocked"
	ClusterCheckUpgradeStateUnknown        ClusterCheckStatus = "upgrades_unknown"
	ClusterCheckUpgradesBlockedViaAdminAck ClusterCheckStatus = "upgrades_blocked_via_admin_ack"
	ClusterCheckDegrade                    ClusterCheckStatus = "degraded"
)

const (
	inTreePluginName = "kubernetes.io/vsphere-volume"
)

type ClusterCheckResult struct {
	CheckError  error
	CheckStatus CheckStatusType
	Action      CheckAction
	// if we are blocking cluster upgrades or degrading the cluster
	// this message contains information about why we are degrading the cluster
	// or blocking upgrades.
	Reason string
}

func MakeClusterCheckResultPass() ClusterCheckResult {
	return ClusterCheckResult{
		CheckError:  nil,
		CheckStatus: CheckStatusPass,
		Action:      CheckActionPass,
	}
}

func makeFoundExistingDriverResult(reason error) ClusterCheckResult {
	checkResult := ClusterCheckResult{
		CheckStatus: CheckStatusExistingDriverFound,
		CheckError:  reason,
		Action:      CheckActionBlockUpgradeDriverInstall,
		Reason:      reason.Error(),
	}
	return checkResult
}

func makeDeprecatedEnvironmentError(statusType CheckStatusType, reason error) ClusterCheckResult {
	checkResult := ClusterCheckResult{
		CheckStatus: statusType,
		CheckError:  reason,
		Action:      CheckActionBlockUpgradeOrDegrade,
		Reason:      reason.Error(),
	}
	return checkResult
}

func makeBuggyEnvironmentError(statusType CheckStatusType, reason error) ClusterCheckResult {
	checkResult := ClusterCheckResult{
		CheckStatus: statusType,
		CheckError:  reason,
		Action:      CheckActionRequiresAdminAck,
		Reason:      reason.Error(),
	}
	return checkResult
}

func MakeGenericVCenterAPIError(reason error) ClusterCheckResult {
	return ClusterCheckResult{
		CheckStatus: CheckStatusVcenterAPIError,
		CheckError:  reason,
		Action:      CheckActionBlockUpgradeOrDegrade,
		Reason:      reason.Error(),
	}
}

func MakeClusterDegradedError(checkStatus CheckStatusType, reason error) ClusterCheckResult {
	return ClusterCheckResult{
		CheckStatus: checkStatus,
		CheckError:  reason,
		Action:      CheckActionDegrade,
		Reason:      reason.Error(),
	}
}

func MakeClusterUnupgradeableError(checkStatus CheckStatusType, reason error) ClusterCheckResult {
	return ClusterCheckResult{
		CheckStatus: checkStatus,
		CheckError:  reason,
		Action:      CheckActionBlockUpgrade,
		Reason:      reason.Error(),
	}
}

// CheckClusterStatus uses results from all the checks we ran and applies additional logic to determine
// overall CheckClusterStatus
func CheckClusterStatus(result ClusterCheckResult, apiDependencies KubeAPIInterface) (ClusterCheckStatus, ClusterCheckResult) {
	switch result.Action {
	case CheckActionDegrade:
		return ClusterCheckDegrade, result

	case CheckActionBlockUpgradeOrDegrade:
		// a failed check that previously only blocked upgrades can degrade the cluster, if we previously successfully installed
		// OCP version of CSIDriver
		driverFound := false
		csiDriver, err := apiDependencies.GetCSIDriver(utils.VSphereDriverName)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				reason := fmt.Errorf("vsphere driver install failed with %s, unable to verify CSIDriver status: %v", result.Reason, err)
				klog.Errorf(reason.Error())
				return ClusterCheckDegrade, MakeClusterDegradedError(CheckStatusOpenshiftAPIError, reason)
			}
		} else {
			annotations := csiDriver.GetAnnotations()
			if _, ok := annotations[utils.OpenshiftCSIDriverAnnotationKey]; ok {
				klog.Errorf("vsphere driver install failed with %s, found existing driver", result.Reason)
				driverFound = true
			}
		}

		if driverFound {
			return ClusterCheckDegrade, result
		}
		// if we can't connect to vcenter, we can't really block upgrades but
		// we should mark upgradeable to be unknown
		if result.CheckStatus == CheckStatusVSphereConnectionFailed {
			return ClusterCheckUpgradeStateUnknown, result
		}

		return ClusterCheckBlockUpgrade, result

	case CheckActionBlockUpgrade:
		return ClusterCheckBlockUpgrade, result
	case CheckActionRequiresAdminAck:
		return checkForIntreePluginUse(result, apiDependencies)
	case CheckActionBlockUpgradeDriverInstall:
		return ClusterCheckBlockUpgradeDriverInstall, result
	default:
		return ClusterCheckAllGood, result
	}
}

// returns false is migration is enabled in the cluster.
// returns true if migration is not enabled in the cluster and cluster is using
// in-tree vSphere volumes.
func checkForIntreePluginUse(result ClusterCheckResult, apiDependencies KubeAPIInterface) (ClusterCheckStatus, ClusterCheckResult) {
	storageCR, err := apiDependencies.GetStorage(storageOperatorName)
	if err != nil {
		reason := fmt.Errorf("vsphere csi driver installed failed with %s, unable to verify storage status: %v", result.Reason, err)
		return ClusterCheckDegrade, MakeClusterDegradedError(CheckStatusOpenshiftAPIError, reason)
	}

	klog.Infof("Checking for intree plugin use")

	allGoodCheckResult := MakeClusterCheckResultPass()

	// migration is enabled and hence we should be fine
	driverName := storageCR.Spec.VSphereStorageDriver
	if driverName == v1.CSIWithMigrationDriver {
		return ClusterCheckAllGood, allGoodCheckResult
	}

	pvs, err := apiDependencies.ListPersistentVolumes()
	if err != nil {
		reason := fmt.Errorf("vsphere csi driver installed failed with %s, unable to list pvs: %v", result.Reason, err)
		return ClusterCheckDegrade, MakeClusterDegradedError(CheckStatusOpenshiftAPIError, reason)
	}

	usingvSphereVolumes := false

	for _, pv := range pvs {
		if pv.Spec.VsphereVolume != nil {
			usingvSphereVolumes = true
			break
		}
	}

	if usingvSphereVolumes {
		return ClusterCheckUpgradesBlockedViaAdminAck, allGoodCheckResult
	}

	nodes, err := apiDependencies.ListNodes()
	if err != nil {
		reason := fmt.Errorf("csi driver installed failed with %s, unable to list pvs: %v", result.Reason, err)
		return ClusterCheckDegrade, MakeClusterDegradedError(CheckStatusOpenshiftAPIError, reason)
	}

	if checkInlineIntreeVolumeUse(nodes) {
		return ClusterCheckUpgradesBlockedViaAdminAck, allGoodCheckResult
	}
	return ClusterCheckAllGood, allGoodCheckResult
}

func checkInlineIntreeVolumeUse(nodes []*corev1.Node) bool {
	for _, node := range nodes {
		volumesInUse := node.Status.VolumesInUse
		for _, volume := range volumesInUse {
			if strings.HasPrefix(string(volume), inTreePluginName) {
				return true
			}
		}
	}
	return false
}
