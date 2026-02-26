package checks

import (
	"context"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsVSphereNode(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		expected   bool
	}{
		{
			name:       "valid vSphere providerID",
			providerID: "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expected:   true,
		},
		{
			name:       "empty providerID",
			providerID: "",
			expected:   false,
		},
		{
			name:       "AWS providerID",
			providerID: "aws:///us-east-1a/i-1234567890abcdef0",
			expected:   false,
		},
		{
			name:       "GCE providerID",
			providerID: "gce://my-project/us-central1-a/my-instance",
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{
				Spec: v1.NodeSpec{
					ProviderID: tt.providerID,
				},
			}

			fg := featuregates.NewFeatureGate(nil, []configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv})
			result := isVSphereNode(node, fg)
			if result != tt.expected {
				t.Errorf("isVSphereNode() = %v, want %v for providerID %q", result, tt.expected, tt.providerID)
			}
		})
	}
}

func TestIsVSphereNode_FeatureGateVSphereMixedNodeEnv(t *testing.T) {
	featureGateEnabled := featuregates.NewFeatureGate(
		[]configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv},
		nil,
	)

	tests := []struct {
		name                  string
		labels                map[string]string
		providerID            string
		isVSphereNodeExpected bool
	}{
		{
			name:                  "platform-type=vsphere label, empty providerID",
			labels:                map[string]string{"platform-type": "vsphere"},
			providerID:            "",
			isVSphereNodeExpected: true,
		},
		{
			name:                  "platform-type=vsphere label, vsphere providerID",
			labels:                map[string]string{"platform-type": "vsphere"},
			providerID:            "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			isVSphereNodeExpected: true,
		},
		{
			name:                  "platform-type=vsphere label, non-vsphere providerID",
			labels:                map[string]string{"platform-type": "vsphere"},
			providerID:            "aws:///us-east-1a/i-1234567890abcdef0",
			isVSphereNodeExpected: true,
		},
		{
			name:                  "no platform-type label, vsphere providerID",
			labels:                nil,
			providerID:            "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			isVSphereNodeExpected: false,
		},
		{
			name:                  "platform-type=other, vsphere providerID",
			labels:                map[string]string{"platform-type": "aws"},
			providerID:            "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			isVSphereNodeExpected: false,
		},
		{
			name:                  "empty platform-type, vsphere providerID",
			labels:                map[string]string{"platform-type": ""},
			providerID:            "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			isVSphereNodeExpected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Labels: tt.labels,
				},
				Spec: v1.NodeSpec{
					ProviderID: tt.providerID,
				},
			}

			result := isVSphereNode(node, featureGateEnabled)
			if result != tt.isVSphereNodeExpected {
				t.Errorf("isVSphereNode() = %v, want %v (labels=%v providerID=%q)",
					result, tt.isVSphereNodeExpected, tt.labels, tt.providerID)
			}
		})
	}
}

func TestNodeChecker_CheckOnNode_ProviderIDValidation(t *testing.T) {
	tests := []struct {
		name           string
		providerID     string
		expectedStatus CheckStatusType
		expectError    bool
		expectedAction CheckAction
	}{
		{
			name:           "empty providerID",
			providerID:     "",
			expectedStatus: CheckStatusNonVSphereNode,
			expectError:    true,
			expectedAction: CheckActionDegrade,
		},
		{
			name:           "azure providerID",
			providerID:     "azure:///subscriptions/sub-id/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-name",
			expectedStatus: CheckStatusNonVSphereNode,
			expectError:    true,
			expectedAction: CheckActionDegrade,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
				},
				Spec: v1.NodeSpec{
					ProviderID: tt.providerID,
				},
			}

			checker := &NodeChecker{}
			fg := featuregates.NewFeatureGate(nil, []configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv})
			checkOpts := CheckArgs{
				featureGate: fg,
			}

			workInfo := nodeChannelWorkData{
				checkOpts: checkOpts,
				node:      node,
				ctx:       context.TODO(),
			}

			result := checker.checkOnNode(workInfo)

			if result.CheckStatus != tt.expectedStatus {
				t.Errorf("expected status %s, got %s", tt.expectedStatus, result.CheckStatus)
			}

			if tt.expectError && result.CheckError == nil {
				t.Errorf("expected an error but got none")
			}

			if !tt.expectError && result.CheckError != nil {
				t.Errorf("expected no error but got: %v", result.CheckError)
			}

			if result.Action != tt.expectedAction {
				t.Errorf("expected action %s, got %s", ActionToString(tt.expectedAction), ActionToString(result.Action))
			}

			// Verify the error message contains useful information
			if tt.expectError {
				errorMsg := result.CheckError.Error()
				if errorMsg == "" {
					t.Errorf("error message should not be empty")
				}
				// Check that error message mentions the node name
				if len(errorMsg) > 0 && !strings.Contains(errorMsg, node.Name) {
					t.Logf("Warning: error message does not contain node name: %s", errorMsg)
				}
			}
		})
	}
}

