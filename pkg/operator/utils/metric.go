package utils

import (
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	failureReason = "failure_reason"
	condition     = "condition"

	topologyTagSource                 = "source"
	topologyTagSourceClusterCSIDriver = "clustercsidriver"
	topologyTagSourceInfrastructure   = "infrastructure"

	domainScope         = "scope"
	scopeRegions        = "regions"
	scopeZones          = "zones"
	scopeVCenters       = "vcenters"
	scopeDatacenters    = "datacenters"
	scopeDatastores     = "datastores"
	scopeFailureDomains = "failure_domains"
)

var (
	InstallErrorMetric = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           "vsphere_csi_driver_error",
			Help:           "vSphere driver installation error",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{failureReason, condition},
	)

	TopologyTagsMetric = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           "vsphere_topology_tags",
			Help:           "Number of vSphere topology tags",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{topologyTagSource},
	)
	InfrastructureFailureDomains = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           "vsphere_infrastructure_failure_domains",
			Help:           "Number of vSphere failure domains",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{domainScope},
	)
)

func init() {
	legacyregistry.MustRegister(InstallErrorMetric)
	legacyregistry.MustRegister(TopologyTagsMetric)
	legacyregistry.MustRegister(InfrastructureFailureDomains)
}
