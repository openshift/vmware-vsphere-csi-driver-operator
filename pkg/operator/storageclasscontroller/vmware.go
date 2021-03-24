package storageclasscontroller

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/legacy-cloud-providers/vsphere"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/vim25/soap"
)

const (
	secretName = "vmware-vsphere-cloud-credentials"
)

func (c *StorageClassController) newClient(ctx context.Context, cfg *vsphere.VSphereConfig, username, password string) (*govmomi.Client, error) {
	serverAddress := cfg.Workspace.VCenterIP
	serverURL, err := soap.ParseURL(serverAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config file: %s", err)
	}
	serverURL.User = url.UserPassword(username, password)
	insecure := cfg.Global.InsecureFlag

	tctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	klog.V(4).Infof("Connecting to %s as %s, insecure %t", serverAddress, username, insecure)
	client, err := govmomi.NewClient(tctx, serverURL, insecure)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (c *StorageClassController) getCredentials(cfg *vsphere.VSphereConfig) (string, string, error) {
	secret, err := c.secretLister.Secrets(c.targetNamespace).Get(secretName)
	if err != nil {
		return "", "", err
	}
	userKey := cfg.Workspace.VCenterIP + "." + "username"
	username, ok := secret.Data[userKey]
	if !ok {
		return "", "", fmt.Errorf("error parsing secret %q: key %q not found", secretName, userKey)
	}
	passwordKey := cfg.Workspace.VCenterIP + "." + "password"
	password, ok := secret.Data[passwordKey]
	if !ok {
		return "", "", fmt.Errorf("error parsing secret %q: key %q not found", secretName, passwordKey)
	}

	return string(username), string(password), nil
}
