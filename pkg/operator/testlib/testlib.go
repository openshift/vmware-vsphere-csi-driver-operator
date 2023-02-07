package testlib

import (
	"embed"
	"fmt"

	ocpv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	fakeconfig "github.com/openshift/client-go/config/clientset/versioned/fake"
	cfginformers "github.com/openshift/client-go/config/informers/externalversions"
	fakeoperator "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"gopkg.in/gcfg.v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fakecore "k8s.io/client-go/kubernetes/fake"
	"k8s.io/legacy-cloud-providers/vsphere"
)

//go:embed *.yaml *.ini
var f embed.FS

const (
	cloudConfigNamespace = "openshift-config"
	infraGlobalName      = "cluster"
	secretName           = "vmware-vsphere-cloud-credentials"
	defaultNamespace     = "openshift-cluster-csi-drivers"
)

// fakeInstance is a fake CSI driver instance that also fullfils the OperatorClient interface
type FakeDriverInstance struct {
	metav1.ObjectMeta
	Spec   opv1.OperatorSpec
	Status opv1.OperatorStatus
}

// ReadFile reads and returns the content of the named file.
func ReadFile(name string) ([]byte, error) {
	return f.ReadFile(name)
}

func WaitForSync(clients *utils.APIClient, stopCh <-chan struct{}) {
	clients.KubeInformers.InformersFor(defaultNamespace).WaitForCacheSync(stopCh)
	clients.KubeInformers.InformersFor("").WaitForCacheSync(stopCh)
	clients.KubeInformers.InformersFor(cloudConfigNamespace).WaitForCacheSync(stopCh)
	clients.ConfigInformers.WaitForCacheSync(stopCh)
	if clients.OCPOperatorInformers != nil {
		clients.OCPOperatorInformers.WaitForCacheSync(stopCh)
	}
}

func StartFakeInformer(clients *utils.APIClient, stopCh <-chan struct{}) {
	for _, informer := range []interface {
		Start(stopCh <-chan struct{})
	}{
		clients.KubeInformers,
		clients.ConfigInformers,
		clients.OCPOperatorInformers,
	} {
		if informer != nil {
			informer.Start(stopCh)
		}
	}
}

func NewFakeClients(coreObjects []runtime.Object, operatorObject *FakeDriverInstance, configObject runtime.Object) *utils.APIClient {
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	kubeClient := fakecore.NewSimpleClientset(coreObjects...)
	kubeInformers := v1helpers.NewKubeInformersForNamespaces(kubeClient, defaultNamespace, cloudConfigNamespace, "")
	nodeInformer := kubeInformers.InformersFor("").Core().V1().Nodes()
	secretInformer := kubeInformers.InformersFor(defaultNamespace).Core().V1().Secrets()

	apiClient := &utils.APIClient{}
	apiClient.KubeClient = kubeClient
	apiClient.KubeInformers = kubeInformers
	apiClient.NodeInformer = nodeInformer
	apiClient.SecretInformer = secretInformer
	apiClient.DynamicClient = dynamicClient

	operatorClient := v1helpers.NewFakeOperatorClientWithObjectMeta(&operatorObject.ObjectMeta, &operatorObject.Spec, &operatorObject.Status, nil)
	apiClient.OperatorClient = operatorClient

	configClient := fakeconfig.NewSimpleClientset(configObject)
	configInformerFactory := cfginformers.NewSharedInformerFactory(configClient, 0)
	configInformer := configInformerFactory.Config().V1().Infrastructures().Informer()
	configInformer.GetIndexer().Add(configObject)

	apiClient.ConfigClientSet = configClient
	apiClient.ConfigInformers = configInformerFactory
	AddClusterCSIDriverClient(apiClient, GetClusterCSIDriver(false))
	return apiClient
}

func AddClusterCSIDriverClient(apiClient *utils.APIClient, obj *opv1.ClusterCSIDriver) *utils.APIClient {
	// fakeOperatorClient holds client for clients to github.com/openshift/api/operator APIs
	fakeOperatorClient := fakeoperator.NewSimpleClientset(obj)
	ocpOperatorInformer := operatorinformers.NewSharedInformerFactory(fakeOperatorClient, 0)
	clusterCSIDriverInformer := ocpOperatorInformer.Operator().V1().ClusterCSIDrivers()
	apiClient.OCPOperatorInformers = ocpOperatorInformer
	apiClient.ClusterCSIDriverInformer = clusterCSIDriverInformer
	return apiClient
}

