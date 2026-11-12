package vclib

import (
	"testing"
)

const testLegacyINIConfig = `
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
`

const testYAMLConfig = `
global:
  secretName: vsphere-creds
  secretNamespace: kube-system
vcenter:
  vcenter.dc0.lan:
    server: vcenter.dc0.lan
    datacenters:
    - DC0
`

func mustLoadConfig(t *testing.T, data string) *VSphereConfig {
	t.Helper()
	cfg := &VSphereConfig{}
	if err := cfg.LoadConfig(data); err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	return cfg
}

// TestLoadConfig_LegacyConfigDetection pins the assumption OCPBUGS-140533 broke: LegacyConfig is only populated for
// ini-format cloud-provider-config, never for yaml. Code that gates behavior on LegacyConfig != nil must not assume
// it covers all clusters.
func TestLoadConfig_LegacyConfigDetection(t *testing.T) {
	iniCfg := mustLoadConfig(t, testLegacyINIConfig)
	if iniCfg.LegacyConfig == nil {
		t.Error("expected LegacyConfig to be populated for ini-format config")
	}
	if iniCfg.Config == nil {
		t.Error("expected Config to be populated for ini-format config")
	}

	yamlCfg := mustLoadConfig(t, testYAMLConfig)
	if yamlCfg.LegacyConfig != nil {
		t.Error("expected LegacyConfig to remain nil for yaml-format config")
	}
	if yamlCfg.Config == nil {
		t.Fatal("expected Config to be populated for yaml-format config")
	}
	if len(yamlCfg.Config.VirtualCenter) != 1 {
		t.Errorf("expected 1 vCenter in common Config for yaml-format config, got %d", len(yamlCfg.Config.VirtualCenter))
	}
}

func TestGetVCenterHostname(t *testing.T) {
	tests := []struct {
		name         string
		config       string
		vcenter      string
		expectedHost string
		expectError  bool
	}{
		{name: "known vcenter, ini config", config: testLegacyINIConfig, vcenter: "vcenter.dc0.lan", expectedHost: "vcenter.dc0.lan"},
		{name: "known vcenter, yaml config", config: testYAMLConfig, vcenter: "vcenter.dc0.lan", expectedHost: "vcenter.dc0.lan"},
		{name: "unknown vcenter, ini config", config: testLegacyINIConfig, vcenter: "does.not.exist", expectError: true},
		{name: "unknown vcenter, yaml config", config: testYAMLConfig, vcenter: "does.not.exist", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustLoadConfig(t, tt.config)
			host, err := cfg.GetVCenterHostname(tt.vcenter)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error for vcenter %q, got none", tt.vcenter)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.expectedHost {
				t.Errorf("expected hostname %q, got %q", tt.expectedHost, host)
			}
		})
	}
}

func TestIsInsecure(t *testing.T) {
	tests := []struct {
		name        string
		config      string
		vcenter     string
		expected    bool
		expectError bool
	}{
		{name: "known vcenter, ini config", config: testLegacyINIConfig, vcenter: "vcenter.dc0.lan", expected: false},
		{name: "known vcenter, yaml config", config: testYAMLConfig, vcenter: "vcenter.dc0.lan", expected: false},
		{name: "unknown vcenter, ini config", config: testLegacyINIConfig, vcenter: "does.not.exist", expectError: true},
		{name: "unknown vcenter, yaml config", config: testYAMLConfig, vcenter: "does.not.exist", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustLoadConfig(t, tt.config)
			insecure, err := cfg.IsInsecure(tt.vcenter)
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error for vcenter %q, got none", tt.vcenter)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if insecure != tt.expected {
				t.Errorf("expected insecure=%v, got %v", tt.expected, insecure)
			}
		})
	}
}

