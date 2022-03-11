package storageclasscontroller

import (
	"context"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"k8s.io/apimachinery/pkg/runtime"
	"testing"
)

const (
	testScControllerName      = "test-sc-controller"
	testScControllerNamespace = "test-sc-namespace"
)

func newStorageClassController(apiClients *utils.APIClient) *StorageClassController {
	rc := events.NewInMemoryRecorder(testScControllerName)
	scBytes, err := assets.ReadFile("storageclass.yaml")
	if err != nil {
		panic("unable to read storageclass file")
	}

	c := &StorageClassController{
		name:            testScControllerName,
		targetNamespace: testScControllerNamespace,
		manifest:        scBytes,
		kubeClient:      apiClients.KubeClient,
		operatorClient:  apiClients.OperatorClient,
		recorder:        rc,
	}

	return c
}

func getCheckAPIDependency(apiClients *utils.APIClient) checks.KubeAPIInterface {
	kubeInformers := apiClients.KubeInformers

	csiDriverLister := kubeInformers.InformersFor("").Storage().V1().CSIDrivers().Lister()
	csiNodeLister := kubeInformers.InformersFor("").Storage().V1().CSINodes().Lister()
	nodeLister := apiClients.NodeInformer.Lister()
	i := &checks.KubeAPIInterfaceImpl{
		Infrastructure:  testlib.GetInfraObject(),
		CSINodeLister:   csiNodeLister,
		CSIDriverLister: csiDriverLister,
		NodeLister:      nodeLister,
	}

	return i
}

func TestSync(t *testing.T) {
	tests := []struct {
		name                   string
		clusterCSIDriverObject *testlib.FakeDriverInstance
		initialObjects         []runtime.Object
		configObjects          runtime.Object
	}{
		{
			name:                   "when all configuration is right",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			commonApiClient := testlib.NewFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)

			apiDeps := getCheckAPIDependency(commonApiClient)
			var conn *vclib.VSphereConnection
			scController := newStorageClassController(commonApiClient)
			//TODO: fake storage policy sync
			err := scController.Sync(context.TODO(), conn, apiDeps)
			if err != nil {
				t.Fatalf("Failed to sync")
			}
		})
	}
}
