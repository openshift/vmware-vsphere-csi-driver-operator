package utils

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/blang/semver"
)

const defaultMinimumVersion = "8.0.2"

func IsMinimumVersion(minimumVersion string, currentVersion string) (bool, error) {
	minimumSemver, err := semver.New(minimumVersion)
	if err != nil {
		return true, err
	}
	semverString := ParseForSemver(currentVersion)
	currentSemVer, err := semver.ParseTolerant(semverString)
	if err != nil {
		return true, err
	}
	if currentSemVer.Compare(*minimumSemver) >= 0 {
		return true, nil
	}
	return false, nil
}

func ParseForSemver(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) > 3 {
		return strings.Join(parts[0:3], ".")
	}
	return version
}

type PatchVersionRequirements struct {
	MinimumVersion7Series string
	MinimumBuild7Series   int

	MinimumVersion8Series string
	MinimumBuild8Series   int
}

func CheckForMinimumPatchedVersion(minRequirement PatchVersionRequirements, vSphereVersion string, build string) (bool, string, error) {
	if strings.HasPrefix(vSphereVersion, "7") {
		return checkVersionAndBuildNumber(vSphereVersion, build, minRequirement.MinimumVersion7Series, minRequirement.MinimumBuild7Series)
	} else if strings.HasPrefix(vSphereVersion, "8") {
		return checkVersionAndBuildNumber(vSphereVersion, build, minRequirement.MinimumVersion8Series, minRequirement.MinimumBuild8Series)
	} else {
		hasMinimumApiVersion, err := IsMinimumVersion(defaultMinimumVersion, vSphereVersion)
		if err != nil {
			return false, defaultMinimumVersion, err
		}

		if !hasMinimumApiVersion {
			return false, defaultMinimumVersion, nil
		}
		return true, defaultMinimumVersion, nil
	}
}

func checkVersionAndBuildNumber(version string, build string, minVersion string, minBuildNumber int) (bool, string, error) {
	minVersionString := fmt.Sprintf(">= %s-%d", minVersion, minBuildNumber)

	hasMinimumApiVersion, err := IsMinimumVersion(minVersion, version)
	if err != nil {
		return false, minVersionString, err
	}

	if !hasMinimumApiVersion {
		return false, minVersionString, nil
	}
	buildNumber, err := strconv.Atoi(build)
	if err != nil {
		return true, minVersionString, fmt.Errorf("error converting build number %s to integer", build)
	}
	if buildNumber >= minBuildNumber {
		return true, minVersionString, nil
	}
	return false, minVersionString, nil
}
