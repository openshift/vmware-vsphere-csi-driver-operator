package checks

import (
	"context"
	"fmt"
	"strings"

	"github.com/blang/semver"
)

const (
	minRequiredVCenterVersion    = "6.7.3"
	minUpgradeableVCenterVersion = "7.0.2"
)

type VCenterChecker struct{}

var _ CheckInterface = &VCenterChecker{}

func (v *VCenterChecker) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	vmClient := checkOpts.vmConnection.Client
	vcenterAPIVersion := vmClient.ServiceContent.About.ApiVersion

	hasRequiredMinimum, err := isMinimumVersion(minRequiredVCenterVersion, vcenterAPIVersion)
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

	hasUpgradeableMinimum, err := isMinimumVersion(minUpgradeableVCenterVersion, vcenterAPIVersion)
	if err != nil {
		reason := fmt.Errorf("error parsing minimum version %v", err)
		return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, reason)}
	}
	if !hasUpgradeableMinimum {
		reason := fmt.Errorf("found older vcenter version %s, minimum required version for upgrade is %s", vcenterAPIVersion, minUpgradeableVCenterVersion)
		return []ClusterCheckResult{MakeClusterUnupgradeableError(CheckStatusDeprecatedVCenter, reason)}
	}

	return []ClusterCheckResult{}
}

func isMinimumVersion(minimumVersion string, currentVersion string) (bool, error) {
	minimumSemver, err := semver.New(minimumVersion)
	if err != nil {
		return true, err
	}
	semverString := parseForSemver(currentVersion)
	currentSemVer, err := semver.ParseTolerant(semverString)
	if err != nil {
		return true, err
	}
	if currentSemVer.Compare(*minimumSemver) >= 0 {
		return true, nil
	}
	return false, nil
}

func parseForSemver(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) > 3 {
		return strings.Join(parts[0:3], ".")
	}
	return version
}
