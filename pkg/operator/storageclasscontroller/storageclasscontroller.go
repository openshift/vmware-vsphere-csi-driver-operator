package storageclasscontroller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	v1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
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

	successCheckInterval = 10 * time.Minute

	// cleanupApiTimeout bounds each cleanup-connection sync separately from the normal
	// apiTimeout (10m). A cleanup connection that TCP-connects but hangs on API calls (e.g. a
	// vCenter in a bad intermediate state) would otherwise be able to add up to apiTimeout of
	// latency per removed vCenter, per sync, to a single sync cycle.
	cleanupApiTimeout = 2 * time.Minute
)

type StorageClassSyncInterface interface {
	Sync(ctx context.Context, connections []*vclib.VSphereConnection, cleanupConnections []*vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) error
	SyncRemove(ctx context.Context) error
	// IsHostFullyClean reports whether host has no remaining storage-policy/orphan-cleanup state,
	// i.e. it is safe for the caller to stop tracking it as pending removal.
	IsHostFullyClean(host string) bool
	// PurgeVCenterState drops all bookkeeping (policy, backoff, pending-orphan state) held for
	// host. Callers use this once cleanup is confirmed done or a bounded retry has been abandoned.
	PurgeVCenterState(host string)
}

type vCenterBackoffState struct {
	backoff   wait.Backoff
	nextCheck time.Time
	lastCheck time.Time
}

