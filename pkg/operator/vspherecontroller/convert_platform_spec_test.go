package vspherecontroller

import (
	"testing"

	ocpv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
)

const legacyINIConfig = `
[Global]
secret-name = "vsphere-creds"
secret-namespace = "kube-system"
insecure-flag = "1"

[Workspace]
server = "vcenter.dc0.lan"
datacenter = "DC0"
default-datastore = "LocalDS_0"
folder = "/DC0/vm"

[VirtualCenter "vcenter.dc0.lan"]
datacenters = "DC0"
`

const yamlConfig = `
global:
  insecureFlag: true
  secretName: vsphere-creds
  secretNamespace: kube-system
vcenter:
  vcenter.dc0.lan:
    server: vcenter.dc0.lan
    datacenters:
    - DC0
`

const twoVCenterLegacyINIConfig = `
[Global]
secret-name = "vsphere-creds"
secret-namespace = "kube-system"

[Workspace]
server = "vcenter.dc0.lan"
datacenter = "DC0"
default-datastore = "LocalDS_0"
folder = "/DC0/vm"

[VirtualCenter "vcenter.dc0.lan"]
datacenters = "DC0"

[VirtualCenter "vcenter.dc1.lan"]
datacenters = "DC1"
`

const twoVCenterYAMLConfig = `
global:
  secretName: vsphere-creds
  secretNamespace: kube-system
vcenter:
  vcenter.dc0.lan:
    server: vcenter.dc0.lan
    datacenters:
    - DC0
  vcenter.dc1.lan:
    server: vcenter.dc1.lan
    datacenters:
    - DC1
`

// tenantRefYAMLConfig uses a map key ("dc0-ref") that differs from the vCenter's actual server ("vcenter.dc0.lan"),
// which the yaml cloud-provider-config schema explicitly allows (TenantRef).
const tenantRefYAMLConfig = `
global:
  secretName: vsphere-creds
  secretNamespace: kube-system
vcenter:
  dc0-ref:
    server: vcenter.dc0.lan
    datacenters:
    - DC0
`

// twoTenantRefYAMLConfig adds a second vCenter, also keyed by a TenantRef distinct from its server, to the single
// vCenter already configured via tenantRefYAMLConfig.
const twoTenantRefYAMLConfig = `
global:
  secretName: vsphere-creds
  secretNamespace: kube-system
vcenter:
  dc0-ref:
    server: vcenter.dc0.lan
    datacenters:
    - DC0
  dc1-ref:
    server: vcenter.dc1.lan
    datacenters:
    - DC1
`

func loadVSphereConfig(t *testing.T, data string) *vclib.VSphereConfig {
	t.Helper()
	cfg := &vclib.VSphereConfig{}
	if err := cfg.LoadConfig(data); err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	return cfg
}

func emptyVSphereInfra() *ocpv1.Infrastructure {
	return &ocpv1.Infrastructure{
		Spec: ocpv1.InfrastructureSpec{
			PlatformSpec: ocpv1.PlatformSpec{
				Type:    ocpv1.VSpherePlatformType,
				VSphere: &ocpv1.VSpherePlatformSpec{},
			},
		},
	}
}