func GetClusterCSIDriver(hasTopology bool) *opv1.ClusterCSIDriver {
	c := &opv1.ClusterCSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: "csi.vsphere.vmware.com",
		},
		Spec: opv1.ClusterCSIDriverSpec{
			DriverConfig: opv1.CSIDriverConfigSpec{
				DriverType: opv1.VSphereDriverType,
				VSphere:    &opv1.VSphereCSIDriverConfigSpec{TopologyCategories: []string{"k8s-zone", "k8s-region"}},
			},
		},
		Status: opv1.ClusterCSIDriverStatus{},
	}
	if !hasTopology {
		c.Spec.DriverConfig = opv1.CSIDriverConfigSpec{}
	}
	return c
}

func AddInitialObjects(objects []runtime.Object, clients *utils.APIClient) error {
	for _, obj := range objects {
		switch obj.(type) {
		case *v1.ConfigMap:
			configMapInformer := clients.KubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps().Informer()
			configMapInformer.GetStore().Add(obj)
		case *v1.Secret:
			secretInformer := clients.SecretInformer.Informer()
			secretInformer.GetStore().Add(obj)
		case *storagev1.CSIDriver:
			csiDriverInformer := clients.KubeInformers.InformersFor("").Storage().V1().CSIDrivers().Informer()
			csiDriverInformer.GetStore().Add(obj)
		case *storagev1.CSINode:
			csiNodeInformer := clients.KubeInformers.InformersFor("").Storage().V1().CSINodes().Informer()
			csiNodeInformer.GetStore().Add(obj)
		case *v1.Node:
			nodeInformer := clients.NodeInformer
			nodeInformer.Informer().GetStore().Add(obj)
		case *opv1.ClusterCSIDriver:
			clusterCSIDriverInformer := clients.ClusterCSIDriverInformer.Informer()
			clusterCSIDriverInformer.GetStore().Add(obj)
		default:
			return fmt.Errorf("Unknown initalObject type: %+v", obj)
		}
	}
	return nil
}

func GetLegacyVSphereConfig(fileName string) (vsphere.VSphereConfig, error) {
	var cfg vsphere.VSphereConfig
	err := gcfg.ReadStringInto(&cfg, getVSphereConfigString(fileName))
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

func getVSphereConfigString(fileName string) string {
	if fileName == "" {
		fileName = "simple_config.ini"
	}
	data, err := ReadFile(fileName)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func GetMatchingCondition(status []opv1.OperatorCondition, conditionType string) *opv1.OperatorCondition {
	for _, condition := range status {
		if condition.Type == conditionType {
			return &condition
		}
	}
	return nil
}

type driverModifier func(*FakeDriverInstance) *FakeDriverInstance

func MakeFakeDriverInstance(modifiers ...driverModifier) *FakeDriverInstance {
	instance := &FakeDriverInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cluster",
			Generation: 0,
		},
		Spec: opv1.OperatorSpec{
			ManagementState: opv1.Managed,
		},
		Status: opv1.OperatorStatus{},
	}
	for _, modifier := range modifiers {
		instance = modifier(instance)
	}
	return instance
}

func GetCSIDriver(withOCPAnnotation bool) *storagev1.CSIDriver {
	driver := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: utils.VSphereDriverName,
		},
		Spec: storagev1.CSIDriverSpec{},
	}
	if withOCPAnnotation {
		driver.Annotations = map[string]string{
			utils.OpenshiftCSIDriverAnnotationKey: "true",
		}
	}
	return driver
}

func GetCSINode() *storagev1.CSINode {
	return &storagev1.CSINode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-abcd",
		},
		Spec: storagev1.CSINodeSpec{
			Drivers: []storagev1.CSINodeDriver{
				{
					Name: utils.VSphereDriverName,
				},
			},
		},
	}
}

