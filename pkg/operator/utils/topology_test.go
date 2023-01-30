package utils

import (
	"strings"
	"testing"

	cfgv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

func TestUpdateMetrics(t *testing.T) {
	emptyClusterCSIDriver := &opv1.ClusterCSIDriver{
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				DriverType: opv1.VSphereDriverType,
				VSphere: &opv1.VSphereCSIDriverConfigSpec{
					TopologyCategories: []string{},
				},
			},
		}}

	nilClusterCSIDriver := &opv1.ClusterCSIDriver{
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				DriverType: opv1.VSphereDriverType,
				VSphere:    nil,
			},
		}}

	twoCategoriesClusterCSIDriver := &opv1.ClusterCSIDriver{
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				DriverType: opv1.VSphereDriverType,
				VSphere: &opv1.VSphereCSIDriverConfigSpec{
					TopologyCategories: []string{"region", "zone"},
				},
			},
		}}

	emptyInfra := &cfgv1.Infrastructure{
		Spec: cfgv1.InfrastructureSpec{
			PlatformSpec: cfgv1.PlatformSpec{
				Type: cfgv1.VSpherePlatformType,
				VSphere: &cfgv1.VSpherePlatformSpec{
					FailureDomains: nil,
				},
			},
		},
	}

	nilInfra := &cfgv1.Infrastructure{
		Spec: cfgv1.InfrastructureSpec{
			PlatformSpec: cfgv1.PlatformSpec{
				Type:    cfgv1.VSpherePlatformType,
				VSphere: nil,
			},
		},
	}

	// Note: region corresponds to datacenter and zone to datastore in these tests,
	// to save us from two sets of tests.
	oneRegionTwoZonesInfra := emptyInfra.DeepCopy()
	oneRegionTwoZonesInfra.Spec.PlatformSpec.VSphere.FailureDomains = []cfgv1.VSpherePlatformFailureDomainSpec{
		{
			Name:   "region1-zone1",
			Region: "region1",
			Zone:   "zone1",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter1",
				ComputeCluster: "computecluster1",
				Networks:       nil,
				Datastore:      "datastore1",
				ResourcePool:   "resourcepool1",
				Folder:         "folder1",
			},
		},
		{
			Name:   "region1-zone2",
			Region: "region1",
			Zone:   "zone2",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter1",
				ComputeCluster: "computecluster1",
				Networks:       nil,
				Datastore:      "datastore2",
				ResourcePool:   "resourcepool2",
				Folder:         "folder2",
			},
		},
	}

	twoRegionsTwoZonesInfra := emptyInfra.DeepCopy()
	twoRegionsTwoZonesInfra.Spec.PlatformSpec.VSphere.FailureDomains = []cfgv1.VSpherePlatformFailureDomainSpec{
		{
			Name:   "region1-zone1",
			Region: "region1",
			Zone:   "zone1",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter1",
				ComputeCluster: "computecluster1",
				Networks:       nil,
				Datastore:      "datastore1",
				ResourcePool:   "resourcepool1",
				Folder:         "folder1",
			},
		},
		{
			Name:   "region1-zone2",
			Region: "region1",
			Zone:   "zone2",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter1",
				ComputeCluster: "computecluster1",
				Networks:       nil,
				Datastore:      "datastore2",
				ResourcePool:   "resourcepool2",
				Folder:         "folder2",
			},
		},
		{
			Name:   "region2-zone1",
			Region: "region2",
			Zone:   "zone1",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter2",
				ComputeCluster: "computecluster2",
				Networks:       nil,
				Datastore:      "datastore1",
				ResourcePool:   "resourcepool1",
				Folder:         "folder1",
			},
		},
		{
			Name:   "region2-zone2",
			Region: "region2",
			Zone:   "zone2",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter2",
				ComputeCluster: "computecluster2",
				Networks:       nil,
				Datastore:      "datastore2",
				ResourcePool:   "resourcepool2",
				Folder:         "folder2",
			},
		},
	}

	twoVCentersTwoRegionsTwoZonesInfra := emptyInfra.DeepCopy()
	twoVCentersTwoRegionsTwoZonesInfra.Spec.PlatformSpec.VSphere.FailureDomains = []cfgv1.VSpherePlatformFailureDomainSpec{
		{
			Name:   "region1-zone1",
			Region: "region1",
			Zone:   "zone1",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter1",
				ComputeCluster: "computecluster1",
				Networks:       nil,
				Datastore:      "datastore1",
				ResourcePool:   "resourcepool1",
				Folder:         "folder1",
			},
		},
		{
			Name:   "region1-zone2",
			Region: "region1",
			Zone:   "zone2",
			Server: "vcenter1",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter1",
				ComputeCluster: "computecluster1",
				Networks:       nil,
				Datastore:      "datastore2",
				ResourcePool:   "resourcepool2",
				Folder:         "folder2",
			},
		},
		{
			Name:   "region2-zone1",
			Region: "region2",
			Zone:   "zone1",
			Server: "vcenter2",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter2",
				ComputeCluster: "computecluster2",
				Networks:       nil,
				Datastore:      "datastore1",
				ResourcePool:   "resourcepool1",
				Folder:         "folder1",
			},
		},
		{
			Name:   "region2-zone2",
			Region: "region2",
			Zone:   "zone2",
			Server: "vcenter2",
			Topology: cfgv1.VSpherePlatformTopology{
				Datacenter:     "datacenter2",
				ComputeCluster: "computecluster2",
				Networks:       nil,
				Datastore:      "datastore2",
				ResourcePool:   "resourcepool2",
				Folder:         "folder2",
			},
		},
	}

	tests := []struct {
		name             string
		infra            *cfgv1.Infrastructure
		clusterCSIDriver *opv1.ClusterCSIDriver
		expectedMetrics  string
	}{
		{
			name:             "empty topology",
			infra:            emptyInfra,
			clusterCSIDriver: emptyClusterCSIDriver,
			expectedMetrics: `
# HELP vsphere_infrastructure_failure_domains [ALPHA] Number of vSphere failure domains
# TYPE vsphere_infrastructure_failure_domains gauge
vsphere_infrastructure_failure_domains{scope="datacenters"} 0
vsphere_infrastructure_failure_domains{scope="datastores"} 0
vsphere_infrastructure_failure_domains{scope="failure_domains"} 0
vsphere_infrastructure_failure_domains{scope="regions"} 0
vsphere_infrastructure_failure_domains{scope="vcenters"} 0
vsphere_infrastructure_failure_domains{scope="zones"} 0
# HELP vsphere_topology_tags [ALPHA] Number of vSphere topology tags
# TYPE vsphere_topology_tags gauge
vsphere_topology_tags{source="clustercsidriver"} 0
vsphere_topology_tags{source="infrastructure"} 0
`,
		},
		{
			name:             "nil topology",
			infra:            nilInfra,
			clusterCSIDriver: nilClusterCSIDriver,
			expectedMetrics: `
# HELP vsphere_infrastructure_failure_domains [ALPHA] Number of vSphere failure domains
# TYPE vsphere_infrastructure_failure_domains gauge
vsphere_infrastructure_failure_domains{scope="datacenters"} 0
vsphere_infrastructure_failure_domains{scope="datastores"} 0
vsphere_infrastructure_failure_domains{scope="failure_domains"} 0
vsphere_infrastructure_failure_domains{scope="regions"} 0
vsphere_infrastructure_failure_domains{scope="vcenters"} 0
vsphere_infrastructure_failure_domains{scope="zones"} 0
# HELP vsphere_topology_tags [ALPHA] Number of vSphere topology tags
# TYPE vsphere_topology_tags gauge
vsphere_topology_tags{source="clustercsidriver"} 0
vsphere_topology_tags{source="infrastructure"} 0
`,
		},
		{
			name:             "clustercsidriver topology",
			infra:            emptyInfra,
			clusterCSIDriver: twoCategoriesClusterCSIDriver,
			expectedMetrics: `
# HELP vsphere_infrastructure_failure_domains [ALPHA] Number of vSphere failure domains
# TYPE vsphere_infrastructure_failure_domains gauge
vsphere_infrastructure_failure_domains{scope="datacenters"} 0
vsphere_infrastructure_failure_domains{scope="datastores"} 0
vsphere_infrastructure_failure_domains{scope="failure_domains"} 0
vsphere_infrastructure_failure_domains{scope="regions"} 0
vsphere_infrastructure_failure_domains{scope="vcenters"} 0
vsphere_infrastructure_failure_domains{scope="zones"} 0
# HELP vsphere_topology_tags [ALPHA] Number of vSphere topology tags
# TYPE vsphere_topology_tags gauge
vsphere_topology_tags{source="clustercsidriver"} 2
vsphere_topology_tags{source="infrastructure"} 0
`,
		},
		{
			name:             "infra topology 1 region 2 zones",
			infra:            oneRegionTwoZonesInfra,
			clusterCSIDriver: emptyClusterCSIDriver,
			expectedMetrics: `
# HELP vsphere_infrastructure_failure_domains [ALPHA] Number of vSphere failure domains
# TYPE vsphere_infrastructure_failure_domains gauge
vsphere_infrastructure_failure_domains{scope="datacenters"} 1
vsphere_infrastructure_failure_domains{scope="datastores"} 2
vsphere_infrastructure_failure_domains{scope="failure_domains"} 2
vsphere_infrastructure_failure_domains{scope="regions"} 1
vsphere_infrastructure_failure_domains{scope="vcenters"} 1
vsphere_infrastructure_failure_domains{scope="zones"} 2
# HELP vsphere_topology_tags [ALPHA] Number of vSphere topology tags
# TYPE vsphere_topology_tags gauge
vsphere_topology_tags{source="clustercsidriver"} 0
vsphere_topology_tags{source="infrastructure"} 2
`,
		},
		{
			name:             "infra topology 2 regions 2 zones each",
			infra:            twoRegionsTwoZonesInfra,
			clusterCSIDriver: emptyClusterCSIDriver,
			expectedMetrics: `
# HELP vsphere_infrastructure_failure_domains [ALPHA] Number of vSphere failure domains
# TYPE vsphere_infrastructure_failure_domains gauge
vsphere_infrastructure_failure_domains{scope="datacenters"} 2
vsphere_infrastructure_failure_domains{scope="datastores"} 4
vsphere_infrastructure_failure_domains{scope="failure_domains"} 4
vsphere_infrastructure_failure_domains{scope="regions"} 2
vsphere_infrastructure_failure_domains{scope="vcenters"} 1
vsphere_infrastructure_failure_domains{scope="zones"} 4
# HELP vsphere_topology_tags [ALPHA] Number of vSphere topology tags
# TYPE vsphere_topology_tags gauge
vsphere_topology_tags{source="clustercsidriver"} 0
vsphere_topology_tags{source="infrastructure"} 2
`,
		},
		{
			name:             "infra topology 2 vCenters",
			infra:            twoVCentersTwoRegionsTwoZonesInfra,
			clusterCSIDriver: emptyClusterCSIDriver,
			expectedMetrics: `
# HELP vsphere_infrastructure_failure_domains [ALPHA] Number of vSphere failure domains
# TYPE vsphere_infrastructure_failure_domains gauge
vsphere_infrastructure_failure_domains{scope="datacenters"} 2
vsphere_infrastructure_failure_domains{scope="datastores"} 4
vsphere_infrastructure_failure_domains{scope="failure_domains"} 4
vsphere_infrastructure_failure_domains{scope="regions"} 2
vsphere_infrastructure_failure_domains{scope="vcenters"} 2
vsphere_infrastructure_failure_domains{scope="zones"} 4
# HELP vsphere_topology_tags [ALPHA] Number of vSphere topology tags
# TYPE vsphere_topology_tags gauge
vsphere_topology_tags{source="clustercsidriver"} 0
vsphere_topology_tags{source="infrastructure"} 2
`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Reset metrics from previous tests. Note: the tests can't run in parallel!
			legacyregistry.Reset()

			UpdateMetrics(test.infra, test.clusterCSIDriver)
			if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(test.expectedMetrics), "vsphere_topology_tags", "vsphere_infrastructure_failure_domains"); err != nil {
				t.Errorf("Unexpected metric: %s", err)
			}
		})
	}
}
