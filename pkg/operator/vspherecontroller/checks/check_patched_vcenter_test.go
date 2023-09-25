package checks

import "testing"

func TestCheckForMinimumPatchedVersion(t *testing.T) {
	tests := []struct {
		name             string
		vSphereVersion   string
		buildNumber      string
		meetsRequirement bool
	}{
		{
			name:             "when vSphere version meets minimum 7 series requirement",
			vSphereVersion:   "7.0.3",
			buildNumber:      "21958406",
			meetsRequirement: true,
		},
		{
			name:             "when vSphere version meets minimum 8 series requirement",
			vSphereVersion:   "8.0.2",
			buildNumber:      "22088126",
			meetsRequirement: true,
		},
		{
			name:             "when vSphere version does not meet 7 series build number requirement",
			vSphereVersion:   "7.0.3",
			buildNumber:      "21324296",
			meetsRequirement: false,
		},
		{
			name:             "when vSphere version does not meet 8 series build number requirement",
			vSphereVersion:   "8.0.1",
			buildNumber:      "998",
			meetsRequirement: false,
		},
		{
			name:             "when vSphere version does not meet 8 series version requirement",
			vSphereVersion:   "8.0.0",
			buildNumber:      "22088127",
			meetsRequirement: false,
		},
		{
			name:             "when vSphere version does not meet 7 series version requirement",
			vSphereVersion:   "7.0.2",
			buildNumber:      "21958406",
			meetsRequirement: false,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			checkFlag, _, err := checkForMinimumPatchedVersion(test.vSphereVersion, test.buildNumber)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if checkFlag != test.meetsRequirement {
				t.Errorf("for checking version requirement, expected %v got %v", test.meetsRequirement, checkFlag)
			}
		})
	}
}
