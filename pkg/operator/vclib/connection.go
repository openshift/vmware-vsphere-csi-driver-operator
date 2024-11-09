package vclib

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/version"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/third_party/cloud-provider-vsphere/pkg/common/config"
	"github.com/vmware/govmomi/cns"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	legacy "k8s.io/legacy-cloud-providers/vsphere"

	"github.com/vmware/govmomi"
	"gopkg.in/gcfg.v1"
	"k8s.io/klog/v2"
)

const apiTimeout = 10 * time.Minute

var (
	clientLock sync.Mutex
)

// VSphereConnection contains information for connecting to vCenter
type VSphereConnection struct {
	Client     *govmomi.Client
	RestClient *rest.Client
	cnsClient  *cns.Client
	Username   string
	Password   string
	Hostname   string
	Port       string
	Insecure   bool
	Config     *VSphereConfig
}

// VSphereConfig contains configuration for cloud provider.  It wraps the legacy version and the newer upstream version
// with yaml support
type VSphereConfig struct {
	Config       *config.Config
	LegacyConfig *legacy.VSphereConfig
}

// LoadConfig load the desired config into this config object.
func (c *VSphereConfig) LoadConfig(data string) error {
	var err error
	c.Config, err = config.ReadConfig([]byte(data))
	if err != nil {
		return err
	}

	// Load legacy if cfgString is not yaml.  May be needed in areas that require legacy logic.
	lCfg := &legacy.VSphereConfig{}
	err = gcfg.ReadStringInto(lCfg, data)
	if err != nil {
		// For now, we can just log an info so that we know.
		klog.V(4).Info("Unable to load cloud config as legacy ini.")
		return nil
	}

	c.LegacyConfig = lCfg
	return nil
}

// GetVCenterHostname get the vcenter's hostname.
func (c *VSphereConfig) GetVCenterHostname(vcenter string) string {
	return c.Config.VirtualCenter[vcenter].VCenterIP
}

// IsInsecure returns true if the vcenter is configured to have an insecure connection.
func (c *VSphereConfig) IsInsecure(vcenter string) bool {
	return c.Config.VirtualCenter[vcenter].InsecureFlag
}

// GetDatacenters gets the datacenters.  Falls back to legacy style ini lookup if vcenter not found in primary config.
func (c *VSphereConfig) GetDatacenters(vcenter string) ([]string, error) {
	var datacenters []string
	if c.Config.VirtualCenter[vcenter] != nil {
		datacenters = strings.Split(c.Config.VirtualCenter[vcenter].Datacenters, ",")
	} else {
		// If here, then legacy config may be in use.
		datacenters = []string{c.LegacyConfig.Workspace.Datacenter}
	}
	klog.V(2).Infof("Gathered the following data centers: %v", datacenters)
	return datacenters, nil
}

// GetWorkspaceDatacenter get the legacy datacenter from workspace config.
func (c *VSphereConfig) GetWorkspaceDatacenter() string {
	return c.LegacyConfig.Workspace.Datacenter
}

// GetDefaultDatastore get the default datastore.  This is primarily used with legacy ini config.
func (c *VSphereConfig) GetDefaultDatastore() string {
	return c.LegacyConfig.Workspace.DefaultDatastore
}

func NewVSphereConnection(username, password, vcenter string, cfg *VSphereConfig) *VSphereConnection {
	return &VSphereConnection{
		Username: username,
		Password: password,
		Config:   cfg,
		Hostname: cfg.GetVCenterHostname(vcenter),
		Insecure: cfg.IsInsecure(vcenter),
	}
}

// Connect makes connection to vCenter and sets VSphereConnection.Client.
// If connection.Client is already set, it obtains the existing user session.
// if user session is not valid, connection.Client will be set to the new client.
func (connection *VSphereConnection) Connect(ctx context.Context) error {
	clientLock.Lock()
	defer clientLock.Unlock()
	var err error

	if connection.Client == nil {
		klog.V(4).Infof("vcenter-csi creating new vcenter connection")
		err = connection.NewClient(ctx)
		if err != nil {
			klog.Errorf("Failed to create govmomi client. err: %+v", err)
			return err
		}
		return nil
	}
	return nil
}

func (connection *VSphereConnection) GetVersionInfo() (string, string, error) {
	if connection.Client == nil {
		return "", "", fmt.Errorf("no connection found to vcenter")
	}
	return connection.Client.Client.ServiceContent.About.ApiVersion, connection.Client.Client.ServiceContent.About.Build, nil
}

func (connection *VSphereConnection) NewClient(ctx context.Context) error {
	serverAddress := connection.Hostname
	serverURL, err := soap.ParseURL(serverAddress)
	if err != nil {
		return fmt.Errorf("failed to parse config file: %s", err)
	}
	tctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	klog.V(4).Infof("Connecting to %s as %s, insecure %t", serverAddress, connection.Username, connection.Insecure)

	// Set user to nil to prevent login during client creation.
	// See https://github.com/vmware/govmomi/blob/master/client.go#L91
	serverURL.User = nil
	client, err := govmomi.NewClient(tctx, serverURL, connection.Insecure)
	if err != nil {
		return err
	}

	// Set up user agent before login
	operatorVersion := version.Get()
	client.UserAgent = fmt.Sprintf("vmware-vsphere-csi-driver-operator/%s", operatorVersion)

	userInfo := url.UserPassword(connection.Username, connection.Password)

	err = client.Login(ctx, userInfo)
	if err != nil {
		msg := fmt.Sprintf("error logging into vcenter: %v", err)
		klog.Error(msg)
		return fmt.Errorf(msg)
	}
	connection.Client = client
	restClient := rest.NewClient(client.Client)
	err = restClient.Login(ctx, userInfo)
	if err != nil {
		msg := fmt.Sprintf("error logging into vcenter RESTful services: %v", err)
		klog.Error(msg)
		return fmt.Errorf(msg)
	}
	connection.RestClient = restClient
	return nil
}

// Logout calls SessionManager.Logout for the given connection.
func (connection *VSphereConnection) Logout(ctx context.Context) error {
	clientLock.Lock()
	c := connection.Client
	clientLock.Unlock()

	klog.V(4).Infof("vcenter-csi logging out from vcenter")
	if c == nil {
		return fmt.Errorf("no connection found to vcenter")
	}
	if connection.RestClient != nil {
		restLogoutError := connection.RestClient.Logout(ctx)
		if restLogoutError != nil {
			klog.Errorf("error logging out from rest session: %v", restLogoutError)
		}
	}
	return connection.Client.Logout(ctx)
}

func (connection *VSphereConnection) LoginToCNS(ctx context.Context) error {
	// Create CNS client
	cnsClient, err := cns.NewClient(ctx, connection.VimClient())
	if err != nil {
		msg := fmt.Errorf("error creating cns client: %v", err)
		klog.Error(msg)
		return msg
	}
	connection.cnsClient = cnsClient
	return nil
}

func (connection *VSphereConnection) VimClient() *vim25.Client {
	return connection.Client.Client
}

// Return default datacenter configured in Config
// TODO: Do we need to setup and handle multiple datacenters?
func (connection *VSphereConnection) DefaultDatacenter() string {
	return connection.Config.GetWorkspaceDatacenter()
}

func (connection *VSphereConnection) CnsClient() *cns.Client {
	return connection.cnsClient
}
