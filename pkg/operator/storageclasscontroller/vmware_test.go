package storageclasscontroller

import (
	"context"
	"fmt"
	"testing"

	v1 "github.com/openshift/api/config/v1"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/stretchr/testify/assert"
)

func TestCreateStoragePolicyOnce(t *testing.T) {
	var cleanUpFunc func()
	var connections []*vclib.VSphereConnection
	var connError error

	infra := testlib.GetInfraObject()

	connections, cleanUpFunc, _, connError = testlib.SetupSimulator(testlib.DefaultModel, infra)
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

	connections, cleanUpFunc, _, connError = testlib.SetupSimulator(testlib.DefaultModel, infra)
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
		},
	}

	for _, test := range tests {
		tc := test
		t.Run(tc.name, func(t *testing.T) {
			var cleanUpFunc func()
			var connections []*vclib.VSphereConnection
			var connError error

			infra := tc.infra

			connections, cleanUpFunc, _, connError = testlib.SetupSimulator(testlib.DefaultModel, infra)
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

func TestGetDefaultDatastore(t *testing.T) {
	tests := []struct {
		name                  string
		infra                 *v1.Infrastructure
		expectedDatastoreName string
		expectedErr           error
		config                string
		configErr             error
	}{
		{
			name:                  "test with no failure domains",
			infra:                 testlib.GetInfraObject(),
			config:                "no_failure_domains_config.ini",
			configErr:             nil,
			expectedDatastoreName: "LocalDS_0",
			expectedErr:           nil,
		},
		{
			name:                  "test with empty legacy config",
			infra:                 testlib.GetInfraObjectWithEmptyPlatformSpec(),
			config:                "empty_config.ini",
			configErr:             fmt.Errorf("Invalid YAML/INI file"),
			expectedDatastoreName: "",
			expectedErr:           fmt.Errorf("unable to determine default datacenter from current config: legacy config not found"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connections, cleanUpFunc, _, connError := testlib.SetupSimulator(testlib.DefaultModel, test.infra)
			defer func() {
				if cleanUpFunc != nil {
					cleanUpFunc()
				}
			}()
			assert.NoErrorf(t, connError, "error connecting to vcenter: %v", connError)

			config, err := testlib.GetVSphereConfig(test.config)
			assert.Equalf(t, test.configErr, err, "expected error: %v, actual error: %v", test.configErr, err)

			for _, conn := range connections {
				conn.Config = config

				storagePolicyAPIClient := &storagePolicyAPI{
					vcenterApiConnection: conn,
					infra:                test.infra,
					categoryName:         fmt.Sprintf(categoryNameTemplate, test.infra.Status.InfrastructureName),
					policyName:           fmt.Sprintf(policyNameTemplate, test.infra.Status.InfrastructureName),
					tagName:              test.infra.Status.InfrastructureName,
					apiTestInfo:          map[string]int{},
				}

				datastore, err := storagePolicyAPIClient.GetDefaultDatastore(context.TODO(), test.infra)
				if datastore != nil {
					datastoreName := datastore.Info.GetDatastoreInfo().Name
					assert.Equal(t, test.expectedDatastoreName, datastoreName)
				}
				assert.Equalf(t, test.expectedErr, err, "expected error: %v, actual error: %v", test.expectedErr, err)
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