// TestNewVSphereConnection_UnknownVCenter is a regression test for the panic fixed in "fix panic when vcenter
// address is incorrect": NewVSphereConnection must return an error, not panic, when asked to connect to a vCenter
// that isn't present in the loaded config (e.g. Infrastructure and cloud-provider-config disagree on vCenter names).
func TestNewVSphereConnection_UnknownVCenter(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
	}{
		{name: "ini config", config: testLegacyINIConfig},
		{name: "yaml config", config: testYAMLConfig},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustLoadConfig(t, tt.config)
			conn, err := NewVSphereConnection("user", "pass", "does.not.exist", cfg)
			if err == nil {
				t.Fatal("expected error for unknown vcenter, got none")
			}
			if conn != nil {
				t.Errorf("expected nil connection on error, got %+v", conn)
			}
		})
	}
}

func TestNewVSphereConnection_KnownVCenter(t *testing.T) {
	cfg := mustLoadConfig(t, testYAMLConfig)
	conn, err := NewVSphereConnection("user", "pass", "vcenter.dc0.lan", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn.Hostname != "vcenter.dc0.lan" {
		t.Errorf("expected hostname %q, got %q", "vcenter.dc0.lan", conn.Hostname)
	}
}

func TestGetDatacenters(t *testing.T) {
	t.Run("yaml config uses common VirtualCenter map", func(t *testing.T) {
		cfg := mustLoadConfig(t, testYAMLConfig)
		dcs, err := cfg.GetDatacenters("vcenter.dc0.lan")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(dcs) != 1 || dcs[0] != "DC0" {
			t.Errorf("expected [DC0], got %v", dcs)
		}
	})

	t.Run("ini config uses common VirtualCenter map", func(t *testing.T) {
		cfg := mustLoadConfig(t, testLegacyINIConfig)
		dcs, err := cfg.GetDatacenters("vcenter.dc0.lan")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(dcs) != 1 || dcs[0] != "DC0" {
			t.Errorf("expected [DC0], got %v", dcs)
		}
	})

	t.Run("unknown vcenter falls back to legacy workspace when available", func(t *testing.T) {
		cfg := mustLoadConfig(t, testLegacyINIConfig)
		dcs, err := cfg.GetDatacenters("does.not.exist")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(dcs) != 1 || dcs[0] != "DC0" {
			t.Errorf("expected [DC0] from legacy workspace fallback, got %v", dcs)
		}
	})

	t.Run("unknown vcenter with no legacy config errors instead of panicking", func(t *testing.T) {
		cfg := mustLoadConfig(t, testYAMLConfig)
		_, err := cfg.GetDatacenters("does.not.exist")
		if err == nil {
			t.Fatal("expected error for unknown vcenter with no legacy fallback, got none")
		}
	})
}

func TestGetWorkspaceDatacenter(t *testing.T) {
	t.Run("legacy config present", func(t *testing.T) {
		cfg := mustLoadConfig(t, testLegacyINIConfig)
		dc, err := cfg.GetWorkspaceDatacenter()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dc != "DC0" {
			t.Errorf("expected DC0, got %q", dc)
		}
	})

	t.Run("legacy config absent", func(t *testing.T) {
		cfg := mustLoadConfig(t, testYAMLConfig)
		if _, err := cfg.GetWorkspaceDatacenter(); err == nil {
			t.Fatal("expected error when legacy config is absent, got none")
		}
	})
}

func TestGetDefaultDatastore(t *testing.T) {
	t.Run("legacy config present", func(t *testing.T) {
		cfg := mustLoadConfig(t, testLegacyINIConfig)
		ds, err := cfg.GetDefaultDatastore()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ds != "LocalDS_0" {
			t.Errorf("expected LocalDS_0, got %q", ds)
		}
	})

	t.Run("legacy config absent", func(t *testing.T) {
		cfg := mustLoadConfig(t, testYAMLConfig)
		if _, err := cfg.GetDefaultDatastore(); err == nil {
			t.Fatal("expected error when legacy config is absent, got none")
		}
	})
}
