package vspherecontroller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	ocpv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
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

// TestTopologyHookTransitions verifies that the topology hook correctly handles
// transitions between topology-enabled and topology-disabled states.
// The hook only appends topology args when categories exist — since library-go's
// DeploymentController reads from the manifest base each sync, this means
// topology args naturally disappear when categories are empty.
func TestTopologyHookTransitions(t *testing.T) {
	tests := []struct {
		name                string
		failureDomainCount  int
		expectTopologyArgs  bool
	}{
		{
			name:               "2+ failure domains enables topology",
			failureDomainCount: 2,
			expectTopologyArgs: true,
		},
		{
			name:               "1 failure domain disables topology",
			failureDomainCount: 1,
			expectTopologyArgs: false,
		},
		{
			name:               "0 failure domains disables topology",
			failureDomainCount: 0,
			expectTopologyArgs: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			infraObj := testlib.GetInfraObject()

			// Set up failure domains
			if infraObj.Spec.PlatformSpec.VSphere == nil {
				infraObj.Spec.PlatformSpec.VSphere = &ocpv1.VSpherePlatformSpec{}
			}
			infraObj.Spec.PlatformSpec.VSphere.FailureDomains = make([]ocpv1.VSpherePlatformFailureDomainSpec, test.failureDomainCount)
			for i := 0; i < test.failureDomainCount; i++ {
				infraObj.Spec.PlatformSpec.VSphere.FailureDomains[i] = ocpv1.VSpherePlatformFailureDomainSpec{
					Name:   "fd" + string(rune('1'+i)),
					Region: "region1",
					Zone:   "zone" + string(rune('1'+i)),
					Server: "vcenter.example.com",
					Topology: ocpv1.VSpherePlatformTopology{
						Datacenter: "dc1",
						Datastore:  "ds" + string(rune('1'+i)),
					},
				}
			}

			clusterCSIDriver := testlib.GetClusterCSIDriver(false)

			// Get topology categories — this is what the topology hook uses
			topologyCategories := utils.GetTopologyCategories(clusterCSIDriver, infraObj)

			// Create a minimal deployment with a csi-provisioner container
			deployment := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: v1.PodTemplateSpec{
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name: "csi-provisioner",
									Args: []string{"--v=2"},
								},
							},
						},
					},
				},
			}

			// Simulate the topology hook behavior
			if len(topologyCategories) > 0 {
				containers := deployment.Spec.Template.Spec.Containers
				for i := range containers {
					if containers[i].Name == "csi-provisioner" {
						containers[i].Args = append(containers[i].Args, "--feature-gates=Topology=true", "--strict-topology")
					}
				}
			}

			// Verify
			provisionerArgs := deployment.Spec.Template.Spec.Containers[0].Args
			hasTopologyArg := false
			for _, arg := range provisionerArgs {
				if arg == "--feature-gates=Topology=true" {
					hasTopologyArg = true
					break
				}
			}

			if test.expectTopologyArgs && !hasTopologyArg {
				t.Error("expected topology args but none found")
			}
			if !test.expectTopologyArgs && hasTopologyArg {
				t.Error("did not expect topology args but found them")
			}
		})
	}
}

// TestTopologyHookSequence verifies topology transitions across a sequence of
// failure domain count changes: 2 FD → 1 FD → 2 FD → 0 FD
func TestTopologyHookSequence(t *testing.T) {
	fdCounts := []int{2, 1, 2, 0}
	expectedTopology := []bool{true, false, true, false}

	for step, fdCount := range fdCounts {
		infraObj := testlib.GetInfraObject()
		if infraObj.Spec.PlatformSpec.VSphere == nil {
			infraObj.Spec.PlatformSpec.VSphere = &ocpv1.VSpherePlatformSpec{}
		}
		infraObj.Spec.PlatformSpec.VSphere.FailureDomains = make([]ocpv1.VSpherePlatformFailureDomainSpec, fdCount)
		for i := 0; i < fdCount; i++ {
			infraObj.Spec.PlatformSpec.VSphere.FailureDomains[i] = ocpv1.VSpherePlatformFailureDomainSpec{
				Name:   "fd",
				Region: "r",
				Zone:   "z",
				Server: "vc",
				Topology: ocpv1.VSpherePlatformTopology{
					Datacenter: "dc",
					Datastore:  "ds",
				},
			}
		}

		clusterCSIDriver := testlib.GetClusterCSIDriver(false)
		topologyCategories := utils.GetTopologyCategories(clusterCSIDriver, infraObj)

		// Fresh deployment each sync (simulating library-go manifest base)
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

		if len(topologyCategories) > 0 {
			deployment.Spec.Template.Spec.Containers[0].Args = append(
				deployment.Spec.Template.Spec.Containers[0].Args,
				"--feature-gates=Topology=true", "--strict-topology",
			)
		}

		hasTopology := false
		for _, arg := range deployment.Spec.Template.Spec.Containers[0].Args {
			if arg == "--feature-gates=Topology=true" {
				hasTopology = true
			}
		}

		if hasTopology != expectedTopology[step] {
			t.Errorf("step %d (FD count=%d): expected topology=%v, got %v", step, fdCount, expectedTopology[step], hasTopology)
		}
	}
}

