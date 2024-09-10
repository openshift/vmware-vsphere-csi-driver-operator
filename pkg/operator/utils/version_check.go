package utils

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/blang/semver"
)

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
	hasMinimumApiVersion, err := IsMinimumVersion(minRequirement.MinimumVersion7Series, vSphereVersion)
	min7SeriesString := fmt.Sprintf(">= %s-%d", minRequirement.MinimumVersion7Series, minRequirement.MinimumBuild7Series)

	if err != nil {
		return true, min7SeriesString, err
	}
	if !hasMinimumApiVersion {
		return false, min7SeriesString, nil
	}

	if strings.HasPrefix(vSphereVersion, "8") {
		hasMinimumApiVersion, err := IsMinimumVersion(minRequirement.MinimumVersion8Series, vSphereVersion)
		min8SeriesString := fmt.Sprintf("> %s-%d", minRequirement.MinimumVersion8Series, minRequirement.MinimumBuild8Series)
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

		if buildNumber > minRequirement.MinimumBuild8Series {
			return true, min8SeriesString, nil
		}

		return false, min8SeriesString, nil
	} else {
		buildNumber, err := strconv.Atoi(build)
		if err != nil {
			return true, min7SeriesString, fmt.Errorf("error converting build number %s to integer", build)
		}

		if buildNumber >= minRequirement.MinimumBuild7Series {
			return true, min7SeriesString, nil
		}

		return false, min7SeriesString, nil
	}
}
