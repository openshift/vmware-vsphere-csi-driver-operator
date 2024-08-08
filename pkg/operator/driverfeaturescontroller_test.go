package operator

import (
	"context"
	"maps"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	cfgv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func setFeature(features map[string]string, name, value string) map[string]string {
	newFeatures := maps.Clone(features)
	newFeatures[name] = value
	return newFeatures
}

func TestDriverFeaturesController_Sync(t *testing.T) {
	csiDriver := &opv1.ClusterCSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: "csi.vsphere.vmware.com",
		},
		Spec: opv1.ClusterCSIDriverSpec{},
	}
	csiDriverWithTopology := &opv1.ClusterCSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: "csi.vsphere.vmware.com",
		},
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				DriverType: opv1.VSphereDriverType,
				VSphere: &opv1.VSphereCSIDriverConfigSpec{
					TopologyCategories: []string{"region", "zone"},
				},
			},
		},
	}

	infra := &cfgv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: cfgv1.InfrastructureSpec{
			PlatformSpec: cfgv1.PlatformSpec{
				Type:    cfgv1.VSpherePlatformType,
				VSphere: &cfgv1.VSpherePlatformSpec{},
			},
		},
	}
	infraWithTopology := &cfgv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: cfgv1.InfrastructureSpec{
			PlatformSpec: cfgv1.PlatformSpec{
				Type: cfgv1.VSpherePlatformType,
				VSphere: &cfgv1.VSpherePlatformSpec{
					FailureDomains: []cfgv1.VSpherePlatformFailureDomainSpec{
						{
							Name:     "name1",
							Region:   "region1",
							Zone:     "zone1",
							Topology: cfgv1.VSpherePlatformTopology{},
						},
						{
							Name:     "name2",
							Region:   "region2",
							Zone:     "zone2",
							Topology: cfgv1.VSpherePlatformTopology{},
						},
					},
				},
			},
		},
	}

	infraWithVCenters := &cfgv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: cfgv1.InfrastructureSpec{
			PlatformSpec: cfgv1.PlatformSpec{
				Type: cfgv1.VSpherePlatformType,
				VSphere: &cfgv1.VSpherePlatformSpec{
					VCenters: []cfgv1.VSpherePlatformVCenterSpec{
						{
							Server: "vcenter1",
							Port:   80,
						},
						{
							Server: "vcenter1",
							Port:   80,
						},
					},
				},
			},
		},
	}

	defaultFeatureGates := map[string]string{
		"async-query-volume":                "true",
		"block-volume-snapshot":             "true",
		"cnsmgr-suspend-create-volume":      "true",
		"csi-auth-check":                    "true",
		"csi-migration":                     "true",
		"csi-windows-support":               "true",
		"improved-csi-idempotency":          "true",
		"improved-volume-topology":          "false",
		"list-volumes":                      "true",
		"multi-vcenter-csi-topology":        "true",
		"online-volume-extend":              "true",
		"pv-to-backingdiskobjectid-mapping": "false",
		"topology-preferential-datastores":  "true",
		"trigger-csi-fullsync":              "false",
		"use-csinode-id":                    "true",
	}

	tests := []struct {
		name             string
		infra            *cfgv1.Infrastructure
		clusterCSIDriver *opv1.ClusterCSIDriver
		expectedFeatures map[string]string
	}{
		{
			name:             "No topology",
			infra:            infra,
			clusterCSIDriver: csiDriver,
			expectedFeatures: defaultFeatureGates,
		}, {
			name:             "ClusterCSIDriver failureDomains",
			infra:            infra,
			clusterCSIDriver: csiDriverWithTopology,
			expectedFeatures: setFeature(defaultFeatureGates, "improved-volume-topology", "true"),
		}, {
			name:             "Infrastructure failureDomains",
			infra:            infraWithTopology,
			clusterCSIDriver: csiDriver,
			expectedFeatures: setFeature(defaultFeatureGates, "improved-volume-topology", "true"),
		}, {
			name:             "multiple vCenters",
			infra:            infraWithVCenters,
			clusterCSIDriver: csiDriver,
			expectedFeatures: setFeature(defaultFeatureGates, "csi-migration", "false"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configObjects := runtime.Object(test.infra)
			commonAPIClient := testlib.NewFakeClients(
				[]runtime.Object{},
				testlib.MakeFakeDriverInstance(),
				configObjects,
			)
			testlib.AddClusterCSIDriverClient(commonAPIClient, test.clusterCSIDriver)

			featureConfigBytes, err := assets.ReadFile("vsphere_features_config.yaml")
			if err != nil {
				t.Fatalf("failed to parse vsphere_features_config.yaml: %v", err)
			}

			recorder := events.NewInMemoryRecorder("test")
			d := NewDriverFeaturesController(
				"test",
				cloudConfigNamespace,
				featureConfigBytes,
				commonAPIClient.KubeClient,
				commonAPIClient.KubeInformers,
				commonAPIClient.OperatorClient,
				commonAPIClient.ConfigInformers,
				commonAPIClient.ClusterCSIDriverInformer,
				recorder,
			)
			stopCh := make(chan struct{})
			defer close(stopCh)
			testlib.StartFakeInformer(commonAPIClient, stopCh)
			testlib.WaitForSync(commonAPIClient, stopCh)

			syncContext := factory.NewSyncContext("vsphere-controller", recorder)
			ctx := context.Background()
			if err := d.Sync(ctx, syncContext); err != nil {
				t.Fatalf("Sync() error = %v", err)
			}

			configMap, err := commonAPIClient.KubeClient.CoreV1().ConfigMaps("openshift-cluster-csi-drivers").Get(ctx, "internal-feature-states.csi.vsphere.vmware.com", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get configmap: %v", err)
			}

			if !reflect.DeepEqual(configMap.Data, test.expectedFeatures) {
				t.Errorf("expected features %+v, got %+v", test.expectedFeatures, configMap.Data)
				t.Errorf("human readable diff: %s", cmp.Diff(test.expectedFeatures, configMap.Data))
			}
		})
	}
}
