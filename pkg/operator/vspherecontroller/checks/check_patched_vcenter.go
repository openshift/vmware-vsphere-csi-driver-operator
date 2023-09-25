package checks

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

const (
	minPatchedVersion7Series = "7.0.3"
	minBuildNumber7Series    = 21424296

	minPatchedVersion8Series = "8.0.1"
	minBuildNumber8Series    = 22088125
)

type PatchedVcenterChecker struct{}

var _ CheckInterface = &PatchedVcenterChecker{}

func (v *PatchedVcenterChecker) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	vmClient := checkOpts.vmConnection.Client
	vcenterAPIVersion := vmClient.ServiceContent.About.ApiVersion
	buildVersion := vmClient.ServiceContent.About.Build

	klog.V(2).Infof("checking for patched version of vSphere for CSI migration: %s-%s", vcenterAPIVersion, buildVersion)

	hasMin, minString, err := checkForMinimumPatchedVersion(vcenterAPIVersion, buildVersion)
	if err != nil {
		return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, err)}
	}

	if !hasMin {
		reason := fmt.Errorf("found version of vCenter which has bugs related to CSI migration. Minimum required, %s", minString)
		return []ClusterCheckResult{makeBuggyEnvironmentError(CheckStatusBuggyMigrationPlatform, reason)}
	}
	return []ClusterCheckResult{}
}

func checkForMinimumPatchedVersion(vSphereVersion string, build string) (bool, string, error) {
	hasMinimumApiVersion, err := isMinimumVersion(minPatchedVersion7Series, vSphereVersion)
	min7SeriesString := fmt.Sprintf(">= %s-%d", minPatchedVersion7Series, minBuildNumber7Series)

	if err != nil {
		return true, min7SeriesString, err
	}
	if !hasMinimumApiVersion {
		return false, min7SeriesString, nil
	}

	if strings.HasPrefix(vSphereVersion, "8") {
		hasMinimumApiVersion, err := isMinimumVersion(minPatchedVersion8Series, vSphereVersion)
		min8SeriesString := fmt.Sprintf("> %s-%d", minPatchedVersion8Series, minBuildNumber8Series)
		if err != nil {
			return true, min8SeriesString, err
		}

		if !hasMinimumApiVersion {
			return false, min8SeriesString, nil
		}

		buildNumber, err := strconv.Atoi(build)
		if err != nil {
			return true, min8SeriesString, fmt.Errorf("error converting build number %s to integer", build)
		}

		if buildNumber > minBuildNumber8Series {
			return true, min8SeriesString, nil
		}

		return false, min8SeriesString, nil
	} else {
		buildNumber, err := strconv.Atoi(build)
		if err != nil {
			return true, min7SeriesString, fmt.Errorf("error converting build number %s to integer", build)
		}

		if buildNumber >= minBuildNumber7Series {
			return true, min7SeriesString, nil
		}

		return false, min7SeriesString, nil
	}
}
