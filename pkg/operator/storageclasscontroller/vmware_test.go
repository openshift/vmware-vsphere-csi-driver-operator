package storageclasscontroller

import (
	"context"
	"fmt"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"testing"
)

func TestCreateStoragePolicy(t *testing.T) {
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
	}

	policyName, err := storagePolicyAPIClient.createStoragePolicy(context.TODO())
	if err != nil {
		t.Fatalf("Error creating storage policy: %v", err)
	}
	t.Logf("Successfully created storage policy %s", policyName)
}
