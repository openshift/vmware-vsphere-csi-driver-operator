package utils

import (
	opv1 "github.com/openshift/api/operator/v1"
)

func GetTopologyCategories(clusterCSIDriver *opv1.ClusterCSIDriver) []string {
	driverConfig := clusterCSIDriver.Spec.DriverConfig
	if driverConfig.DriverType == opv1.VSphereDriverType {
		vSphereConfig := driverConfig.VSphere
		if vSphereConfig != nil && len(vSphereConfig.TopologyCategories) > 0 {
			return vSphereConfig.TopologyCategories
		}
	}
	return []string{}
}
