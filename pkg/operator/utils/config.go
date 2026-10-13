package utils

import (
	opv1 "github.com/openshift/api/operator/v1"
	"strconv"
)

func GetMaxVolumesPerNode(clusterCSIDriver *opv1.ClusterCSIDriver) int {
	maxVolumesPerNode := MaxVolumesPerNodeVSphere7

	if clusterCSIDriver != nil && clusterCSIDriver.Spec.DriverConfig.VSphere != nil &&
		clusterCSIDriver.Spec.DriverConfig.VSphere.MaxAllowedBlockVolumesPerNode > 0 {
		maxVolumesPerNode = int(clusterCSIDriver.Spec.DriverConfig.VSphere.MaxAllowedBlockVolumesPerNode)
	}

	return maxVolumesPerNode
}

type SnapshotOption struct {
	Key   string
	Value string
}

func addOption(options []SnapshotOption, name string, intValue *uint32) []SnapshotOption {
	if intValue == nil {
		return options
	}
	return append(options, SnapshotOption{
		Key:   name,
		Value: strconv.FormatUint(uint64(*intValue), 10),
	})
}

func GetSnapshotOptions(clusterCSIDriver *opv1.ClusterCSIDriver) []SnapshotOption {
	if clusterCSIDriver == nil || clusterCSIDriver.Spec.DriverConfig.VSphere == nil {
		return nil
	}

	vSphereConfig := clusterCSIDriver.Spec.DriverConfig.VSphere
	var options []SnapshotOption

	options = addOption(options, "global-max-snapshots-per-block-volume", vSphereConfig.GlobalMaxSnapshotsPerBlockVolume)
	options = addOption(options, "granular-max-snapshots-per-block-volume-vsan", vSphereConfig.GranularMaxSnapshotsPerBlockVolumeInVSAN)
	options = addOption(options, "granular-max-snapshots-per-block-volume-vvol", vSphereConfig.GranularMaxSnapshotsPerBlockVolumeInVVOL)

	return options
}
