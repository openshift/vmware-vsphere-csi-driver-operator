package utils

import (
	opv1 "github.com/openshift/api/operator/v1"
	"sort"
)

func GetTopologyCategories(clusterCSIDriver *opv1.ClusterCSIDriver) []string {
	driverConfig := clusterCSIDriver.Spec.DriverConfig
	if driverConfig.DriverType == opv1.VSphereDriverType {
		vSphereConfig := driverConfig.VSphere
		if vSphereConfig != nil && len(vSphereConfig.TopologyCategories) > 0 {
			topologyCategories := vSphereConfig.TopologyCategories
			// sort the topologyCategories so as they are stable and don't change their order
			sort.Slice(topologyCategories, func(i, j int) bool {
				return topologyCategories[i] < topologyCategories[j]
			})
			return topologyCategories
		}
	}
	return []string{}
}
