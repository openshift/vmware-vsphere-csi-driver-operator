package checks

import (
	"context"
	check "github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
)

type CheckArgs struct {
	vmConnection        []*check.VSphereConnection
	apiClient           KubeAPIInterface
	multiVCenterEnabled bool
}

func NewCheckArgs(connection []*check.VSphereConnection, apiClient KubeAPIInterface, multiVCenterEnabled bool) CheckArgs {
	// TODO: May need to update this w/ multiple vcenter connections.
	return CheckArgs{
		vmConnection:        connection,
		apiClient:           apiClient,
		multiVCenterEnabled: multiVCenterEnabled,
	}
}

type CheckInterface interface {
	Check(ctx context.Context, args CheckArgs) []ClusterCheckResult
}
