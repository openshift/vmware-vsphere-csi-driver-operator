package vclib

import (
	"context"
	"fmt"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/version"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vim25/soap"
	"k8s.io/legacy-cloud-providers/vsphere"
	"net/url"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"k8s.io/klog/v2"
)

// VSphereConnection contains information for connecting to vCenter
type VSphereConnection struct {
	Client     *govmomi.Client
	RestClient *rest.Client
	Username   string
	Password   string
	Hostname   string
	Port       string
	Insecure   bool
	Config     *vsphere.VSphereConfig
}

const apiTimeout = 10 * time.Minute

var (
	clientLock sync.Mutex
)

func NewVSphereConnection(username, password string, cfg *vsphere.VSphereConfig) *VSphereConnection {
	return &VSphereConnection{
		Username: username,
		Password: password,
		Config:   cfg,
		Hostname: cfg.Workspace.VCenterIP,
		Insecure: cfg.Global.InsecureFlag,
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
