package checks

import (
	"context"

	check "github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
)

type CheckArgs struct {
	vmConnection []*check.VSphereConnection
	apiClient    KubeAPIInterface
}

func NewCheckArgs(connection []*check.VSphereConnection, apiClient KubeAPIInterface) CheckArgs {
	return CheckArgs{
		vmConnection: connection,
		apiClient:    apiClient,
	}
}

type CheckInterface interface {
	Check(ctx context.Context, args CheckArgs) []ClusterCheckResult
}
