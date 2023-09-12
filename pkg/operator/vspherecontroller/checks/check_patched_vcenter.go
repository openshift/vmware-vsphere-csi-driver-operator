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

	// TODO: Fix with real version information
	minPatchedVersion8Series = "8.0.2"
	minBuildNumber8Series    = 999
)

type PatchedVcenterChecker struct{}

var _ CheckInterface = &PatchedVcenterChecker{}

func (v *PatchedVcenterChecker) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	vmClient := checkOpts.vmConnection.Client
	vcenterAPIVersion := vmClient.ServiceContent.About.ApiVersion
	buildVersion := vmClient.ServiceContent.About.Build

	klog.V(2).Infof("checking for patched version of vSphere for CSI migration: %s-%s", vcenterAPIVersion, buildVersion)

	hasMin, err := checkForMinimumPatchedVersion(vcenterAPIVersion, buildVersion)
	if err != nil {
		return []ClusterCheckResult{makeDeprecatedEnvironmentError(CheckStatusVcenterAPIError, err)}
	}

	if !hasMin {
		reason := fmt.Errorf("found version of vCenter which has bugs related to CSI migration. Minimum required, version: %s, build: %d", minPatchedVersion7Series, minBuildNumber7Series)
		return []ClusterCheckResult{makeBuggyEnvironmentError(CheckStatusBuggyMigrationPlatform, reason)}
	}
	return []ClusterCheckResult{}
}

func checkForMinimumPatchedVersion(vSphereVersion string, build string) (bool, error) {
	hasMinimumApiVersion, err := isMinimumVersion(minPatchedVersion7Series, vSphereVersion)
	if err != nil {
		return true, err
	}
	if !hasMinimumApiVersion {
		return false, nil
	}

	if strings.HasPrefix(vSphereVersion, "8") {
		hasMinimumApiVersion, err := isMinimumVersion(minPatchedVersion8Series, vSphereVersion)
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

		if buildNumber >= minBuildNumber8Series {
			return true, nil
		}

		return false, nil
	} else {
		buildNumber, err := strconv.Atoi(build)
		if err != nil {
			return true, fmt.Errorf("error converting build number %s to integer", build)
		}

		if buildNumber >= minBuildNumber7Series {
			return true, nil
		}

		return false, nil
	}
}
