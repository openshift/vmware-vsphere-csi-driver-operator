package checks

import (
	"context"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	check "github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
)

type CheckArgs struct {
	vmConnection []*check.VSphereConnection
	apiClient    KubeAPIInterface
}

func NewCheckArgs(connection []*check.VSphereConnection, apiClient KubeAPIInterface, gates featuregates.FeatureGate) CheckArgs {
	return CheckArgs{
		vmConnection: connection,
		apiClient:    apiClient,
	}
}

type CheckInterface interface {
	Check(ctx context.Context, args CheckArgs) []ClusterCheckResult
}
