package storageclasscontroller

import (
	"context"
	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"k8s.io/klog/v2"
	"time"
)

type MultiVCenterStorageClassController struct {
	AbstractStorageClass
	connPolicyNames map[string]string // Key is vcenter name, value is policy name
}

func makeMultiVCenterStorageClassController(abstract AbstractStorageClass) *MultiVCenterStorageClassController {
	scc := MultiVCenterStorageClassController{
		AbstractStorageClass: abstract,
		connPolicyNames:      make(map[string]string),
	}
	scc.AbstractStorageClass.StorageClassSyncInterface = &scc
	return &scc
}

func (c *MultiVCenterStorageClassController) Sync(ctx context.Context, connections []*vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) error {
	checkResultFunc := func() (checks.ClusterCheckResult, checks.ClusterCheckStatus) {
		sc := resourceread.ReadStorageClassV1OrDie(c.manifest)
		scState := c.scStateEvaluator.GetStorageClassState(sc.Provisioner)

		// Iterate through each vcenter connection for storage policy.
		for _, connection := range connections {
			klog.V(4).Infof("Syncing %v", connection.Hostname)
			policyName, syncResult := c.syncStoragePolicy(ctx, connection, apiDeps, scState)
			if syncResult.CheckError != nil {
				klog.Errorf("error syncing storage policy: %v", syncResult.Reason)
				clusterCondition := "storage_class_sync_failed"
				utils.InstallErrorMetric.WithLabelValues(string(syncResult.CheckStatus), clusterCondition).Set(1)
				return syncResult, checks.ClusterCheckAllGood
			}
			klog.V(4).Infof("Synced policy %v", policyName)
			c.connPolicyNames[connection.Hostname] = policyName
			c.policyName = policyName // This is the global name of policy.  may need to make it more logical to not set in loop.
		}

		err := c.syncStorageClass(ctx, scState)
		if err != nil {
			klog.Errorf("error syncing storage class: %v", err)
			return checks.MakeClusterDegradedError(checks.CheckStatusOpenshiftAPIError, err), checks.ClusterCheckDegrade
		}
		return checks.MakeClusterCheckResultPass(), checks.ClusterCheckAllGood
	}

	checkResult, overallClusterStatus := checkResultFunc()
	return c.updateConditions(ctx, checkResult, overallClusterStatus)
}

func (c *MultiVCenterStorageClassController) syncStoragePolicy(ctx context.Context, connection *vclib.VSphereConnection, apiDeps checks.KubeAPIInterface, scState operatorapi.StorageClassStateName) (string, checks.ClusterCheckResult) {
	// if the SC is not managed, there is no need to sync the storage policy
	if !c.scStateEvaluator.IsManaged(scState) {
		klog.V(2).Info("sc is not managed")
		return "", checks.MakeClusterCheckResultPass()
	}

	// if we are running the checks after creating the policy successfully
	// then lets run checks less frequently.
	if !time.Now().After(c.nextCheck) && len(c.connPolicyNames[connection.Hostname]) > 0 {
		klog.V(4).Infof("Returning without running any checks")
		return c.connPolicyNames[connection.Hostname], checks.MakeClusterCheckResultPass()
	}

	infra := apiDeps.GetInfrastructure()
	apiClient := c.makeStoragePolicyAPI(ctx, connection, infra)

	// we expect all API calls to finish within apiTimeout or else operator might be stuck
	tctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	policyName, err := apiClient.createStoragePolicy(tctx)

	nextRunDelay := c.backoff.Step()
	c.lastCheck = time.Now()
	c.nextCheck = c.lastCheck.Add(nextRunDelay)

	if err != nil {
		return "", checks.MakeGenericVCenterAPIError(err)
	}
	return policyName, checks.MakeClusterCheckResultPass()
}
