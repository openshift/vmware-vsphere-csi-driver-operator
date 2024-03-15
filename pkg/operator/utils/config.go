package utils

import (
	opv1 "github.com/openshift/api/operator/v1"
	"strconv"
)

func GetSnapshotOptions(clusterCSIDriver *opv1.ClusterCSIDriver) map[string]string {
	snapshotOptions := map[string]string{}
	if clusterCSIDriver == nil || clusterCSIDriver.Spec.DriverConfig.VSphere == nil {
		return snapshotOptions
	}

	vSphereConfig := clusterCSIDriver.Spec.DriverConfig.VSphere

	options := []string{
		"global-max-snapshots-per-block-volume",
		"granular-max-snapshots-per-block-volume-vsan",
		"granular-max-snapshots-per-block-volume-vvol",
	}

	for _, option := range options {
		var value *uint32

		switch option {
		case "global-max-snapshots-per-block-volume":
			value = vSphereConfig.GlobalMaxSnapshotsPerBlockVolume
		case "granular-max-snapshots-per-block-volume-vsan":
			value = vSphereConfig.GranularMaxSnapshotsPerBlockVolumeInVSAN
		case "granular-max-snapshots-per-block-volume-vvol":
			value = vSphereConfig.GranularMaxSnapshotsPerBlockVolumeInVVOL
		}

		if value != nil {
			snapshotOptions[option] = strconv.FormatUint(uint64(*value), 10)
		}
	}

	return snapshotOptions
}
