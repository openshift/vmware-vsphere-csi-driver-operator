package vspherecontroller

import (
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ocpv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
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
			name:                "when infra object does not have vcenter YAML",
			infra:               testlib.GetInfraObject(),
			configMap:           testlib.GetNewConfigMap(),
			expectedVCenterName: "localhost",
		},
		{
			name:                "when infra object does have vcenter",
			infra:               testlib.GetSingleFailureDomainInfra(),
			configMap:           testlib.GetConfigMap(),
			expectedVCenterName: "vcenter.home.lan",
		},
		{
			name:                "when infra object does have vcenter YAML",
			infra:               testlib.GetSingleFailureDomainInfra(),
			configMap:           testlib.GetNewConfigMap(),
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

// TestTopologyHookTransitions verifies that the actual topologyHook method
// correctly handles transitions between topology-enabled and topology-disabled
// states, including the else branch that sets --feature-gates=Topology=false.
func TestTopologyHookTransitions(t *testing.T) {
	tests := []struct {
		name                    string
		failureDomainCount      int
		expectTopologyTrue      bool
		expectTopologyFalse     bool
		expectStrictTopology    bool
	}{
		{
			name:                 "2+ failure domains enables topology",
			failureDomainCount:   2,
			expectTopologyTrue:   true,
			expectStrictTopology: true,
		},
		{
			name:                "1 failure domain sets topology false",
			failureDomainCount:  1,
			expectTopologyFalse: true,
		},
		{
			name:                "0 failure domains sets topology false",
			failureDomainCount:  0,
			expectTopologyFalse: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			infraObj := testlib.GetInfraObject()
			if infraObj.Spec.PlatformSpec.VSphere == nil {
				infraObj.Spec.PlatformSpec.VSphere = &ocpv1.VSpherePlatformSpec{}
			}
			infraObj.Spec.PlatformSpec.VSphere.FailureDomains = make([]ocpv1.VSpherePlatformFailureDomainSpec, test.failureDomainCount)
			for i := 0; i < test.failureDomainCount; i++ {
				infraObj.Spec.PlatformSpec.VSphere.FailureDomains[i] = ocpv1.VSpherePlatformFailureDomainSpec{
					Name:   fmt.Sprintf("fd%d", i+1),
					Region: "region1",
					Zone:   fmt.Sprintf("zone%d", i+1),
					Server: "vcenter.example.com",
					Topology: ocpv1.VSpherePlatformTopology{
						Datacenter: "dc1",
						Datastore:  fmt.Sprintf("ds%d", i+1),
					},
				}
			}

			clusterCSIDriver := testlib.GetClusterCSIDriver(false)
			initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
			commonApiClient := testlib.NewFakeClients(initialObjects, testlib.MakeFakeDriverInstance(), infraObj)
			testlib.AddClusterCSIDriverClient(commonApiClient, clusterCSIDriver)

			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects(append(initialObjects, clusterCSIDriver), commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}
			testlib.WaitForSync(commonApiClient, stopCh)

			ctrl := newVsphereController(commonApiClient)

			// Fresh deployment each call (simulating library-go manifest base)
			deployment := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{Name: "csi-provisioner", Args: []string{"--v=2"}},
								{Name: "csi-driver", Args: []string{}},
							},
						},
					},
				},
			}

			err := ctrl.topologyHook(&operatorapi.OperatorSpec{}, deployment)
			if err != nil {
				t.Fatalf("topologyHook returned error: %v", err)
			}

			// Check csi-provisioner args
			var provisionerArgs []string
			for _, c := range deployment.Spec.Template.Spec.Containers {
				if c.Name == "csi-provisioner" {
					provisionerArgs = c.Args
					break
				}
			}

			hasTopologyTrue := containsArg(provisionerArgs, "--feature-gates=Topology=true")
			hasTopologyFalse := containsArg(provisionerArgs, "--feature-gates=Topology=false")
			hasStrictTopology := containsArg(provisionerArgs, "--strict-topology")

			if test.expectTopologyTrue && !hasTopologyTrue {
				t.Errorf("expected --feature-gates=Topology=true but not found in args: %v", provisionerArgs)
			}
			if test.expectTopologyFalse && !hasTopologyFalse {
				t.Errorf("expected --feature-gates=Topology=false but not found in args: %v", provisionerArgs)
			}
			if test.expectStrictTopology && !hasStrictTopology {
				t.Errorf("expected --strict-topology but not found in args: %v", provisionerArgs)
			}
			// Mutually exclusive: topology=true and topology=false should not both appear
			if hasTopologyTrue && hasTopologyFalse {
				t.Errorf("both Topology=true and Topology=false found in args: %v", provisionerArgs)
			}
		})
	}
}

