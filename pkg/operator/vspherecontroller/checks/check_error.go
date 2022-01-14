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
)

type ClusterCheckStatus string

const (
	ClusterCheckAllGood             ClusterCheckStatus = "pass"
	ClusterCheckBlockUpgrade        ClusterCheckStatus = "upgrades_blocked"
	ClusterCheckUpgradeStateUnknown ClusterCheckStatus = "upgrades_unknown"
	ClusterCheckDegrade             ClusterCheckStatus = "degraded"
)

type ClusterCheckResult struct {
	CheckError     error
	CheckStatus    CheckStatusType
	BlockUpgrade   bool
	ClusterDegrade bool
	// if we are blocking cluster upgrades or degrading the cluster
	// this message contains information about why we are degrading the cluster
	// or blocking upgrades.
	Reason string
}

func MakeClusterCheckResultPass() ClusterCheckResult {
	return ClusterCheckResult{
		CheckError:   nil,
		CheckStatus:  CheckStatusPass,
		BlockUpgrade: false,
	}
}

func makeFoundExistingDriverResult(reason error) ClusterCheckResult {
	checkResult := ClusterCheckResult{
		CheckStatus:  CheckStatusExistingDriverFound,
		CheckError:   reason,
		BlockUpgrade: true,
		Reason:       reason.Error(),
	}
	return checkResult
}

func makeDeprecatedEnvironmentError(statusType CheckStatusType, reason error) ClusterCheckResult {
	checkResult := ClusterCheckResult{
		CheckStatus:    statusType,
		CheckError:     reason,
		BlockUpgrade:   true,
		ClusterDegrade: false,
		Reason:         reason.Error(),
	}
	return checkResult
}

func MakeGenericVCenterAPIError(reason error) ClusterCheckResult {
	return ClusterCheckResult{
		CheckStatus:    CheckStatusVcenterAPIError,
		CheckError:     reason,
		BlockUpgrade:   true,
		ClusterDegrade: false,
		Reason:         reason.Error(),
	}
}

func MakeClusterDegradedError(checkStatus CheckStatusType, reason error) ClusterCheckResult {
	return ClusterCheckResult{
		CheckStatus:    checkStatus,
		CheckError:     reason,
		BlockUpgrade:   false,
		ClusterDegrade: true,
		Reason:         reason.Error(),
	}
}

func CheckClusterStatus(result ClusterCheckResult, apiDependencies KubeAPIInterface) (ClusterCheckStatus, ClusterCheckResult) {
	if result.ClusterDegrade {
		return ClusterCheckDegrade, result
	}

	// if we can't connect to vcenter, we can't really block upgrades but
	// we should mark upgradeable to be unknown
	if result.CheckStatus == CheckStatusVSphereConnectionFailed && result.BlockUpgrade {
		return ClusterCheckUpgradeStateUnknown, result
	}

	// a failed check that previously only blocked upgrades can degrade the cluster, if we previously successfully installed
	// OCP version of CSIDriver
	if result.BlockUpgrade {
		csiDriver, err := apiDependencies.GetCSIDriver(utils.VSphereDriverName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ClusterCheckBlockUpgrade, result
			}
			reason := fmt.Errorf("vsphere driver install failed with %s, unable to verify CSIDriver status: %v", result.Reason, err)
			klog.Errorf(reason.Error())
			return ClusterCheckDegrade, MakeClusterDegradedError(CheckStatusOpenshiftAPIError, reason)
		}
		annotations := csiDriver.GetAnnotations()
		if _, ok := annotations[utils.OpenshiftCSIDriverAnnotationKey]; ok {
			klog.Errorf("vsphere driver install failed with %s, found existing driver", result.Reason)
			return ClusterCheckDegrade, result
		}
		return ClusterCheckBlockUpgrade, result
	}
	return ClusterCheckAllGood, result
}
