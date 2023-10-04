package checks

import (
	"fmt"
	"testing"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCheckForIntreePluginUse(t *testing.T) {
	tests := []struct {
		name                   string
		initialObjects         []runtime.Object
		configObjects          runtime.Object
		clusterCSIDriverObject *testlib.FakeDriverInstance
		expectedBlockResult    ClusterCheckStatus
	}{
		{
			name:                   "when intree pvs exist",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetIntreePV("intree-pv"), testlib.GetNodeWithInlinePV("foobar", false /*hasinline volume*/)},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			expectedBlockResult:    ClusterCheckUpgradesBlockedViaAdminAck,
		},
		{
			name:                   "when inline volumes exist",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetNodeWithInlinePV("foobar", true /*hasinline volume*/)},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			expectedBlockResult:    ClusterCheckUpgradesBlockedViaAdminAck,
		},
		{
			name:                   "when csi migration is not enabled but no intree volumes exist",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			expectedBlockResult:    ClusterCheckAllGood,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			commonApiClient := testlib.NewFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)
			clusterCSIDriver := testlib.GetClusterCSIDriver(false)
			testlib.AddClusterCSIDriverClient(commonApiClient, clusterCSIDriver)

			test.initialObjects = append(test.initialObjects, clusterCSIDriver)

			stopCh := make(chan struct{})
			defer close(stopCh)

			testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects(test.initialObjects, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			testlib.WaitForSync(commonApiClient, stopCh)
			checkApiClient := KubeAPIInterfaceImpl{
				Infrastructure:  testlib.GetInfraObject(),
				CSINodeLister:   commonApiClient.KubeInformers.InformersFor("").Storage().V1().CSINodes().Lister(),
				CSIDriverLister: commonApiClient.KubeInformers.InformersFor("").Storage().V1().CSIDrivers().Lister(),
				NodeLister:      commonApiClient.NodeInformer.Lister(),
				PvLister:        commonApiClient.KubeInformers.InformersFor("").Core().V1().PersistentVolumes().Lister(),
			}

			overallClusterStatus, _ := checkForIntreePluginUse(makeBuggyEnvironmentError(CheckStatusBuggyMigrationPlatform, fmt.Errorf("version is older")), &checkApiClient)
			if overallClusterStatus != test.expectedBlockResult {
				t.Errorf("expected %v, got %v", test.expectedBlockResult, overallClusterStatus)
			}
		})
	}

}
