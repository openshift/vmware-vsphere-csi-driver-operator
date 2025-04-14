package checks

import (
	"fmt"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

type CheckStatusType string

const (
	CheckStatusPass                    CheckStatusType = "pass"
	CheckStatusVSphereConnectionFailed CheckStatusType = "vsphere_connection_failed"
	CheckStatusOpenshiftAPIError       CheckStatusType = "openshift_api_error"
	CheckStatusExistingDriverFound     CheckStatusType = "existing_driver_found"
	CheckStatusDeprecatedVCenter       CheckStatusType = "check_deprecated_vcenter"
	CheckStatusDeprecatedHWVersion     CheckStatusType = "check_deprecated_hw_version"
	CheckStatusDeprecatedESXIVersion   CheckStatusType = "check_deprecated_esxi_version"
	CheckStatusVcenterAPIError         CheckStatusType = "vcenter_api_error"
	CheckStatusGenericError            CheckStatusType = "generic_error"
	CheckStatusBlockVolumeLimitError   CheckStatusType = "block_volume_limit_error"
)

type ClusterCheckStatus string

const (
	ClusterCheckAllGood                   ClusterCheckStatus = "pass"
	ClusterCheckBlockUpgradeDriverInstall ClusterCheckStatus = "installation_blocked"
	ClusterCheckBlockUpgrade              ClusterCheckStatus = "upgrades_blocked"
	ClusterCheckUpgradeStateUnknown       ClusterCheckStatus = "upgrades_unknown"
	ClusterCheckDegrade                   ClusterCheckStatus = "degraded"
)

type CheckAction int

// Ordered by severity, Pass must be 0 (for struct initialization).
const (
	CheckActionPass                      = iota
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
	default:
		return "Unknown"
	}
}

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
			return ClusterCheckDegrade, MakeClusterDegradedError(result.CheckStatus, result.CheckError)
		}

		// if we can't connect to vcenter, we can't really block upgrades but
		// we should mark upgradeable to be unknown
		if result.CheckStatus == CheckStatusVSphereConnectionFailed {
			return ClusterCheckUpgradeStateUnknown, result
		}

		return ClusterCheckBlockUpgrade, result

	case CheckActionBlockUpgrade:
		return ClusterCheckBlockUpgrade, result

	case CheckActionBlockUpgradeDriverInstall:
		return ClusterCheckBlockUpgradeDriverInstall, result

	default:
		return ClusterCheckAllGood, result
	}
}
