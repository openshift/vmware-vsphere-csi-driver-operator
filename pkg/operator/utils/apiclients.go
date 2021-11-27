package utils

import (
	cfgclientset "github.com/openshift/client-go/config/clientset/versioned"
	cfginformers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/dynamic"
	v1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
)

// Rather than passing individual apiclients to controllers
// ApiClients defines a type that can encapsulate all of those
type ApiClients struct {
	// Client for CSO's CR
	OperatorClient v1helpers.OperatorClientWithFinalizers
	// Kubernetes API client
	KubeClient kubernetes.Interface
	// Kubernetes API informers, per namespace
	KubeInformers v1helpers.KubeInformersForNamespaces

	SecretInformer v1.SecretInformer
	NodeInformer   v1.NodeInformer

	// config.openshift.io client
	ConfigClientSet cfgclientset.Interface
	// config.openshift.io informers
	ConfigInformers cfginformers.SharedInformerFactory

	// Dynamic client for OLM and old CSI operator APIs
	DynamicClient dynamic.Interface
}
