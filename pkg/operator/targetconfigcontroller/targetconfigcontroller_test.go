package targetconfigcontroller

import (
	"context"
	"testing"

	opv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	testControllerName = "VMWareVsphereDriverTargetConfigController"
)

func TestApplyClusterCSIDriverChange(t *testing.T) {
	tests := []struct {
		name             string
		clusterCSIDriver *opv1.ClusterCSIDriver
		operatorObj      *testlib.FakeDriverInstance
	}{
		{
			name:             "when driver has available and progressing conditions",
			clusterCSIDriver: testlib.GetClusterCSIDriver(true),
			operatorObj: testlib.MakeFakeDriverInstance(func(i *testlib.FakeDriverInstance) *testlib.FakeDriverInstance {
				i.Status.Conditions = []opv1.OperatorCondition{
					{
						Type:   testControllerName + opv1.OperatorStatusTypeAvailable,
						Status: opv1.ConditionTrue,
					},
					{
						Type:   testControllerName + opv1.OperatorStatusTypeProgressing,
						Status: opv1.ConditionFalse,
					},
				}
				return i
			}),
		},
		{
			name:             "when driver has degraded condition",
			clusterCSIDriver: testlib.GetClusterCSIDriver(true),
			operatorObj: testlib.MakeFakeDriverInstance(func(i *testlib.FakeDriverInstance) *testlib.FakeDriverInstance {
				i.Status.Conditions = []opv1.OperatorCondition{
					{
						Type:   testControllerName + opv1.OperatorStatusTypeDegraded,
						Status: opv1.ConditionTrue,
					},
				}
				return i
			}),
		},
	}

	for i := range tests {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			infra := testlib.GetInfraObject()
			commonApiClient := testlib.NewFakeClients([]runtime.Object{}, tc.operatorObj, infra)
			testlib.AddClusterCSIDriverClient(commonApiClient, tc.clusterCSIDriver)
			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects([]runtime.Object{tc.clusterCSIDriver}, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			testlib.WaitForSync(commonApiClient, stopCh)

			configMapInformer := commonApiClient.KubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
			infraInformer := commonApiClient.ConfigInformers.Config().V1().Infrastructures()
			rc := events.NewInMemoryRecorder(testControllerName)
			ctrl := &TargetConfigController{
				name:                   testControllerName,
				targetNamespace:        utils.DefaultNamespace,
				kubeClient:             commonApiClient.KubeClient,
				operatorClient:         commonApiClient.OperatorClient,
				configMapLister:        configMapInformer.Lister(),
				infraLister:            infraInformer.Lister(),
				clusterCSIDriverLister: commonApiClient.ClusterCSIDriverInformer.Lister(),
			}

			err := ctrl.sync(context.TODO(), factory.NewSyncContext("vsphere-controller", rc))
			if err != nil {
				t.Fatalf("Unexpected error that could degrade cluster: %+v", err)
			}
			_, status, _, _ := commonApiClient.OperatorClient.GetOperatorState()
			conditions := status.Conditions
			if len(conditions) > 0 {
				t.Fatalf("expectec conditions to be empty, got %+v", conditions)
			}
		})
	}

}