// TestConvertToPlatformSpec_MissingVCenters exercises OCPBUGS-140533: a cluster whose Infrastructure CR has no
// vCenters/failure domains must still have VCenters reconstructed from cloud-provider-config regardless of whether
// that config is rendered in the legacy ini format or the newer yaml format. FailureDomains is only reconstructed
// for legacy ini config, which has real per-vCenter topology (datacenter/datastore/folder/resourcePool) to draw
// from; yaml config has no such data, so a reconstructed FailureDomains entry would misrepresent a connectivity-only
// fallback as real, admin-configured zonal topology to code that branches on its presence.
func TestConvertToPlatformSpec_MissingVCenters(t *testing.T) {
	tests := []struct {
		name                string
		config              string
		expectFailureDomain bool
	}{
		{name: "legacy ini config", config: legacyINIConfig, expectFailureDomain: true},
		{name: "yaml config", config: yamlConfig, expectFailureDomain: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadVSphereConfig(t, tt.config)
			infra := emptyVSphereInfra()

			ConvertToPlatformSpec(cfg, infra)

			vSphereSpec := infra.Spec.PlatformSpec.VSphere
			if len(vSphereSpec.VCenters) != 1 {
				t.Fatalf("expected 1 vCenter to be reconstructed, got %d: %+v", len(vSphereSpec.VCenters), vSphereSpec.VCenters)
			}
			if vSphereSpec.VCenters[0].Server != "vcenter.dc0.lan" {
				t.Errorf("expected vCenter server %q, got %q", "vcenter.dc0.lan", vSphereSpec.VCenters[0].Server)
			}

			if !tt.expectFailureDomain {
				if len(vSphereSpec.FailureDomains) != 0 {
					t.Errorf("expected no failure domain to be reconstructed (no real topology data available), got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
				}
				return
			}

			if len(vSphereSpec.FailureDomains) != 1 {
				t.Fatalf("expected 1 failure domain to be reconstructed, got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
			}
			if vSphereSpec.FailureDomains[0].Server != "vcenter.dc0.lan" {
				t.Errorf("expected failure domain server %q, got %q", "vcenter.dc0.lan", vSphereSpec.FailureDomains[0].Server)
			}
			if vSphereSpec.FailureDomains[0].Topology.Datacenter != "DC0" {
				t.Errorf("expected failure domain datacenter %q, got %q", "DC0", vSphereSpec.FailureDomains[0].Topology.Datacenter)
			}
		})
	}
}

// TestConvertToPlatformSpec_AlreadyConfigured ensures the reconstruction logic is a no-op when Infrastructure
// already carries vCenters and failure domains, for both config formats.
func TestConvertToPlatformSpec_AlreadyConfigured(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "legacy ini config", config: legacyINIConfig},
		{name: "yaml config", config: yamlConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadVSphereConfig(t, tt.config)
			infra := emptyVSphereInfra()
			infra.Spec.PlatformSpec.VSphere.VCenters = []ocpv1.VSpherePlatformVCenterSpec{
				{Server: "vcenter.dc0.lan", Datacenters: []string{"DC0"}},
			}
			infra.Spec.PlatformSpec.VSphere.FailureDomains = []ocpv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "us-east-1",
					Region: "us-east",
					Zone:   "us-east-1a",
					Server: "vcenter.dc0.lan",
					Topology: ocpv1.VSpherePlatformTopology{
						Datacenter: "DC0",
					},
				},
			}

			ConvertToPlatformSpec(cfg, infra)

			vSphereSpec := infra.Spec.PlatformSpec.VSphere
			if len(vSphereSpec.VCenters) != 1 {
				t.Fatalf("expected vCenters to remain untouched (1 entry), got %d: %+v", len(vSphereSpec.VCenters), vSphereSpec.VCenters)
			}
			if len(vSphereSpec.FailureDomains) != 1 {
				t.Fatalf("expected failure domains to remain untouched (1 entry), got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
			}
		})
	}
}

// TestConvertToPlatformSpec_NilVSphereSpec is a regression test for "Fix panic on nil infrastructure
// Spec.PlatformSpec.VSphere": infra objects created by OCP 4.10 and earlier may have a nil VSphere platform spec,
// and ConvertToPlatformSpec must initialize it instead of panicking, for both config formats.
func TestConvertToPlatformSpec_NilVSphereSpec(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "legacy ini config", config: legacyINIConfig},
		{name: "yaml config", config: yamlConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadVSphereConfig(t, tt.config)
			infra := emptyVSphereInfra()
			infra.Spec.PlatformSpec.VSphere = nil

			ConvertToPlatformSpec(cfg, infra)

			if infra.Spec.PlatformSpec.VSphere == nil {
				t.Fatal("expected VSphere platform spec to be initialized, got nil")
			}
			if len(infra.Spec.PlatformSpec.VSphere.VCenters) != 1 {
				t.Errorf("expected 1 vCenter to be reconstructed, got %d", len(infra.Spec.PlatformSpec.VSphere.VCenters))
			}
		})
	}
}

// TestConvertToPlatformSpec_NilConfig ensures a nil cloud-provider config (e.g. it failed to load) never panics and
// leaves the platform spec untouched other than the nil-VSphere-spec initialization.
func TestConvertToPlatformSpec_NilConfig(t *testing.T) {
	infra := emptyVSphereInfra()
	infra.Spec.PlatformSpec.VSphere = nil

	ConvertToPlatformSpec(nil, infra)

	if infra.Spec.PlatformSpec.VSphere == nil {
		t.Fatal("expected VSphere platform spec to be initialized, got nil")
	}
	if len(infra.Spec.PlatformSpec.VSphere.VCenters) != 0 {
		t.Errorf("expected no vCenters to be reconstructed from a nil config, got %d", len(infra.Spec.PlatformSpec.VSphere.VCenters))
	}
}

