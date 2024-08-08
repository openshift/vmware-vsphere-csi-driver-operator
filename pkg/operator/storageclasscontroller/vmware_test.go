package storageclasscontroller

import (
	"context"
	"fmt"
	v1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"testing"
)

func TestCreateStoragePolicyOnce(t *testing.T) {
	var cleanUpFunc func()
	var connections []*vclib.VSphereConnection
	var connError error

	infra := testlib.GetInfraObject()

	connections, cleanUpFunc, connError = testlib.SetupSimulator(testlib.DefaultModel, infra)
	defer func() {
		if cleanUpFunc != nil {
			cleanUpFunc()
		}
	}()

	if connError != nil {
		t.Fatalf("error connecting to vcenter: %v", connError)
	}

	for _, conn := range connections {
		storagePolicyAPIClient := &storagePolicyAPI{
			vcenterApiConnection: conn,
			infra:                infra,
			categoryName:         fmt.Sprintf(categoryNameTemplate, infra.Status.InfrastructureName),
			policyName:           fmt.Sprintf(policyNameTemplate, infra.Status.InfrastructureName),
			tagName:              infra.Status.InfrastructureName,
			apiTestInfo:          map[string]int{},
		}

		_, err := storagePolicyAPIClient.createStoragePolicy(context.TODO())
		if err != nil {
			t.Fatalf("Error creating storage policy: %v", err)
		}

		// We should delete the created storage policy, because simulator seems to be caching it
		defer func() {
			err := storagePolicyAPIClient.deleteStoragePolicy(context.TODO())
			if err != nil {
				t.Errorf("error deleting storage policy: %v", err)
			}
		}()

		found, err := storagePolicyAPIClient.checkForExistingPolicy(context.TODO())
		if err != nil {
			t.Fatalf("error while trying to find storage policy: %v", err)
		}
		if !found {
			t.Errorf("expected policy to be created found none")
		}

		validateAPICallCount(t, storagePolicyAPIClient, map[string]int{
			create_tag_api:      1,
			create_category_api: 1,
			attach_tag_api:      1,
			create_profile_api:  1,
		})
	}

}

func TestDuplicatePolicyCreation(t *testing.T) {
	var cleanUpFunc func()
	var connections []*vclib.VSphereConnection
	var connError error

	infra := testlib.GetInfraObject()

	connections, cleanUpFunc, connError = testlib.SetupSimulator(testlib.DefaultModel, infra)
	defer func() {
		if cleanUpFunc != nil {
			cleanUpFunc()
		}
	}()

	if connError != nil {
		t.Fatalf("error connecting to vcenter: %v", connError)
	}

	for _, conn := range connections {
		storagePolicyAPIClient := &storagePolicyAPI{
			vcenterApiConnection: conn,
			infra:                infra,
			categoryName:         fmt.Sprintf(categoryNameTemplate, infra.Status.InfrastructureName),
			policyName:           fmt.Sprintf(policyNameTemplate, infra.Status.InfrastructureName),
			tagName:              infra.Status.InfrastructureName,
			apiTestInfo:          map[string]int{},
		}

		_, err := storagePolicyAPIClient.createStoragePolicy(context.TODO())
		if err != nil {
			t.Fatalf("Error creating storage policy: %v", err)
		}

		// We should delete the created storage policy, because simulator seems to be caching it
		defer func() {
			err := storagePolicyAPIClient.deleteStoragePolicy(context.TODO())
			if err != nil {
				t.Errorf("error deleting storage policy: %v", err)
			}
		}()

		// Now lets create same storage policy again
		_, err = storagePolicyAPIClient.createStoragePolicy(context.TODO())
		if err != nil {
			t.Fatalf("Error creating storage policy: %v", err)
		}

		found, err := storagePolicyAPIClient.checkForExistingPolicy(context.TODO())
		if err != nil {
			t.Fatalf("error while trying to find storage policy: %v", err)
		}
		if !found {
			t.Errorf("expected policy to be created found none")
		}
		// calling createStoragePolicy should not result in multiple calls to create tags and categories
		validateAPICallCount(t, storagePolicyAPIClient, map[string]int{
			create_tag_api:      1,
			create_category_api: 1,
			attach_tag_api:      1,
			create_profile_api:  1,
		})
	}
}

