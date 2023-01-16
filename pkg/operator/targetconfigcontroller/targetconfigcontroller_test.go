package targetconfigcontroller

import (
	"testing"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	iniv1 "gopkg.in/ini.v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestApplyClusterCSIDriverChange(t *testing.T) {
	tests := []struct {
		name               string
		operatorObj        *testlib.FakeDriverInstance
		configFileName     string
		expectedDatacenter string
		expectError        bool
	}{
		{
			name:               "when driver does not have topology enabled",
			operatorObj:        testlib.MakeFakeDriverInstance(),
			expectedDatacenter: "Datacenter",
		},
		{
			name:               "when configuration has more than one datacenter",
			operatorObj:        testlib.MakeFakeDriverInstance(),
			configFileName:     "multiple_dc.ini",
			expectedDatacenter: "Datacentera, DatacenterB",
		},
	}

	for i := range tests {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			infra := testlib.GetInfraObject()
			commonApiClient := testlib.NewFakeClients([]runtime.Object{}, tc.operatorObj, infra)
			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)

			cloudConfigBytes, err := assets.ReadFile("vsphere_cloud_config.yaml")
			if err != nil {
				t.Fatalf("error reading vsphere cloud config file: %v", err)
			}

			testlib.WaitForSync(commonApiClient, stopCh)

			configMapInformer := commonApiClient.KubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
			infraInformer := commonApiClient.ConfigInformers.Config().V1().Infrastructures()
			ctrl := &TargetConfigController{
				name:            "VMwareVSphereDriverTargetConfigController",
				targetNamespace: utils.DefaultNamespace,
				manifest:        cloudConfigBytes,
				kubeClient:      commonApiClient.KubeClient,
				operatorClient:  commonApiClient.OperatorClient,
				configMapLister: configMapInformer.Lister(),
				infraLister:     infraInformer.Lister(),
			}
			legacyVsphereConfig, err := testlib.GetLegacyVSphereConfig(tc.configFileName)
			if err != nil {
				t.Fatalf("error loading legacy vsphere config: %v", err)
			}

			configMap, err := ctrl.applyConfigMapChanges(infra, legacyVsphereConfig)

			// if we expected error and we got some, we should stop running this test
			if tc.expectError && err != nil {
				return
			}

			if tc.expectError && err == nil {
				t.Fatal("Expected error got none")
			}
			if err != nil {
				t.Fatalf("error creating configmap: %v", err)
			}

			configMapIni := configMap.Data["cloud.conf"]
			csiConfig, err := iniv1.Load([]byte(configMapIni))
			if err != nil {
				t.Fatalf("error loading result ini: %v", err)
			}

			datacenters, err := csiConfig.Section("VirtualCenter \"foobar.lan\"").GetKey("datacenters")
			if err != nil {
				t.Fatalf("error reading datacenters from config file: %v", err)
			}

			if datacenters.String() != tc.expectedDatacenter {
				t.Fatalf("expected datacenter to be %s, got %s", tc.expectedDatacenter, datacenters.String())
			}

		})
	}

}
