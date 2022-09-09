package storageclasscontroller

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/openshift/api/config/v1"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	DatastoreInfoProperty = "info"
	SummaryProperty       = "summary"
)

type StorageClassController struct {
	name                 string
	targetNamespace      string
	manifest             []byte
	kubeClient           kubernetes.Interface
	operatorClient       v1helpers.OperatorClient
	recorder             events.Recorder
	StorageClassCreated  bool
	makeStoragePolicyAPI func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure) vCenterInterface
}

func NewStorageClassController(
	name,
	targetNamespace string,
	manifest []byte,
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.OperatorClient,
	recorder events.Recorder,
) *StorageClassController {
	c := &StorageClassController{
		name:                 name,
		targetNamespace:      targetNamespace,
		manifest:             manifest,
		kubeClient:           kubeClient,
		operatorClient:       operatorClient,
		recorder:             recorder,
		makeStoragePolicyAPI: newStoragePolicyAPI,
	}

	return c
}

func (c *StorageClassController) Sync(ctx context.Context, connection *vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) error {
	checkResultFunc := func() (checks.ClusterCheckResult, checks.ClusterCheckStatus) {
		policyName, syncResult := c.syncStoragePolicy(ctx, connection, apiDeps)
		if syncResult.CheckError != nil {
			klog.Errorf("error syncing storage policy: %v", syncResult.Reason)
			clusterCondition := "storage_class_sync_failed"
			utils.InstallErrorMetric.WithLabelValues(string(syncResult.CheckStatus), clusterCondition).Set(1)
			return syncResult, checks.ClusterCheckAllGood
		}

		err := c.syncStorageClass(ctx, policyName)
		if err != nil {
			klog.Errorf("error syncing storage class: %v", err)
			return checks.MakeClusterDegradedError(checks.CheckStatusOpenshiftAPIError, err), checks.ClusterCheckDegrade
		}
		c.StorageClassCreated = true
		return checks.MakeClusterCheckResultPass(), checks.ClusterCheckAllGood
	}

	checkResult, overallClusterStatus := checkResultFunc()
	return c.updateConditions(ctx, checkResult, overallClusterStatus)
}

func (c *StorageClassController) syncStoragePolicy(ctx context.Context, connection *vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) (string, checks.ClusterCheckResult) {
	infra := apiDeps.GetInfrastructure()

	apiClient := c.makeStoragePolicyAPI(ctx, connection, infra)

	// we expect all API calls to finish within apiTimeout or else operator might be stuck
	tctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	policyName, err := apiClient.createStoragePolicy(tctx)
	if err != nil {
		return "", checks.MakeGenericVCenterAPIError(err)
	}
	return policyName, checks.MakeClusterCheckResultPass()
}

func (c *StorageClassController) updateConditions(ctx context.Context, lastCheckResult checks.ClusterCheckResult, clusterStatus checks.ClusterCheckStatus) error {
	availableCnd := operatorapi.OperatorCondition{
		Type:   c.name + operatorapi.OperatorStatusTypeAvailable,
		Status: operatorapi.ConditionTrue,
	}

	degradedCondition := operatorapi.OperatorCondition{
		Type:   c.name + operatorapi.OperatorStatusTypeDegraded,
		Status: operatorapi.ConditionFalse,
	}

	switch clusterStatus {
	case checks.ClusterCheckDegrade:
		degradedMessage := fmt.Sprintf("Marking cluster degraded because %s", lastCheckResult.Reason)
		klog.Warningf(degradedMessage)
		c.recorder.Warningf(string(lastCheckResult.CheckStatus), "Marking cluster degraded because %s", lastCheckResult.Reason)
		degradedCondition.Status = operatorapi.ConditionTrue
		degradedCondition.Message = lastCheckResult.Reason
		degradedCondition.Reason = string(lastCheckResult.CheckStatus)

		// also mark available as false
		availableCnd.Status = operatorapi.ConditionFalse
		availableCnd.Message = degradedMessage
		availableCnd.Reason = string(lastCheckResult.Reason)
	default:
		klog.V(4).Infof("StorageClass install for vsphere CSI driver succeeded")
	}

	var updateFuncs []v1helpers.UpdateStatusFunc
	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(availableCnd))
	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(degradedCondition))
	if _, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, updateFuncs...); updateErr != nil {
		return updateErr
	}
	return nil
}

func (c *StorageClassController) syncStorageClass(ctx context.Context, policyName string) error {
	scString := string(c.manifest)
	pairs := []string{
		"${STORAGE_POLICY_NAME}", policyName,
	}

	policyReplacer := strings.NewReplacer(pairs...)
	scString = policyReplacer.Replace(scString)

	sc := resourceread.ReadStorageClassV1OrDie([]byte(scString))
	_, _, err := resourceapply.ApplyStorageClass(ctx, c.kubeClient.StorageV1(), c.recorder, sc)
	return err
}
