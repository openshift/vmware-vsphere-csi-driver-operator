package checks

import (
	"context"
	check "github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
)

type CheckArgs struct {
	vmConnection *check.VSphereConnection
	apiClient    KubeApiInterface
}

func NewCheckArgs(connection *check.VSphereConnection, apiClient KubeApiInterface) CheckArgs {
	return CheckArgs{
		vmConnection: connection,
		apiClient:    apiClient,
	}
}

type CheckInterface interface {
	Check(ctx context.Context, args CheckArgs) ClusterCheckResult
}
