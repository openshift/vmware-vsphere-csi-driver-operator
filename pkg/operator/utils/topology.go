package utils

import (
	"fmt"
	"strings"

	opv1 "github.com/openshift/api/operator/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/legacy-cloud-providers/vsphere"
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

func GetDatacenters(config *vsphere.VSphereConfig) ([]string, error) {
	datacenters := []string{config.Workspace.Datacenter}

	virtualCenterIPs := sets.StringKeySet(config.VirtualCenter)

	if len(virtualCenterIPs) != 1 {
		return nil, fmt.Errorf("cloud config must define a single VirtualCenter")
	}

	virtualCenterIP := virtualCenterIPs.List()[0]
	if virtualCenterConfig, ok := config.VirtualCenter[virtualCenterIP]; ok {
		datacenters = strings.Split(virtualCenterConfig.Datacenters, ",")
	}
	return datacenters, nil
}
