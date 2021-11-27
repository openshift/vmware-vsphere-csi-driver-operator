package checks

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
