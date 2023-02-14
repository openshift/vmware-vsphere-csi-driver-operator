package utils

import (
	"fmt"
	"strings"

	cfgv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/legacy-cloud-providers/vsphere"
)

const (
	defaultOpenshiftZoneCategory   = "openshift-zone"
	defaultOpenshiftRegionCategory = "openshift-region"
)

func GetInfraTopologyCategories(infra *cfgv1.Infrastructure) []string {
	vSpherePlatformConfig := infra.Spec.PlatformSpec.VSphere
	if vSpherePlatformConfig != nil {
		failureDomains := vSpherePlatformConfig.FailureDomains
		if len(failureDomains) > 0 {
			return []string{defaultOpenshiftZoneCategory, defaultOpenshiftRegionCategory}
		}
	}
	return []string{}
}

func GetCSIDriverTopologyCategories(clusterCSIDriver *opv1.ClusterCSIDriver) []string {
	driverConfig := clusterCSIDriver.Spec.DriverConfig
	if driverConfig.DriverType == opv1.VSphereDriverType {
		vSphereConfig := driverConfig.VSphere
		if vSphereConfig != nil && len(vSphereConfig.TopologyCategories) > 0 {
			return vSphereConfig.TopologyCategories
		}
	}
	return []string{}
}

func GetTopologyCategories(clusterCSIDriver *opv1.ClusterCSIDriver, infra *cfgv1.Infrastructure) []string {
	infraCategories := GetInfraTopologyCategories(infra)
	if len(infraCategories) > 0 {
		return infraCategories
	}
	return GetCSIDriverTopologyCategories(clusterCSIDriver)
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

func UpdateMetrics(infra *cfgv1.Infrastructure, clusterCSIDriver *opv1.ClusterCSIDriver) {
	domains := GetCSIDriverTopologyCategories(clusterCSIDriver)
	TopologyTagsMetric.WithLabelValues(topologyTagSourceClusterCSIDriver).Set(float64(len(domains)))
	domains = GetInfraTopologyCategories(infra)
	TopologyTagsMetric.WithLabelValues(topologyTagSourceInfrastructure).Set(float64(len(domains)))

	vSpherePlatformConfig := infra.Spec.PlatformSpec.VSphere
	if vSpherePlatformConfig == nil {
		// Reset all metrics
		InfrastructureFailureDomains.WithLabelValues(scopeDatacenters).Set(0.0)
		InfrastructureFailureDomains.WithLabelValues(scopeDatastores).Set(0.0)
		InfrastructureFailureDomains.WithLabelValues(scopeRegions).Set(0.0)
		InfrastructureFailureDomains.WithLabelValues(scopeZones).Set(0.0)
		InfrastructureFailureDomains.WithLabelValues(scopeVCenters).Set(0.0)
		InfrastructureFailureDomains.WithLabelValues(scopeFailureDomains).Set(0.0)
		return
	}

	// Report detail topology from Infrastructure (ClusterCSIDriver does not have that level of detail)
	datacenterDatastores := map[string]sets.String{} // datacenter -> list of its datastores
	regionZones := map[string]sets.String{}          // region -> list of its zones
	vCenters := sets.NewString()                     // list of vCenters

	for _, fd := range vSpherePlatformConfig.FailureDomains {
		region := fd.Region
		zone := fd.Zone
		if region != "" {
			zones := regionZones[region]
			if zones == nil {
				zones = sets.NewString()
			}
			if zone != "" {
				zones.Insert(zone)
			}
			regionZones[region] = zones
		}

		datacenter := fd.Topology.Datacenter
		datastore := fd.Topology.Datastore
		if datacenter != "" {
			datastores := datacenterDatastores[datacenter]
			if datastores == nil {
				datastores = sets.NewString()
			}
			if datastore != "" {
				datastores.Insert(datastore)
			}
			datacenterDatastores[datacenter] = datastores
		}

		vCenter := fd.Server
		if vCenter != "" {
			vCenters.Insert(vCenter)
		}
	}

	InfrastructureFailureDomains.WithLabelValues(scopeDatacenters).Set(float64(len(datacenterDatastores)))
	datastoreCount := 0
	for _, datastores := range datacenterDatastores {
		datastoreCount += datastores.Len()
	}
	InfrastructureFailureDomains.WithLabelValues(scopeDatastores).Set(float64(datastoreCount))

	InfrastructureFailureDomains.WithLabelValues(scopeRegions).Set(float64(len(regionZones)))
	zoneCount := 0
	for _, zones := range regionZones {
		zoneCount += zones.Len()
	}
	InfrastructureFailureDomains.WithLabelValues(scopeZones).Set(float64(zoneCount))

	InfrastructureFailureDomains.WithLabelValues(scopeVCenters).Set(float64(len(vCenters)))
	InfrastructureFailureDomains.WithLabelValues(scopeFailureDomains).Set(float64(len(vSpherePlatformConfig.FailureDomains)))
}
