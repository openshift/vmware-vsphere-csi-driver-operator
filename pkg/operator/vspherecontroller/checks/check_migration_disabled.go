package checks

import (
	"context"
	"fmt"

	operatorv1 "github.com/openshift/api/operator/v1"
)

const (
	storageOperatorName = "cluster"
)

type CheckMigrationDisabled struct{}

func (c *CheckMigrationDisabled) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	storageOperator, err := checkOpts.apiClient.GetStorage(storageOperatorName)
	if err != nil {
		checkResult := ClusterCheckResult{
			CheckStatus: CheckStatusOpenshiftAPIError,
			CheckError:  err,
			Action:      CheckActionDegrade,
			Reason:      fmt.Sprintf("failed to check Storage operator %s: %v", storageOperatorName, err),
		}
		return []ClusterCheckResult{checkResult}
	}
	// If CSI migration is explictly disabled, then block upgrade.
	// Future releases have migration enabled and can not be turned off.
	if storageOperator.Spec.VSphereStorageDriver == operatorv1.LegacyDeprecatedInTreeDriver {
		reason := fmt.Errorf("Storage operator %s must not have vsphereStorageDriver set to LegacyDeprecatedInTreeDriver", storageOperatorName)
		return []ClusterCheckResult{MakeClusterUnupgradeableError(CheckStatusCSIMigrationDisabled, reason)}
	}
	return []ClusterCheckResult{}
}
