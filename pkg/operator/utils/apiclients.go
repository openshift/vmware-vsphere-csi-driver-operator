package utils

import (
	cfgclientset "github.com/openshift/client-go/config/clientset/versioned"
	cfginformers "github.com/openshift/client-go/config/informers/externalversions"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	clustercsidriverinformer "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/dynamic"
	v1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
)

// Rather than passing individual apiclients to controllers
// APIClient defines a type that can encapsulate all of those
type APIClient struct {
	// Client for CSO's CR
	OperatorClient v1helpers.OperatorClientWithFinalizers
	// Kubernetes API client
	KubeClient kubernetes.Interface
	// API extension client
	ApiExtClient apiextclient.Interface
	// Kubernetes API informers, per namespace
	KubeInformers v1helpers.KubeInformersForNamespaces

	SecretInformer    v1.SecretInformer
	ConfigMapInformer v1.ConfigMapInformer
	NodeInformer      v1.NodeInformer

	// config.openshift.io client
	ConfigClientSet cfgclientset.Interface
	// config.openshift.io informers
	ConfigInformers cfginformers.SharedInformerFactory

	// a more specific version of Openshift operator informers.
	OCPOperatorInformers     operatorinformers.SharedInformerFactory
	ClusterCSIDriverInformer clustercsidriverinformer.ClusterCSIDriverInformer

	// Dynamic client for OLM and old CSI operator APIs
	DynamicClient dynamic.Interface
}
