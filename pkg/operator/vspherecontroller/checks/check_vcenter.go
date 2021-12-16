package checks

import (
	"context"
	"fmt"
	"github.com/blang/semver"
	"strings"
)

const minVCenterVersion = "6.7.3"

type VCenterChecker struct{}

var _ CheckInterface = &VCenterChecker{}

func (v *VCenterChecker) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	vmClient := checkOpts.vmConnection.Client
	vcenterAPIVersion := vmClient.ServiceContent.About.ApiVersion
	hasMinimum, err := isMinimumVersion(minVCenterVersion, vcenterAPIVersion)

	// if we can't determine the version, we are going to mark cluster as upgrade
	// disabled without degrading the cluster
	if err != nil {
		reason := fmt.Errorf("error parsing minimum version %v", err)
		return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, reason)}
	}
	if !hasMinimum {
		reason := fmt.Errorf("found older vcenter version %s, minimum required version is %s", vcenterAPIVersion, minVCenterVersion)
		return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusDeprecatedVCenter, reason)}
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
