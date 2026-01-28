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
		expectedResult   []SnapshotOption
	}{
		{
			name:             "no configuration",
			clusterCSIDriver: emptyClusterCSIDriver,
			expectedResult:   nil,
		},
		{
			name:             "only global max snapshot limit configured",
			clusterCSIDriver: globalMaxSnapshotClusterCSIDriver,
			expectedResult: []SnapshotOption{
				{Key: "global-max-snapshots-per-block-volume", Value: "5"},
			},
		},
		{
			name:             "all snapshot options configured",
			clusterCSIDriver: allSnapshotOptionsClusterCSIDriver,
			expectedResult: []SnapshotOption{
				{Key: "global-max-snapshots-per-block-volume", Value: "5"},
				{Key: "granular-max-snapshots-per-block-volume-vsan", Value: "10"},
				{Key: "granular-max-snapshots-per-block-volume-vvol", Value: "15"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := GetSnapshotOptions(test.clusterCSIDriver)
			if !reflect.DeepEqual(result, test.expectedResult) {
				t.Errorf("Unexpected result: %v\nExpected: %v", result, test.expectedResult)
			}
		})
	}
}
