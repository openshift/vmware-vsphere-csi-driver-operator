package checks

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/klog/v2"
)

const (
	minPatchedVersion = "7.0.3"
	minBuildNumber    = 21424296
)

type PatchedVcenterChecker struct{}

var _ CheckInterface = &PatchedVcenterChecker{}

func (v *PatchedVcenterChecker) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	vmClient := checkOpts.vmConnection.Client
	vcenterAPIVersion := vmClient.ServiceContent.About.ApiVersion
	buildVersion := vmClient.ServiceContent.About.Build

	klog.Infof("checking for patched version of vSphere for CSI migration: %s-%s", vcenterAPIVersion, buildVersion)

	hasMin, err := checkForMinimumPatchedVersion(vcenterAPIVersion, buildVersion)
	if err != nil {
		return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, err)}
	}

	if !hasMin {
		reason := fmt.Errorf("found version of vCenter which has bugs related to CSI migration. Minimum required, version: %s, build: %d", minPatchedVersion, minBuildNumber)
		return []ClusterCheckResult{makeBuggyEnvironmentError(CheckStatusBuggyMigrationPlatform, reason)}
	}
	return []ClusterCheckResult{}
}

func checkForMinimumPatchedVersion(vCenterVersion string, build string) (bool, error) {
	hasMinimumApiVersion, err := isMinimumVersion(minPatchedVersion, vCenterVersion)
	if err != nil {
		return true, err
	}
	if !hasMinimumApiVersion {
		return false, nil
	}

	buildNumber, err := strconv.Atoi(build)
	if err != nil {
		return true, fmt.Errorf("error converting build number %s to integer", build)
	}

	if buildNumber >= minBuildNumber {
		return true, nil
	}

	return false, nil
}