// TestConvertToPlatformSpec_MultipleVCentersUnsupported documents the existing safety behavior: when Infrastructure
// has no vCenters of its own and cloud-provider-config declares more than one vCenter, ConvertToPlatformSpec cannot
// safely guess which one(s) belong in the platform spec, so it must leave it empty rather than guess or panic. This
// applies to both config formats.
func TestConvertToPlatformSpec_MultipleVCentersUnsupported(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "legacy ini config", config: twoVCenterLegacyINIConfig},
		{name: "yaml config", config: twoVCenterYAMLConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadVSphereConfig(t, tt.config)
			infra := emptyVSphereInfra()

			ConvertToPlatformSpec(cfg, infra)

			vSphereSpec := infra.Spec.PlatformSpec.VSphere
			if len(vSphereSpec.VCenters) != 0 {
				t.Errorf("expected no vCenters to be reconstructed for an ambiguous multi-vCenter config, got %d: %+v", len(vSphereSpec.VCenters), vSphereSpec.VCenters)
			}
			if len(vSphereSpec.FailureDomains) != 0 {
				t.Errorf("expected no failure domains to be reconstructed for an ambiguous multi-vCenter config, got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
			}
		})
	}
}

// TestConvertToPlatformSpec_AddMissingVCenter covers the merge path: Infrastructure already has one vCenter
// configured, but cloud-provider-config lists a second one that Infrastructure doesn't know about yet (e.g. a
// vCenter added via multi-vCenter day-2 support). The missing vCenter must be appended without disturbing the
// existing failure domains, for both config formats.
func TestConvertToPlatformSpec_AddMissingVCenter(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "legacy ini config", config: twoVCenterLegacyINIConfig},
		{name: "yaml config", config: twoVCenterYAMLConfig},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadVSphereConfig(t, tt.config)
			infra := emptyVSphereInfra()
			infra.Spec.PlatformSpec.VSphere.VCenters = []ocpv1.VSpherePlatformVCenterSpec{
				{Server: "vcenter.dc0.lan", Datacenters: []string{"DC0"}},
			}
			infra.Spec.PlatformSpec.VSphere.FailureDomains = []ocpv1.VSpherePlatformFailureDomainSpec{
				{
					Name:   "us-east-1",
					Region: "us-east",
					Zone:   "us-east-1a",
					Server: "vcenter.dc0.lan",
					Topology: ocpv1.VSpherePlatformTopology{
						Datacenter: "DC0",
					},
				},
			}

			ConvertToPlatformSpec(cfg, infra)

			vSphereSpec := infra.Spec.PlatformSpec.VSphere
			if len(vSphereSpec.VCenters) != 2 {
				t.Fatalf("expected the missing vCenter to be appended (2 entries total), got %d: %+v", len(vSphereSpec.VCenters), vSphereSpec.VCenters)
			}
			found := false
			for _, vc := range vSphereSpec.VCenters {
				if vc.Server == "vcenter.dc1.lan" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected vcenter.dc1.lan to be added to VCenters, got %+v", vSphereSpec.VCenters)
			}

			// Existing failure domains must be left untouched since they were already non-empty.
			if len(vSphereSpec.FailureDomains) != 1 {
				t.Fatalf("expected failure domains to remain untouched (1 entry), got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
			}
		})
	}
}

