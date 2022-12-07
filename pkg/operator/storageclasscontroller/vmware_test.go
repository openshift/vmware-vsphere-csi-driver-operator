package storageclasscontroller

import (
	"context"
	"fmt"
	"testing"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
)

func TestCreateStoragePolicyOnce(t *testing.T) {
	var cleanUpFunc func()
	var conn *vclib.VSphereConnection
	var connError error
	conn, cleanUpFunc, connError = testlib.SetupSimulator(testlib.DefaultModel)
	defer func() {
		if cleanUpFunc != nil {
			cleanUpFunc()
		}
	}()

	if connError != nil {
		t.Fatalf("error connecting to vcenter: %v", connError)
	}

	infra := testlib.GetInfraObject()

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
	validateApiCallCount(t, storagePolicyAPIClient, map[string]int{
		create_tag_api:      1,
		create_category_api: 1,
		attach_tag_api:      1,
		create_profile_api:  1,
	})

}

func TestDuplicatePolicyCreation(t *testing.T) {
	var cleanUpFunc func()
	var conn *vclib.VSphereConnection
	var connError error
	conn, cleanUpFunc, connError = testlib.SetupSimulator(testlib.DefaultModel)
	defer func() {
		if cleanUpFunc != nil {
			cleanUpFunc()
		}
	}()

	if connError != nil {
		t.Fatalf("error connecting to vcenter: %v", connError)
	}

	infra := testlib.GetInfraObject()

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
	validateApiCallCount(t, storagePolicyAPIClient, map[string]int{
		create_tag_api:      1,
		create_category_api: 1,
		attach_tag_api:      1,
		create_profile_api:  1,
	})
}

func TestZonalPolicyCreation(t *testing.T) {
	var cleanUpFunc func()
	var conn *vclib.VSphereConnection
	var connError error
	conn, cleanUpFunc, connError = testlib.SetupSimulator(testlib.DefaultModel)
	defer func() {
		if cleanUpFunc != nil {
			cleanUpFunc()
		}
	}()

	if connError != nil {
		t.Fatalf("error connecting to vcenter: %v", connError)
	}

	infra := testlib.GetZonalInfra()

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
	validateApiCallCount(t, storagePolicyAPIClient, map[string]int{
		create_tag_api:      1,
		create_category_api: 1,
		update_category_api: 1,
		attach_tag_api:      2,
		create_profile_api:  1,
	})
}

func validateApiCallCount(t *testing.T, vmwareAPI *storagePolicyAPI, expectedMap map[string]int) {
	for k, v := range expectedMap {
		actualCount := vmwareAPI.apiTestInfo[k]
		if actualCount != v {
			t.Errorf("expected %s call to be %d, found %d", k, v, actualCount)
		}
	}
}
