package checks

import (
	"context"
	"fmt"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
)

const (
	minRequiredVCenterVersion    = "6.7.3"
	minUpgradeableVCenterVersion = "7.0.2"
)

type VCenterChecker struct{}

var _ CheckInterface = &VCenterChecker{}

func (v *VCenterChecker) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	for _, vConn := range checkOpts.vmConnection {
		vmClient := vConn.Client
		vcenterAPIVersion := vmClient.ServiceContent.About.ApiVersion

		hasRequiredMinimum, err := utils.IsMinimumVersion(minRequiredVCenterVersion, vcenterAPIVersion)
		// if we can't determine the version, we are going to mark cluster as upgrade
		// disabled without degrading the cluster
		if err != nil {
			reason := fmt.Errorf("error parsing minimum version %v", err)
			return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, reason)}
		}
		if !hasRequiredMinimum {
			reason := fmt.Errorf("found older vcenter version %s, minimum required version is %s", vcenterAPIVersion, minRequiredVCenterVersion)
			return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusDeprecatedVCenter, reason)}
		}

		hasUpgradeableMinimum, err := utils.IsMinimumVersion(minUpgradeableVCenterVersion, vcenterAPIVersion)
		if err != nil {
			reason := fmt.Errorf("error parsing minimum version %v", err)
			return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, reason)}
		}
		if !hasUpgradeableMinimum {
			reason := fmt.Errorf("found older vcenter version %s, minimum required version for upgrade is %s", vcenterAPIVersion, minUpgradeableVCenterVersion)
			return []ClusterCheckResult{MakeClusterUnupgradeableError(CheckStatusDeprecatedVCenter, reason)}
		}
	}

	return []ClusterCheckResult{}
}