func GetInfraObject() *ocpv1.Infrastructure {
	return &ocpv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: infraGlobalName,
		},
		Spec: ocpv1.InfrastructureSpec{
			CloudConfig: ocpv1.ConfigMapFileReference{
				Name: "cloud-provider-config",
				Key:  "config",
			},
			PlatformSpec: ocpv1.PlatformSpec{
				Type: ocpv1.VSpherePlatformType,
			},
		},
		Status: ocpv1.InfrastructureStatus{
			InfrastructureName: "vsphere",
			PlatformStatus: &ocpv1.PlatformStatus{
				Type: ocpv1.VSpherePlatformType,
			},
		},
	}
}

func GetSingleFailureDomainInfra() *ocpv1.Infrastructure {
	return &ocpv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: infraGlobalName,
		},
		Spec: ocpv1.InfrastructureSpec{
			CloudConfig: ocpv1.ConfigMapFileReference{
				Name: "cloud-provider-config",
				Key:  "config",
			},
			PlatformSpec: ocpv1.PlatformSpec{
				Type: ocpv1.VSpherePlatformType,
				VSphere: &ocpv1.VSpherePlatformSpec{
					FailureDomains: []ocpv1.VSpherePlatformFailureDomainSpec{
						{
							Name:   "us-east-1",
							Region: "us-east",
							Server: "vcenter.lan",
							Zone:   "us-east-1a",
							Topology: ocpv1.VSpherePlatformTopology{
								ComputeCluster: "/DC0/host/DC0_H0",
								Datacenter:     "DC0",
								Datastore:      "/DC0/datastore/LocalDS_0",
							},
						},
					},
				},
			},
		},
		Status: ocpv1.InfrastructureStatus{
			InfrastructureName: "vsphere2",
			PlatformStatus: &ocpv1.PlatformStatus{
				Type: ocpv1.VSpherePlatformType,
			},
		},
	}
}

func GetZonalInfra() *ocpv1.Infrastructure {
	return &ocpv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name: infraGlobalName,
		},
		Spec: ocpv1.InfrastructureSpec{
			CloudConfig: ocpv1.ConfigMapFileReference{
				Name: "cloud-provider-config",
				Key:  "config",
			},
			PlatformSpec: ocpv1.PlatformSpec{
				Type: ocpv1.VSpherePlatformType,
				VSphere: &ocpv1.VSpherePlatformSpec{
					FailureDomains: []ocpv1.VSpherePlatformFailureDomainSpec{
						{
							Name:   "us-east-1",
							Region: "us-east",
							Server: "vcenter.lan",
							Zone:   "us-east-1a",
							Topology: ocpv1.VSpherePlatformTopology{
								ComputeCluster: "/DC0/host/DC0_H0",
								Datacenter:     "DC0",
								Datastore:      "/DC0/datastore/LocalDS_0",
							},
						},
						{
							Name:   "us-west-1",
							Region: "us-west",
							Server: "vcenter.lan",
							Zone:   "us-west-1a",
							Topology: ocpv1.VSpherePlatformTopology{
								ComputeCluster: "/F0/DC1/host/F0/DC1_H0",
								Datacenter:     "/F0/DC1",
								Datastore:      "/F0/DC1/datastore/F0/LocalDS_0",
							},
						},
					},
				},
			},
		},
		Status: ocpv1.InfrastructureStatus{
			InfrastructureName: "vsphere",
			PlatformStatus: &ocpv1.PlatformStatus{
				Type: ocpv1.VSpherePlatformType,
			},
		},
	}
}

func GetConfigMap() *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloud-provider-config",
			Namespace: cloudConfigNamespace,
		},
		Data: map[string]string{
			"config": `
[Global]
secret-name = "vsphere-creds"
secret-namespace = "kube-system"
insecure-flag = "1"

[Workspace]
server = "localhost"
datacenter = "DC0"
default-datastore = "LocalDS_0"
folder = "/DC0/vm"

[VirtualCenter "dc0"]
datacenters = "DC0"
`,
		},
	}
}

func GetSecret() *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: defaultNamespace,
		},
		Data: map[string][]byte{
			"localhost.password": []byte("vsphere-user"),
			"localhost.username": []byte("vsphere-password"),
		},
	}
}

func GetTestClusterResult(statusType checks.CheckStatusType) checks.ClusterCheckResult {
	return checks.ClusterCheckResult{
		CheckError:  fmt.Errorf("some error"),
		CheckStatus: statusType,
	}
}
