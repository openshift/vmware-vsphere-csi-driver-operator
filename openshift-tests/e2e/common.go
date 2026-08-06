package e2e

import (
	"context"
	"time"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

const (
	storageCOName = "storage"
	csiDriverName = "csi.vsphere.vmware.com"

	// Overall timeout of each ginkgo test
	testContextTimeout = 10 * time.Minute
)

// NewE2EClientsFromDefaultKubeconfig loads the default kubeconfig, builds Kubernetes, config, and
// operator clients with the given user agent, and returns a cancellable context with testTimeout.
func NewE2EClientsFromDefaultKubeconfig(userAgent string, testTimeout time.Duration) (
	ctx context.Context,
	cancel context.CancelFunc,
	kubeClient *kubernetes.Clientset,
	configClient *configclient.Clientset,
	operatorClient *operatorclient.Clientset,
	err error,
) {
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loader,
		&clientcmd.ConfigOverrides{ClusterInfo: api.Cluster{InsecureSkipTLSVerify: true}},
	)
	config, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	config = rest.AddUserAgent(config, userAgent)
	kubeClient = kubernetes.NewForConfigOrDie(config)
	configClient = configclient.NewForConfigOrDie(config)
	operatorClient = operatorclient.NewForConfigOrDie(config)
	ctx, cancel = context.WithTimeout(context.Background(), testTimeout)
	return ctx, cancel, kubeClient, configClient, operatorClient, nil
}