type StorageClassController struct {
	StorageClassSyncInterface
	name                 string
	targetNamespace      string
	manifest             []byte
	kubeClient           kubernetes.Interface
	operatorClient       v1helpers.OperatorClient
	storageClassLister   storagev1.StorageClassLister
	recorder             events.Recorder
	featureGates         featuregates.FeatureGate
	makeStoragePolicyAPI func(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface
	scStateEvaluator     *csiscc.StorageClassStateEvaluator

	sharedPolicyName     string
	vCenterStoragePolicy map[string]string               // Key is vcenter hostname, value is policy name
	backoffStates        map[string]*vCenterBackoffState // Key is vcenter hostname
	pendingOrphans       map[string]int                  // Key is vcenter hostname, value is unresolved count
}

func NewStorageClassController(
	name,
	targetNamespace string,
	manifest []byte,
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.OperatorClient,
	storageClassLister storagev1.StorageClassLister,
	clusterCSIDriverInformer clustercsidriverinformer.ClusterCSIDriverInformer,
	recorder events.Recorder,
	gates featuregates.FeatureGate,
) StorageClassSyncInterface {
	evaluator := csiscc.NewStorageClassStateEvaluator(
		kubeClient,
		clusterCSIDriverInformer.Lister(),
		recorder,
	)

	scc := &StorageClassController{
		name:                 name,
		targetNamespace:      targetNamespace,
		manifest:             manifest,
		kubeClient:           kubeClient,
		operatorClient:       operatorClient,
		storageClassLister:   storageClassLister,
		recorder:             recorder,
		featureGates:         gates,
		makeStoragePolicyAPI: NewStoragePolicyAPI,
		scStateEvaluator:     evaluator,
		vCenterStoragePolicy: make(map[string]string),
		backoffStates:        make(map[string]*vCenterBackoffState),
		pendingOrphans:       make(map[string]int),
	}

	return scc
}

func (c *StorageClassController) Sync(ctx context.Context, connections []*vclib.VSphereConnection, cleanupConnections []*vclib.VSphereConnection, apiDeps checks.KubeAPIInterface) error {
	sc := resourceread.ReadStorageClassV1OrDie(c.manifest)
	scState := c.scStateEvaluator.GetStorageClassState(sc.Provisioner)

	checkResultFunc := func() (checks.ClusterCheckResult, checks.ClusterCheckStatus) {
		// Iterate through each vcenter connection for storage policy.
		for _, connection := range connections {
			klog.V(4).Infof("Syncing %v", connection.Hostname)
			policyName, syncResult := c.syncStoragePolicy(ctx, connection, apiDeps, scState, false)
			if syncResult.CheckError != nil {
				klog.Errorf("error syncing storage policy for %v: %v", connection.Hostname, syncResult.Reason)
				clusterCondition := "storage_class_sync_failed"
				utils.InstallErrorMetric.WithLabelValues(string(syncResult.CheckStatus), clusterCondition).Set(1)
				return syncResult, checks.ClusterCheckDegrade
			}
			klog.V(4).Infof("Synced policy %v", policyName)
			// Only update the shared policy name from vCenters that have active failure domains.
			// A vCenter with zero FDs returns empty string after deleting its orphaned profile.
			if policyName != "" {
				c.sharedPolicyName = policyName
			}
		}

		err := c.syncStorageClass(ctx, scState)
		if err != nil {
			klog.Errorf("error syncing storage class: %v", err)
			return checks.MakeClusterDegradedError(checks.CheckStatusOpenshiftAPIError, err), checks.ClusterCheckDegrade
		}
		return checks.MakeClusterCheckResultPass(), checks.ClusterCheckAllGood
	}

	// connections only - cleanupConnections are best-effort and must never degrade the cluster,
	// so they are deliberately not part of this result.
	checkResult, overallClusterStatus := checkResultFunc()

	// Cleanup connections (removed vCenters being reconnected to for best-effort tag/SPBM-profile
	// cleanup) run through the same syncStoragePolicy()/createStoragePolicy() path - the existing
	// zero-FD branch there already deletes the profile and detaches orphaned tags. Errors here
	// only affect this vCenter's own retry bookkeeping (pendingOrphans, backoffStates), never
	// checkResult/overallClusterStatus.
	for _, cleanupConn := range cleanupConnections {
		if cleanupConn == nil {
			continue
		}
		klog.V(4).Infof("Syncing cleanup connection for removed vCenter %v", cleanupConn.Hostname)
		cctx, cancel := context.WithTimeout(ctx, cleanupApiTimeout)
		_, syncResult := c.syncStoragePolicy(cctx, cleanupConn, apiDeps, scState, true)
		cancel()
		if syncResult.CheckError != nil {
			klog.Warningf("best-effort cleanup sync failed for removed vCenter %s: %v", cleanupConn.Hostname, syncResult.Reason)
		}
	}

	totalPending := 0
	for _, count := range c.pendingOrphans {
		totalPending += count
	}

	return c.updateConditions(ctx, checkResult, overallClusterStatus, totalPending)
}

// IsHostFullyClean reports true once host has no remaining storage-policy state: no (non-empty)
// tracked SPBM policy name, and no orphaned tags still blocked on bound PVs. VSphereController
// calls this after Sync() returns, for each host it attempted cleanup on, to decide whether to
// stop retrying it.
func (c *StorageClassController) IsHostFullyClean(host string) bool {
	if policyName, ok := c.vCenterStoragePolicy[host]; ok && policyName != "" {
		return false
	}
	return c.pendingOrphans[host] == 0
}

// PurgeVCenterState drops all state Sync() tracks for host: its storage policy name, backoff
// state, and pending-orphan count. Callers (VSphereController) call this only once cleanup is
// confirmed done (IsHostFullyClean) or a bounded retry has been abandoned - never merely because
// host is momentarily absent from the connections passed to Sync().
func (c *StorageClassController) PurgeVCenterState(host string) {
	delete(c.vCenterStoragePolicy, host)
	delete(c.backoffStates, host)
	delete(c.pendingOrphans, host)
}

func (c *StorageClassController) SyncRemove(ctx context.Context) error {
	scString := string(c.manifest)
	sc := resourceread.ReadStorageClassV1OrDie([]byte(scString))
	_, _, err := resourceapply.DeleteStorageClass(ctx, c.kubeClient.StorageV1(), c.recorder, sc)
	return err
}

func (c *StorageClassController) getBackoffState(hostname string) *vCenterBackoffState {
	state, ok := c.backoffStates[hostname]
	if !ok {
		state = &vCenterBackoffState{
			backoff: defaultBackoff,
		}
		c.backoffStates[hostname] = state
	}
	return state
}

func (c *StorageClassController) syncStoragePolicy(ctx context.Context, connection *vclib.VSphereConnection, apiDeps checks.KubeAPIInterface, scState operatorapi.StorageClassStateName, isCleanup bool) (string, checks.ClusterCheckResult) {
	if !c.scStateEvaluator.IsManaged(scState) {
		klog.V(2).Info("sc is not managed")
		return "", checks.MakeClusterCheckResultPass()
	}

	bs := c.getBackoffState(connection.Hostname)

	day2Enabled := c.featureGates != nil && c.featureGates.Enabled(features.FeatureGateVSphereMultiVCenterDay2)

	forceCleanup := false
	if day2Enabled {
		if ccd, err := apiDeps.GetClusterCSIDriver(utils.VSphereDriverName); err == nil && ccd != nil && ccd.Annotations != nil {
			forceCleanup = ccd.Annotations["csi.vsphere.vmware.com/force-orphan-cleanup"] == "true"
		}
	}

	// Cleanup connections never honor the backoff window: this hostname's backoff state may
	// still carry a nextCheck set far in the future by its last successful sync *while it was
	// still an active connection*. Skipping the check here would silently no-op the cleanup
	// sync - never calling createStoragePolicy - until that stale window happens to elapse.
	// Cleanup connections are rare, best-effort and already bounded by cleanupApiTimeout, so
	// always running the real check is cheap and correct.
	if !isCleanup && !time.Now().After(bs.nextCheck) && len(c.vCenterStoragePolicy[connection.Hostname]) > 0 && !forceCleanup && c.pendingOrphans[connection.Hostname] == 0 {
		klog.V(4).Infof("Returning without running any checks for %s", connection.Hostname)
		return c.vCenterStoragePolicy[connection.Hostname], checks.MakeClusterCheckResultPass()
	}

	infra := apiDeps.GetInfrastructure()

	apiClient := c.makeStoragePolicyAPI(ctx, connection, infra, day2Enabled, forceCleanup, c.recorder)

	tctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	policyName, err := apiClient.createStoragePolicy(tctx)

	// Track unresolved orphans for this vCenter (used for OrphanCleanupPending condition)
	if spa, ok := apiClient.(*storagePolicyAPI); ok {
		c.pendingOrphans[connection.Hostname] = len(spa.unresolvedOrphans)
	}

	bs.lastCheck = time.Now()
	if err != nil {
		bs.nextCheck = bs.lastCheck.Add(bs.backoff.Step())
		return "", checks.MakeGenericVCenterAPIError(err)
	}

	bs.backoff = defaultBackoff
	bs.nextCheck = bs.lastCheck.Add(successCheckInterval)
	// Recorded here (rather than by the caller) so that cleanup-connection syncs - which discard
	// the returned policyName - also refresh this vCenter's tracked state. Otherwise a removed
	// vCenter would keep its stale, pre-removal (non-empty) policy name forever, and
	// IsHostFullyClean would never report it clean even after the profile is deleted.
	c.vCenterStoragePolicy[connection.Hostname] = policyName
	return policyName, checks.MakeClusterCheckResultPass()
}

func (c *StorageClassController) updateConditions(ctx context.Context, lastCheckResult checks.ClusterCheckResult, clusterStatus checks.ClusterCheckStatus, pendingOrphans int) error {
	availableCnd := operatorapi.OperatorCondition{
		Type:   c.name + operatorapi.OperatorStatusTypeAvailable,
		Status: operatorapi.ConditionTrue,
	}

	degradedCondition := operatorapi.OperatorCondition{
		Type:   c.name + operatorapi.OperatorStatusTypeDegraded,
		Status: operatorapi.ConditionFalse,
	}

	orphanCondition := operatorapi.OperatorCondition{
		Type:   c.name + "OrphanCleanupPending",
		Status: operatorapi.ConditionFalse,
		Reason: "NoOrphans",
	}
	if pendingOrphans > 0 {
		orphanCondition.Status = operatorapi.ConditionTrue
		orphanCondition.Reason = "OrphansBlocked"
		orphanCondition.Message = fmt.Sprintf("%d orphaned datastore tag(s) could not be cleaned up", pendingOrphans)
	}

	switch clusterStatus {
	case checks.ClusterCheckDegrade:
		degradedMessage := fmt.Sprintf("Marking cluster degraded because %s", lastCheckResult.Reason)
		klog.Warningf("%s", degradedMessage)
		c.recorder.Warningf(string(lastCheckResult.CheckStatus), "Marking cluster degraded because %s", lastCheckResult.Reason)
		degradedCondition.Status = operatorapi.ConditionTrue
		degradedCondition.Message = lastCheckResult.Reason
		degradedCondition.Reason = string(lastCheckResult.CheckStatus)

		availableCnd.Status = operatorapi.ConditionFalse
		availableCnd.Message = degradedMessage
		availableCnd.Reason = string(lastCheckResult.Reason)
	default:
		klog.V(4).Infof("StorageClass install for vsphere CSI driver succeeded")
	}

	var updateFuncs []v1helpers.UpdateStatusFunc
	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(availableCnd))
	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(degradedCondition))
	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(orphanCondition))
	if _, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, updateFuncs...); updateErr != nil {
		return updateErr
	}
	return nil
}

func (c *StorageClassController) syncStorageClass(ctx context.Context, scState operatorapi.StorageClassStateName) error {
	scString := string(c.manifest)
	pairs := []string{
		"${STORAGE_POLICY_NAME}", c.sharedPolicyName,
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