func TestNodeChecker_CheckOnNode_ValidVSphereProviderID(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
	}{
		{
			name:       "valid vSphere providerID",
			providerID: "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
		},
		{
			name:       "valid vSphere providerID with different UUID",
			providerID: "vsphere://12345678-1234-1234-1234-123456789abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
				},
				Spec: v1.NodeSpec{
					ProviderID: tt.providerID,
				},
			}

			checker := &NodeChecker{}
			fg := featuregates.NewFeatureGate(nil, []configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv})
			checkOpts := CheckArgs{
				featureGate: fg,
				// Note: vmConnection is nil, so the test will fail when trying to get VM
				// but that's expected - we're only testing that the providerID validation passes
			}

			workInfo := nodeChannelWorkData{
				checkOpts: checkOpts,
				node:      node,
				ctx:       context.TODO(),
			}

			result := checker.checkOnNode(workInfo)

			// The check should pass the providerID validation but fail on VM lookup
			// since we don't have a connection. The important thing is that it doesn't
			// fail with CheckStatusNonVSphereNode
			if result.CheckStatus == CheckStatusNonVSphereNode {
				t.Errorf("valid vSphere providerID %q should not fail with CheckStatusNonVSphereNode", tt.providerID)
			}
		})
	}
}

func TestNodeChecker_CheckOnNode_FeatureGateVSphereMixedNodeEnv(t *testing.T) {
	featureGateEnabled := featuregates.NewFeatureGate(
		[]configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv},
		nil,
	)

	tests := []struct {
		name             string
		labels           map[string]string
		providerID       string
		expectNonVSphere bool
	}{
		{
			name:             "platform-type=vsphere, empty providerID passes node check",
			labels:           map[string]string{"platform-type": "vsphere"},
			providerID:       "",
			expectNonVSphere: false,
		},
		{
			name:             "no platform-type label, vsphere providerID fails with gate on",
			labels:           nil,
			providerID:       "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectNonVSphere: true,
		},
		{
			name:             "platform-type=aws, vsphere providerID fails with gate on",
			labels:           map[string]string{"platform-type": "aws"},
			providerID:       "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectNonVSphere: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node",
					Labels: tt.labels,
				},
				Spec: v1.NodeSpec{
					ProviderID: tt.providerID,
				},
			}

			checker := &NodeChecker{}
			checkOpts := CheckArgs{featureGate: featureGateEnabled}
			workInfo := nodeChannelWorkData{
				checkOpts: checkOpts,
				node:      node,
				ctx:       context.TODO(),
			}

			result := checker.checkOnNode(workInfo)

			if result.CheckStatus == CheckStatusNonVSphereNode && !tt.expectNonVSphere {
				t.Errorf("unexpected CheckStatusNonVSphereNode (labels=%v providerID=%q)",
					tt.labels, tt.providerID)
			}
			if result.CheckStatus != CheckStatusNonVSphereNode && tt.expectNonVSphere {
				t.Errorf("expected CheckStatusNonVSphereNode, got %s (labels=%v providerID=%q)",
					result.CheckStatus, tt.labels, tt.providerID)
			}
		})
	}
}
