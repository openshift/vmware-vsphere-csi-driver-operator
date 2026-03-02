package checks

import (
	"context"
	"fmt"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsVSphereNode(t *testing.T) {
	tests := []struct {
		name          string
		providerID    string
		expectedError error
	}{
		{
			name:          "valid vSphere providerID",
			providerID:    "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectedError: nil,
		},
		{
			name:          "empty providerID",
			providerID:    "",
			expectedError: fmt.Errorf("node test-node is not a vSphere node: providerID is empty"),
		},
		{
			name:          "AWS providerID",
			providerID:    "aws:///us-east-1a/i-1234567890abcdef0",
			expectedError: fmt.Errorf("node test-node is not a vSphere node: providerID \"aws:///us-east-1a/i-1234567890abcdef0\" does not have the expected vSphere prefix \"vsphere://\""),
		},
		{
			name:          "GCE providerID",
			providerID:    "gce://my-project/us-central1-a/my-instance",
			expectedError: fmt.Errorf("node test-node is not a vSphere node: providerID \"gce://my-project/us-central1-a/my-instance\" does not have the expected vSphere prefix \"vsphere://\""),
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

			fg := featuregates.NewFeatureGate(nil, []configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv})
			err := isVSphereNode(node, fg)
			assert.Equalf(t, tt.expectedError, err, "providerID %q", tt.providerID)
		})
	}
}

func TestIsVSphereNode_FeatureGateVSphereMixedNodeEnv(t *testing.T) {
	featureGateEnabled := featuregates.NewFeatureGate(
		[]configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv},
		nil,
	)

	platformTypeErr := fmt.Errorf("node test-node is not a vSphere node: platform-type label is not set to vsphere")
	tests := []struct {
		name          string
		labels        map[string]string
		providerID    string
		expectedError error
	}{
		{
			name:          "platform-type=vsphere label, empty providerID",
			labels:        map[string]string{"platform-type": "vsphere"},
			providerID:    "",
			expectedError: nil,
		},
		{
			name:          "platform-type=vsphere label, vsphere providerID",
			labels:        map[string]string{"platform-type": "vsphere"},
			providerID:    "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectedError: nil,
		},
		{
			name:          "platform-type=vsphere label, non-vsphere providerID",
			labels:        map[string]string{"platform-type": "vsphere"},
			providerID:    "aws:///us-east-1a/i-1234567890abcdef0",
			expectedError: nil,
		},
		{
			name:          "no platform-type label, vsphere providerID",
			labels:        nil,
			providerID:    "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectedError: platformTypeErr,
		},
		{
			name:          "platform-type=other, vsphere providerID",
			labels:        map[string]string{"platform-type": "aws"},
			providerID:    "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectedError: platformTypeErr,
		},
		{
			name:          "empty platform-type, vsphere providerID",
			labels:        map[string]string{"platform-type": ""},
			providerID:    "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectedError: platformTypeErr,
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
			err := isVSphereNode(node, featureGateEnabled)
			assert.Equalf(t, tt.expectedError, err, "labels=%v providerID=%q", tt.labels, tt.providerID)
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

			assert.Equal(t, tt.expectedStatus, result.CheckStatus)
			if tt.expectError {
				assert.Error(t, result.CheckError)
				assert.NotEmpty(t, result.CheckError.Error(), "error message should not be empty")
				assert.Contains(t, result.CheckError.Error(), node.Name, "error message should contain node name")
			} else {
				assert.NoError(t, result.CheckError)
			}
			assert.Equal(t, tt.expectedAction, result.Action)
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
			assert.NotEqual(t, CheckStatusNonVSphereNode, result.CheckStatus,
				"valid vSphere providerID %q should not fail with CheckStatusNonVSphereNode", tt.providerID)
		})
	}
}

func TestNodeChecker_CheckOnNode_FeatureGateVSphereMixedNodeEnv(t *testing.T) {
	featureGateEnabled := featuregates.NewFeatureGate(
		[]configv1.FeatureGateName{features.FeatureGateVSphereMixedNodeEnv},
		nil,
	)

	platformTypeErr := fmt.Errorf("node test-node is not a vSphere node: platform-type label is not set to vsphere")
	tests := []struct {
		name                   string
		labels                 map[string]string
		providerID             string
		expectedError          error
		expectNonVSphereStatus bool
	}{
		{
			name:                   "platform-type=vsphere, empty providerID passes node check",
			labels:                 map[string]string{"platform-type": "vsphere"},
			providerID:             "",
			expectedError:          nil,
			expectNonVSphereStatus: false,
		},
		{
			name:                   "no platform-type label, vsphere providerID fails with gate on",
			labels:                 nil,
			providerID:             "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectedError:          platformTypeErr,
			expectNonVSphereStatus: true,
		},
		{
			name:                   "platform-type=aws, vsphere providerID fails with gate on",
			labels:                 map[string]string{"platform-type": "aws"},
			providerID:             "vsphere://42290e77-dc1d-10ef-380c-26ed0ab90cb9",
			expectedError:          platformTypeErr,
			expectNonVSphereStatus: true,
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
			err := isVSphereNode(node, featureGateEnabled)
			assert.Equalf(t, tt.expectedError, err, "labels=%v providerID=%q", tt.labels, tt.providerID)

			checker := &NodeChecker{}
			checkOpts := CheckArgs{featureGate: featureGateEnabled}
			workInfo := nodeChannelWorkData{
				checkOpts: checkOpts,
				node:      node,
				ctx:       context.TODO(),
			}

			result := checker.checkOnNode(workInfo)
			assert.Equalf(t, tt.expectNonVSphereStatus, result.CheckStatus == CheckStatusNonVSphereNode, "labels=%v providerID=%q", tt.labels, tt.providerID)
		})
	}
}