func TestZonalPolicyCreation(t *testing.T) {

	tests := []struct {
		name                 string
		infra                *v1.Infrastructure
		expectedApiCallCount map[string]int
		featureGates         featuregates.FeatureGate
	}{
		{
			name:  "when there multiple failure domains exist",
			infra: testlib.GetZonalInfra(),
			expectedApiCallCount: map[string]int{
				create_tag_api:      1,
				create_category_api: 1,
				update_category_api: 1,
				attach_tag_api:      2,
				create_profile_api:  1,
			},
			featureGates: featuregates.NewFeatureGate([]v1.FeatureGateName{"SomeEnabledFeatureGate"}, []v1.FeatureGateName{"SomeDisabledFeatureGate", features.FeatureGateVSphereMultiVCenters}),
		},
		{
			name:  "when one failure domain exists",
			infra: testlib.GetSingleFailureDomainInfra(),
			expectedApiCallCount: map[string]int{
				create_tag_api:      1,
				create_category_api: 1,
				attach_tag_api:      1,
				create_profile_api:  1,
			},
			featureGates: featuregates.NewFeatureGate([]v1.FeatureGateName{"SomeEnabledFeatureGate"}, []v1.FeatureGateName{"SomeDisabledFeatureGate", features.FeatureGateVSphereMultiVCenters}),
		},
		{
			name:  "when multiple failure domain exists across multiple vcenters",
			infra: testlib.GetZonalMultiVCenterInfra(),
			expectedApiCallCount: map[string]int{
				create_tag_api:      1,
				create_category_api: 1,
				attach_tag_api:      1,
				create_profile_api:  1,
			},
			featureGates: featuregates.NewFeatureGate([]v1.FeatureGateName{"SomeEnabledFeatureGate"}, []v1.FeatureGateName{"SomeDisabledFeatureGate", features.FeatureGateVSphereMultiVCenters}),
		},
	}

	for _, test := range tests {
		tc := test
		t.Run(tc.name, func(t *testing.T) {
			var cleanUpFunc func()
			var connections []*vclib.VSphereConnection
			var connError error

			infra := tc.infra

			connections, cleanUpFunc, connError = testlib.SetupSimulator(testlib.DefaultModel, infra)
			defer func() {
				if cleanUpFunc != nil {
					cleanUpFunc()
				}
			}()

			if connError != nil {
				t.Fatalf("error connecting to vcenter: %v", connError)
			}

			for _, conn := range connections {
				var fds []*v1.VSpherePlatformFailureDomainSpec

				// Get Failure domains to use for this storage policy based on the connection hostname (vCenter)
				server := conn.Hostname

				for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
					if fd.Server == server {
						fds = append(fds, &fd)
					}
				}

				storagePolicyAPIClient := &storagePolicyAPI{
					vcenterApiConnection: conn,
					infra:                tc.infra,
					failureDomains:       fds,
					categoryName:         fmt.Sprintf(categoryNameTemplate, infra.Status.InfrastructureName),
					policyName:           fmt.Sprintf(policyNameTemplate, infra.Status.InfrastructureName),
					tagName:              infra.Status.InfrastructureName,
					apiTestInfo:          map[string]int{},
				}

				_, err := storagePolicyAPIClient.createStoragePolicy(context.TODO())
				if err != nil {
					t.Fatalf("Error creating storage policy: %v", err)
				}

				found, err := storagePolicyAPIClient.checkForExistingPolicy(context.TODO())
				if err != nil {
					t.Fatalf("error while trying to find storage policy: %v", err)
				}
				if !found {
					t.Errorf("expected policy to be created found none")
				}

				validateAPICallCount(t, storagePolicyAPIClient, tc.expectedApiCallCount)

				// We should delete the created storage policy, because simulator seems to be caching it
				err = storagePolicyAPIClient.deleteStoragePolicy(context.TODO())
				if err != nil {
					t.Errorf("error deleting storage policy: %v", err)
				}
			}
		})
	}

}

func validateAPICallCount(t *testing.T, vmwareAPI *storagePolicyAPI, expectedMap map[string]int) {
	for k, v := range expectedMap {
		actualCount := vmwareAPI.apiTestInfo[k]
		if actualCount != v {
			t.Errorf("expected %s call to be %d, found %d", k, v, actualCount)
		}
	}
}
