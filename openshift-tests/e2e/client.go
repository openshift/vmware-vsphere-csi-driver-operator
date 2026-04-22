package e2e

import (
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// NewClientConfig returns a config configured to connect to the api server
// This is the Ginkgo-compatible version that doesn't require testing.T
func NewClientConfig() (*rest.Config, error) {
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loader,
		&clientcmd.ConfigOverrides{ClusterInfo: api.Cluster{InsecureSkipTLSVerify: true}},
	)
	return clientConfig.ClientConfig()
}

// Clients holds all the Kubernetes and OpenShift clients needed for tests
type Clients struct {
	KubeClient     *kubernetes.Clientset
	ConfigClient   *configclient.Clientset
	OperatorClient *operatorclient.Clientset
}

// NewClients creates all necessary clients with the given user agent
func NewClients(userAgent string) (*Clients, error) {
	config, err := NewClientConfig()
	if err != nil {
		return nil, err
	}

	return &Clients{
		KubeClient:     kubernetes.NewForConfigOrDie(rest.AddUserAgent(config, userAgent)),
		ConfigClient:   configclient.NewForConfigOrDie(rest.AddUserAgent(config, userAgent)),
		OperatorClient: operatorclient.NewForConfigOrDie(rest.AddUserAgent(config, userAgent)),
	}, nil
}
