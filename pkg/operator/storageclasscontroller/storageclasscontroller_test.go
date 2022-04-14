package storageclasscontroller

import (
	"context"
	"fmt"
	v1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/events"
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

type fakeStoragePolicyAPI struct {
	vCenterInterface
	ret string
	err error
}

func (v *fakeStoragePolicyAPI) createStoragePolicy(ctx context.Context) (string, error) {
	return v.ret, v.err
}

func newFakeStoragePolicyAPISuccess(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure) vCenterInterface {
	fakeStoragePolicyAPI := &fakeStoragePolicyAPI{ret: "fake-return-value"}
	return fakeStoragePolicyAPI
}

func newFakeStoragePolicyAPIFailure(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure) vCenterInterface {
	fakeStoragePolicyAPI := &fakeStoragePolicyAPI{ret: "fake-return-value", err: fmt.Errorf("fake-error")}
	return fakeStoragePolicyAPI
}

func newStorageClassController(apiClients *utils.APIClient, storageclassfile string, storagePolicyAPIfailing bool) *StorageClassController {
	rc := events.NewInMemoryRecorder(testScControllerName)
	scBytes, err := testlib.ReadFile(storageclassfile)
	if err != nil {
		panic("unable to read storageclass file")
	}

	spFunc := newFakeStoragePolicyAPISuccess
	if storagePolicyAPIfailing {
		spFunc = newFakeStoragePolicyAPIFailure
	}

	c := &StorageClassController{
		name:                 testScControllerName,
		targetNamespace:      testScControllerNamespace,
		manifest:             scBytes,
		kubeClient:           apiClients.KubeClient,
		operatorClient:       apiClients.OperatorClient,
		recorder:             rc,
		makeStoragePolicyAPI: spFunc,
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

func assertPanic(t *testing.T) {
	if r := recover(); r == nil {
		t.Errorf("Test should have panicked but did not.")
	}
}

func TestSync(t *testing.T) {
	tests := []struct {
		name                   string
		clusterCSIDriverObject *testlib.FakeDriverInstance
		initialObjects         []runtime.Object
		configObjects          runtime.Object
		storageClass           string
		expectError            error
		expectedConditions     []opv1.OperatorCondition
		scConstructor          interface{}
		StoragePolicyAPIfails  bool
		shouldPanic            bool
	}{
		{
			name:                   "sync succeeds with valid storage class",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			storageClass:           "storageclass1.yaml",
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeDegraded,
					Status: opv1.ConditionFalse,
				},
			},
		},
		{
			name:                   "sync does not degrade on storage policy api error",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			storageClass:           "storageclass1.yaml",
			StoragePolicyAPIfails:  true,
			expectedConditions: []opv1.OperatorCondition{
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeAvailable,
					Status: opv1.ConditionTrue,
				},
				{
					Type:   testScControllerName + opv1.OperatorStatusTypeDegraded,
					Status: opv1.ConditionFalse,
				},
			},
		},
		{
			name:                   "sync panics with invalid storage class object",
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			storageClass:           "storageclass2.yaml",
			shouldPanic:            true,
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			commonApiClient := testlib.NewFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)

			apiDeps := getCheckAPIDependency(commonApiClient)
			var conn *vclib.VSphereConnection
			scController := newStorageClassController(commonApiClient, test.storageClass, test.StoragePolicyAPIfails)

			if test.shouldPanic {
				defer assertPanic(t)
			}
			// err will be nil on even on failure, need to check conditions instead
			err := scController.Sync(context.TODO(), conn, apiDeps)

			_, status, _, err := scController.operatorClient.GetOperatorState()
			if err != nil {
				t.Errorf("failed to get operator state: %+v", err)
			}

			for i := range test.expectedConditions {
				expectedCondition := test.expectedConditions[i]
				matchingCondition := testlib.GetMatchingCondition(status.Conditions, expectedCondition.Type)
				if matchingCondition == nil {
					t.Fatalf("found no matching condition for: %s", expectedCondition.Type)
				}
				if matchingCondition.Status != expectedCondition.Status {
					t.Fatalf("for condition %s: expected status: %v, got: %v", expectedCondition.Type, expectedCondition.Status, matchingCondition.Status)
				}
			}

		})
	}
}