// TestTopologyHookSequence verifies topology transitions across a sequence of
// failure domain count changes: 2 FD -> 1 FD -> 2 FD -> 0 FD
// Each step creates a fresh deployment to simulate library-go's manifest base behavior.
func TestTopologyHookSequence(t *testing.T) {
	fdCounts := []int{2, 1, 2, 0}
	expectTrue := []bool{true, false, true, false}
	expectFalse := []bool{false, true, false, true}

	for step, fdCount := range fdCounts {
		infraObj := testlib.GetInfraObject()
		if infraObj.Spec.PlatformSpec.VSphere == nil {
			infraObj.Spec.PlatformSpec.VSphere = &ocpv1.VSpherePlatformSpec{}
		}
		infraObj.Spec.PlatformSpec.VSphere.FailureDomains = make([]ocpv1.VSpherePlatformFailureDomainSpec, fdCount)
		for i := 0; i < fdCount; i++ {
			infraObj.Spec.PlatformSpec.VSphere.FailureDomains[i] = ocpv1.VSpherePlatformFailureDomainSpec{
				Name: "fd", Region: "r", Zone: "z", Server: "vc",
				Topology: ocpv1.VSpherePlatformTopology{Datacenter: "dc", Datastore: "ds"},
			}
		}

		clusterCSIDriver := testlib.GetClusterCSIDriver(false)
		initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()}
		commonApiClient := testlib.NewFakeClients(initialObjects, testlib.MakeFakeDriverInstance(), infraObj)
		testlib.AddClusterCSIDriverClient(commonApiClient, clusterCSIDriver)

		stopCh := make(chan struct{})
		go testlib.StartFakeInformer(commonApiClient, stopCh)
		if err := testlib.AddInitialObjects(append(initialObjects, clusterCSIDriver), commonApiClient); err != nil {
			close(stopCh)
			t.Fatalf("step %d: AddInitialObjects failed: %v", step, err)
		}
		testlib.WaitForSync(commonApiClient, stopCh)

		ctrl := newVsphereController(commonApiClient)

		deployment := &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{Name: "csi-provisioner", Args: []string{"--v=2"}},
						},
					},
				},
			},
		}

		err := ctrl.topologyHook(&operatorapi.OperatorSpec{}, deployment)
		close(stopCh)
		if err != nil {
			t.Fatalf("step %d: topologyHook error: %v", step, err)
		}

		args := deployment.Spec.Template.Spec.Containers[0].Args
		hasTrue := containsArg(args, "--feature-gates=Topology=true")
		hasFalse := containsArg(args, "--feature-gates=Topology=false")

		if hasTrue != expectTrue[step] {
			t.Errorf("step %d (FD=%d): Topology=true expected=%v got=%v, args=%v",
				step, fdCount, expectTrue[step], hasTrue, args)
		}
		if hasFalse != expectFalse[step] {
			t.Errorf("step %d (FD=%d): Topology=false expected=%v got=%v, args=%v",
				step, fdCount, expectFalse[step], hasFalse, args)
		}
	}
}

func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}
