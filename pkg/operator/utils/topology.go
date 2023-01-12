package utils

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/legacy-cloud-providers/vsphere"
)

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
