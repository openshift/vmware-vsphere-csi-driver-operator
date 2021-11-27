package checks

import (
	ocpv1 "github.com/openshift/api/config/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelister "k8s.io/client-go/listers/core/v1"
	storagelister "k8s.io/client-go/listers/storage/v1"
)

type KubeApiInterface interface {
	// ListNodes returns list of all nodes in the cluster.
	ListNodes() ([]*v1.Node, error)
	GetCSIDriver(name string) (*storagev1.CSIDriver, error)
	ListCSINodes() ([]*storagev1.CSINode, error)
}

type KubeApiInterfaceImpl struct {
	Infrastructure  *ocpv1.Infrastructure
	NodeLister      corelister.NodeLister
	CSINodeLister   storagelister.CSINodeLister
	CSIDriverLister storagelister.CSIDriverLister
}

func (k *KubeApiInterfaceImpl) ListNodes() ([]*v1.Node, error) {
	return k.NodeLister.List(labels.Everything())
}

func (k *KubeApiInterfaceImpl) GetCSIDriver(name string) (*storagev1.CSIDriver, error) {
	return k.CSIDriverLister.Get(name)
}

func (k *KubeApiInterfaceImpl) ListCSINodes() ([]*storagev1.CSINode, error) {
	return k.CSINodeLister.List(labels.Everything())
}
