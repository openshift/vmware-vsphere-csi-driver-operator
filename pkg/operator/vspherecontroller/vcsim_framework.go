package vspherecontroller

import (
	"context"
	"crypto/tls"
	"fmt"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/simulator"
	"gopkg.in/gcfg.v1"
	"k8s.io/legacy-cloud-providers/vsphere"
)

const (
	defaultModel = "testdata/default"
)

func setupSimulator(modelDir string) (*vclib.VSphereConnection, func(), error) {
	model := simulator.Model{}
	err := model.Load(modelDir)
	if err != nil {
		return nil, nil, err
	}
	model.Service.TLS = new(tls.Config)

	s := model.Service.NewServer()
	client, err := connectToSimulator(s)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to the similator: %s", err)
	}
	conn := &vclib.VSphereConnection{
		Config: simulatorConfig(),
		Client: client,
	}

	cleanup := func() {
		s.Close()
		model.Remove()
	}
	return conn, cleanup, nil
}

func connectToSimulator(s *simulator.Server) (*govmomi.Client, error) {
	client, err := govmomi.NewClient(context.TODO(), s.URL, true)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func simulatorConfig() *vsphere.VSphereConfig {
	var cfg vsphere.VSphereConfig
	// Configuration that corresponds to the simulated vSphere
	data := `[Global]
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
`
	err := gcfg.ReadStringInto(&cfg, data)
	if err != nil {
		panic(err)
	}
	return &cfg
}

func customizeVCenterVersion(version string, apiVersion string, conn *vclib.VSphereConnection) {
	conn.Client.Client.ServiceContent.About.Version = version
	conn.Client.Client.ServiceContent.About.ApiVersion = apiVersion
	fmt.Printf("customize vcenter version")
}
