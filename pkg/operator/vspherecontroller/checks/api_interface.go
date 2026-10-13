package checks

import (
	ocpv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	clustercsidriverlister "github.com/openshift/client-go/operator/listers/operator/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelister "k8s.io/client-go/listers/core/v1"
	storagelister "k8s.io/client-go/listers/storage/v1"
)

type KubeAPIInterface interface {
	// ListLinuxNodes returns list of all linux nodes in the cluster.
	ListLinuxNodes() ([]*v1.Node, error)
	GetCSIDriver(name string) (*storagev1.CSIDriver, error)
	GetClusterCSIDriver(name string) (*opv1.ClusterCSIDriver, error)
	ListCSINodes() ([]*storagev1.CSINode, error)
	GetStorageClass(name string) (*storagev1.StorageClass, error)
	GetInfrastructure() *ocpv1.Infrastructure
}

type KubeAPIInterfaceImpl struct {
	Infrastructure         *ocpv1.Infrastructure
	NodeLister             corelister.NodeLister
	CSINodeLister          storagelister.CSINodeLister
	CSIDriverLister        storagelister.CSIDriverLister
	ClusterCSIDriverLister clustercsidriverlister.ClusterCSIDriverLister
	StorageClassLister     storagelister.StorageClassLister
}

func getLinuxNodeSelector() labels.Selector {
	labelSelector := metav1.LabelSelector{
		MatchLabels: map[string]string{
			"kubernetes.io/os": "linux",
		},
	}
	selector, _ := metav1.LabelSelectorAsSelector(&labelSelector)
	return selector
}

func (k *KubeAPIInterfaceImpl) ListLinuxNodes() ([]*v1.Node, error) {
	return k.NodeLister.List(getLinuxNodeSelector())
}

func (k *KubeAPIInterfaceImpl) GetCSIDriver(name string) (*storagev1.CSIDriver, error) {
	return k.CSIDriverLister.Get(name)
}

func (k *KubeAPIInterfaceImpl) GetClusterCSIDriver(name string) (*opv1.ClusterCSIDriver, error) {
	return k.ClusterCSIDriverLister.Get(name)
}

func (k *KubeAPIInterfaceImpl) ListCSINodes() ([]*storagev1.CSINode, error) {
	return k.CSINodeLister.List(labels.Everything())
}

func (k *KubeAPIInterfaceImpl) GetStorageClass(name string) (*storagev1.StorageClass, error) {
	return k.StorageClassLister.Get(name)
}

func (k *KubeAPIInterfaceImpl) GetInfrastructure() *ocpv1.Infrastructure {
	return k.Infrastructure
}
