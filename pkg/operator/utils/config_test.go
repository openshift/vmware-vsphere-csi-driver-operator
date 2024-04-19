package utils

import (
	"reflect"
	"testing"

	opv1 "github.com/openshift/api/operator/v1"
)

func TestSnapshotConfiguration(t *testing.T) {
	globalMaxSnapshotsPerBlockVolume := uint32(5)
	granularMaxSnapshotsPerBlockVolumeInVSAN := uint32(10)
	granularMaxSnapshotsPerBlockVolumeInVVOL := uint32(15)

	emptyClusterCSIDriver := &opv1.ClusterCSIDriver{
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				VSphere: &opv1.VSphereCSIDriverConfigSpec{},
			},
		}}
	globalMaxSnapshotClusterCSIDriver := &opv1.ClusterCSIDriver{
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				VSphere: &opv1.VSphereCSIDriverConfigSpec{
					GlobalMaxSnapshotsPerBlockVolume: &globalMaxSnapshotsPerBlockVolume,
				},
			},
		}}
	allSnapshotOptionsClusterCSIDriver := &opv1.ClusterCSIDriver{
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				VSphere: &opv1.VSphereCSIDriverConfigSpec{
					GlobalMaxSnapshotsPerBlockVolume:         &globalMaxSnapshotsPerBlockVolume,
					GranularMaxSnapshotsPerBlockVolumeInVSAN: &granularMaxSnapshotsPerBlockVolumeInVSAN,
					GranularMaxSnapshotsPerBlockVolumeInVVOL: &granularMaxSnapshotsPerBlockVolumeInVVOL,
				},
			},
		}}

	tests := []struct {
		name             string
		clusterCSIDriver *opv1.ClusterCSIDriver
		expectedResult   map[string]string
	}{
		{
			name:             "no configuration",
			clusterCSIDriver: emptyClusterCSIDriver,
			expectedResult:   map[string]string{},
		},
		{
			name:             "only global max snapshot limit configured",
			clusterCSIDriver: globalMaxSnapshotClusterCSIDriver,
			expectedResult: map[string]string{
				"global-max-snapshots-per-block-volume": "5",
			},
		},
		{
			name:             "all snapshot options configured",
			clusterCSIDriver: allSnapshotOptionsClusterCSIDriver,
			expectedResult: map[string]string{
				"global-max-snapshots-per-block-volume":        "5",
				"granular-max-snapshots-per-block-volume-vsan": "10",
				"granular-max-snapshots-per-block-volume-vvol": "15",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := GetSnapshotOptions(test.clusterCSIDriver)
			if !reflect.DeepEqual(result, test.expectedResult) {
				t.Errorf("Unexpected result: %s\nExpected: %s", result, test.expectedResult)
			}
		})
	}
}
