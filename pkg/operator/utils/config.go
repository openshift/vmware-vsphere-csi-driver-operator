package utils

import (
	opv1 "github.com/openshift/api/operator/v1"
	"strconv"
)

func addOption(options map[string]string, name string, intValue *uint32) {
	if intValue == nil {
		return
	}
	options[name] = strconv.FormatUint(uint64(*intValue), 10)
}

func GetMaxVolumesPerNode(clusterCSIDriver *opv1.ClusterCSIDriver) int {
	maxVolumesPerNode := MaxVolumesPerNodeVSphere7

	if clusterCSIDriver != nil && clusterCSIDriver.Spec.DriverConfig.VSphere != nil &&
		clusterCSIDriver.Spec.DriverConfig.VSphere.MaxAllowedBlockVolumesPerNode > 0 {
		maxVolumesPerNode = int(clusterCSIDriver.Spec.DriverConfig.VSphere.MaxAllowedBlockVolumesPerNode)
	}

	return maxVolumesPerNode
}

func GetSnapshotOptions(clusterCSIDriver *opv1.ClusterCSIDriver) map[string]string {
	snapshotOptions := map[string]string{}
	if clusterCSIDriver == nil || clusterCSIDriver.Spec.DriverConfig.VSphere == nil {
		return nil
	}

	vSphereConfig := clusterCSIDriver.Spec.DriverConfig.VSphere

	addOption(snapshotOptions, "global-max-snapshots-per-block-volume", vSphereConfig.GlobalMaxSnapshotsPerBlockVolume)
	addOption(snapshotOptions, "granular-max-snapshots-per-block-volume-vsan", vSphereConfig.GranularMaxSnapshotsPerBlockVolumeInVSAN)
	addOption(snapshotOptions, "granular-max-snapshots-per-block-volume-vvol", vSphereConfig.GranularMaxSnapshotsPerBlockVolumeInVVOL)

	return snapshotOptions
}
