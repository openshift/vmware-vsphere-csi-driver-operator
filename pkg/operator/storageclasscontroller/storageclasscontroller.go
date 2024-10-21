package storageclasscontroller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	v1 "github.com/openshift/api/config/v1"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"

	clustercsidriverinformer "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	csiscc "github.com/openshift/library-go/pkg/operator/csi/csistorageclasscontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	storagev1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/klog/v2"
)

const (
	DatastoreInfoProperty = "info"
	SummaryProperty       = "summary"
)

var (
	defaultBackoff = wait.Backoff{
		Duration: time.Minute,
		Factor:   2,
		Jitter:   0.01,
		// Don't limit nr. of steps
		Steps: math.MaxInt32,
		// Maximum interval between checks.
		Cap: 30 * time.Minute,
	}
)

type StorageClassSyncInterface interface {
	Sync(ctx context.Context, connection []*vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) error
	SyncRemove(ctx context.Context) error
}

type AbstractStorageClass struct {
	StorageClassSyncInterface
	name                 string
	targetNamespace      string
	manifest             []byte
	kubeClient           kubernetes.Interface
	operatorClient       v1helpers.OperatorClient
	storageClassLister   storagev1.StorageClassLister
	recorder             events.Recorder
	makeStoragePolicyAPI func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure) vCenterInterface
	scStateEvaluator     *csiscc.StorageClassStateEvaluator

	policyName string
	backoff    wait.Backoff
	nextCheck  time.Time
	lastCheck  time.Time
}

type StorageClassController struct{ AbstractStorageClass }

func NewStorageClassController(
	name,
	targetNamespace string,
	manifest []byte,
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.OperatorClient,
	storageClassLister storagev1.StorageClassLister,
	clusterCSIDriverInformer clustercsidriverinformer.ClusterCSIDriverInformer,
	recorder events.Recorder,
) StorageClassSyncInterface {
	evaluator := csiscc.NewStorageClassStateEvaluator(
		kubeClient,
		clusterCSIDriverInformer.Lister(),
		recorder,
	)

	var c StorageClassSyncInterface
	scc := AbstractStorageClass{
		name:                 name,
		targetNamespace:      targetNamespace,
		manifest:             manifest,
		kubeClient:           kubeClient,
		operatorClient:       operatorClient,
		storageClassLister:   storageClassLister,
		recorder:             recorder,
		makeStoragePolicyAPI: NewStoragePolicyAPI,
		scStateEvaluator:     evaluator,
		backoff:              defaultBackoff,
		nextCheck:            time.Now(),
	}
	klog.V(2).Infof("Creating multi vcenter storage class controller")
	c = makeMultiVCenterStorageClassController(scc)

	return c
}

func (c *StorageClassController) Sync(ctx context.Context, connections []*vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) error {
	checkResultFunc := func() (checks.ClusterCheckResult, checks.ClusterCheckStatus) {
		sc := resourceread.ReadStorageClassV1OrDie(c.manifest)
		scState := c.scStateEvaluator.GetStorageClassState(sc.Provisioner)

		// This storage class controller only handles single vCenter.  It will only use the first connection (there should never be
		// more than 1)
		policyName, syncResult := c.syncStoragePolicy(ctx, connections[0], apiDeps, scState)
		if syncResult.CheckError != nil {
			klog.Errorf("error syncing storage policy: %v", syncResult.Reason)
			clusterCondition := "storage_class_sync_failed"
			utils.InstallErrorMetric.WithLabelValues(string(syncResult.CheckStatus), clusterCondition).Set(1)
			return syncResult, checks.ClusterCheckAllGood
		}
		c.policyName = policyName

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

func (c *StorageClassController) SyncRemove(ctx context.Context) error {
	scString := string(c.manifest)
	sc := resourceread.ReadStorageClassV1OrDie([]byte(scString))
	_, _, err := resourceapply.DeleteStorageClass(ctx, c.kubeClient.StorageV1(), c.recorder, sc)
	return err
}

func (c *AbstractStorageClass) syncStoragePolicy(ctx context.Context, connection *vclib.VSphereConnection, apiDeps checks.KubeAPIInterface, scState operatorapi.StorageClassStateName) (string, checks.ClusterCheckResult) {
	// if the SC is not managed, there is no need to sync the storage policy
	if !c.scStateEvaluator.IsManaged(scState) {
		return "", checks.MakeClusterCheckResultPass()
	}

	// if we are running the checks after creating the policy successfully
	// then lets run checks less frequently.
	if !time.Now().After(c.nextCheck) && len(c.policyName) > 0 {
		klog.V(4).Infof("Returning without running any checks")
		return c.policyName, checks.MakeClusterCheckResultPass()
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

func (c *AbstractStorageClass) updateConditions(ctx context.Context, lastCheckResult checks.ClusterCheckResult, clusterStatus checks.ClusterCheckStatus) error {
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

func (c *AbstractStorageClass) syncStorageClass(ctx context.Context, scState operatorapi.StorageClassStateName) error {
	scString := string(c.manifest)
	pairs := []string{
		"${STORAGE_POLICY_NAME}", c.policyName,
	}

	policyReplacer := strings.NewReplacer(pairs...)
	scString = policyReplacer.Replace(scString)

	sc := resourceread.ReadStorageClassV1OrDie([]byte(scString))
	err := csiscc.SetDefaultStorageClass(c.storageClassLister, sc)
	if err != nil {
		return err
	}

	return c.scStateEvaluator.ApplyStorageClass(ctx, sc, scState)
}
