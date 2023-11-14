package vspherecontroller

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ocpv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
)

func TestGetvCenterName(t *testing.T) {
	tests := []struct {
		name                string
		infra               *ocpv1.Infrastructure
		configMap           *v1.ConfigMap
		expectedVCenterName string
	}{
		{
			name:                "when infra object does not have vcenter",
			infra:               testlib.GetInfraObject(),
			configMap:           testlib.GetConfigMap(),
			expectedVCenterName: "localhost",
		},
		{
			name:                "when infra object does have vcenter",
			infra:               testlib.GetSingleFailureDomainInfra(),
			configMap:           testlib.GetConfigMap(),
			expectedVCenterName: "vcenter.home.lan",
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			initialiObjects := []runtime.Object{test.configMap}
			configObjects := runtime.Object(test.infra)

			commonApiClient := testlib.NewFakeClients(initialiObjects, testlib.MakeFakeDriverInstance(), configObjects)

			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects(initialiObjects, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}

			testlib.WaitForSync(commonApiClient, stopCh)

			kubeInformers := commonApiClient.KubeInformers
			configMapInformer := kubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
			configMapLister := configMapInformer.Lister()

			vCenterName, err := getvCenterName(test.infra, configMapLister)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if vCenterName != test.expectedVCenterName {
				t.Errorf("expected vCenter name to be %s got %s", test.expectedVCenterName, vCenterName)
			}
		})
	}
}