// TestConvertToPlatformSpec_FailureDomainFailsafe covers the case where Infrastructure already has its vCenters
// configured (so the top-level merge branch runs, not the full single-vCenter reconstruction), but is missing
// FailureDomains entirely -- treated as invalid config that gets a best-effort failsafe failure domain, but only for
// legacy ini config, which has real per-vCenter topology to draw the failsafe from; yaml config does not.
func TestConvertToPlatformSpec_FailureDomainFailsafe(t *testing.T) {
	tests := []struct {
		name                string
		config              string
		expectFailureDomain bool
	}{
		{name: "legacy ini config", config: legacyINIConfig, expectFailureDomain: true},
		{name: "yaml config", config: yamlConfig, expectFailureDomain: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadVSphereConfig(t, tt.config)
			infra := emptyVSphereInfra()
			infra.Spec.PlatformSpec.VSphere.VCenters = []ocpv1.VSpherePlatformVCenterSpec{
				{Server: "vcenter.dc0.lan", Datacenters: []string{"DC0"}},
			}

			ConvertToPlatformSpec(cfg, infra)

			vSphereSpec := infra.Spec.PlatformSpec.VSphere
			if len(vSphereSpec.VCenters) != 1 {
				t.Fatalf("expected VCenters to remain untouched (1 entry), got %d: %+v", len(vSphereSpec.VCenters), vSphereSpec.VCenters)
			}

			if !tt.expectFailureDomain {
				if len(vSphereSpec.FailureDomains) != 0 {
					t.Errorf("expected no failsafe failure domain (no real topology data available), got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
				}
				return
			}

			if len(vSphereSpec.FailureDomains) != 1 {
				t.Fatalf("expected a failsafe failure domain to be added, got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
			}
			if vSphereSpec.FailureDomains[0].Server != "vcenter.dc0.lan" {
				t.Errorf("expected failsafe failure domain server %q, got %q", "vcenter.dc0.lan", vSphereSpec.FailureDomains[0].Server)
			}
			if vSphereSpec.FailureDomains[0].Topology.Datacenter != "DC0" {
				t.Errorf("expected failsafe failure domain datacenter %q, got %q", "DC0", vSphereSpec.FailureDomains[0].Topology.Datacenter)
			}
		})
	}
}

// TestConvertToPlatformSpec_TenantRefDiffersFromServer is a regression test for a review finding: yaml
// cloud-provider-config permits a vCenter's map key (TenantRef) to differ from its actual server address. The
// reconstructed VCenters must store the real server address in .Server (matching the OpenShift API contract for
// that field), not the TenantRef map key used to look up the entry in config. FailureDomains is never reconstructed
// for yaml config regardless, since it carries no topology data.
func TestConvertToPlatformSpec_TenantRefDiffersFromServer(t *testing.T) {
	cfg := loadVSphereConfig(t, tenantRefYAMLConfig)
	infra := emptyVSphereInfra()

	ConvertToPlatformSpec(cfg, infra)

	vSphereSpec := infra.Spec.PlatformSpec.VSphere
	if len(vSphereSpec.VCenters) != 1 {
		t.Fatalf("expected 1 vCenter to be reconstructed, got %d: %+v", len(vSphereSpec.VCenters), vSphereSpec.VCenters)
	}
	if vSphereSpec.VCenters[0].Server != "vcenter.dc0.lan" {
		t.Errorf("expected vCenter server %q (not the TenantRef map key), got %q", "vcenter.dc0.lan", vSphereSpec.VCenters[0].Server)
	}

	if len(vSphereSpec.FailureDomains) != 0 {
		t.Errorf("expected no failure domain to be reconstructed for yaml config, got %d: %+v", len(vSphereSpec.FailureDomains), vSphereSpec.FailureDomains)
	}
}

// TestConvertToPlatformSpec_AddMissingVCenter_TenantRefDiffersFromServer covers the merge path with a TenantRef:
// the newly-added vCenter must be looked up by its map key but stored under its real server address.
func TestConvertToPlatformSpec_AddMissingVCenter_TenantRefDiffersFromServer(t *testing.T) {
	cfg := loadVSphereConfig(t, twoTenantRefYAMLConfig)
	infra := emptyVSphereInfra()
	infra.Spec.PlatformSpec.VSphere.VCenters = []ocpv1.VSpherePlatformVCenterSpec{
		{Server: "vcenter.dc0.lan", Datacenters: []string{"DC0"}},
	}
	infra.Spec.PlatformSpec.VSphere.FailureDomains = []ocpv1.VSpherePlatformFailureDomainSpec{
		{Server: "vcenter.dc0.lan", Topology: ocpv1.VSpherePlatformTopology{Datacenter: "DC0"}},
	}

	ConvertToPlatformSpec(cfg, infra)

	vSphereSpec := infra.Spec.PlatformSpec.VSphere
	if len(vSphereSpec.VCenters) != 2 {
		t.Fatalf("expected the missing vCenter to be appended (2 entries total), got %d: %+v", len(vSphereSpec.VCenters), vSphereSpec.VCenters)
	}
	found := false
	for _, vc := range vSphereSpec.VCenters {
		if vc.Server == "vcenter.dc1.lan" {
			found = true
		}
		if vc.Server == "dc1-ref" {
			t.Errorf("vCenter server was set to the TenantRef map key instead of the real server: %+v", vc)
		}
	}
	if !found {
		t.Errorf("expected vcenter.dc1.lan to be added to VCenters, got %+v", vSphereSpec.VCenters)
	}
}

// TestConvertToPlatformSpec_AmbiguousFailureDomainFailsafe covers a multi-vCenter yaml config specifically: even
// setting aside that yaml config never gets a reconstructed failure domain, picking one of several vCenters at
// random (Go map iteration order) to build one from would have been doubly wrong here.
func TestConvertToPlatformSpec_AmbiguousFailureDomainFailsafe(t *testing.T) {
	cfg := loadVSphereConfig(t, twoVCenterYAMLConfig)
	infra := emptyVSphereInfra()
	infra.Spec.PlatformSpec.VSphere.VCenters = []ocpv1.VSpherePlatformVCenterSpec{
		{Server: "vcenter.dc0.lan", Datacenters: []string{"DC0"}},
	}

	ConvertToPlatformSpec(cfg, infra)

	vSphereSpec := infra.Spec.PlatformSpec.VSphere
	if len(vSphereSpec.FailureDomains) != 0 {
		t.Errorf("expected no failsafe failure domain to be guessed from an ambiguous multi-vCenter yaml config, got %+v", vSphereSpec.FailureDomains)
	}
}
