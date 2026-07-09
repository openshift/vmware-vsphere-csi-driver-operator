package vspherecontroller

import (
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	ocpv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	operatorapi "github.com/openshift/api/operator/v1"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	clustercsidriverlister "github.com/openshift/client-go/operator/listers/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/storageclasscontroller"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	storagelister "k8s.io/client-go/listers/storage/v1"
	"k8s.io/klog/v2"
)

type VSphereController struct {
	name                     string
	targetNamespace          string
	secretManifest           []byte
	eventRecorder            events.Recorder
	kubeClient               kubernetes.Interface
	operatorClient           v1helpers.OperatorClientWithFinalizers
	configMapLister          corelister.ConfigMapLister
	secretLister             corelister.SecretLister
	scLister                 storagelister.StorageClassLister
	clusterCSIDriverLister   clustercsidriverlister.ClusterCSIDriverLister
	infraLister              infralister.InfrastructureLister
	nodeLister               corelister.NodeLister
	csiDriverLister          storagelister.CSIDriverLister
	csiNodeLister            storagelister.CSINodeLister
	apiClients               utils.APIClient
	controllers              []conditionalController
	storageClassController   storageclasscontroller.StorageClassSyncInterface
	operandControllerStarted bool
	vSphereConnections       []*vclib.VSphereConnection
	csiConfigManifest        []byte
	vSphereChecker           vSphereEnvironmentCheckInterface
	vCenterConnectionStatus  bool
	featureGates             featuregates.FeatureGate
	cloudConfig              *vclib.VSphereConfig

	currentManagmentState operatorapi.ManagementState

	// creates a new vSphereConnection - mainly used for testing
	vsphereConnectionFunc func() ([]*vclib.VSphereConnection, checks.ClusterCheckResult, bool)

	// vCenterConfigSnapshots caches {Hostname, Insecure} per vCenter host while it is still
	// active in cloudConfig, so we can still reconnect to it after it disappears from
	// infra.Spec.PlatformSpec.VSphere.VCenters (and thus from cloudConfig.Config.VirtualCenter).
	vCenterConfigSnapshots map[string]vCenterConnSnapshot
	// previousVCenterHosts is the set of vCenter hosts seen active on the previous sync, used to
	// detect hosts that disappeared from VCenters between syncs.
	previousVCenterHosts map[string]bool
	// pendingVCenterRemoval tracks removed vCenters that are being reconnected to for best-effort
	// tag/SPBM-profile cleanup, bounded by classifyConnectError/maxTransientRetryWindow.
	pendingVCenterRemoval map[string]*vCenterRemovalState

	// secondaryVCenterUnreachable/secondaryVCenterMessage reflect whether any non-workspace
	// vCenter failed to connect on the most recent sync (Phase 6 fault isolation). They never
	// cause a cluster degrade; they only drive the SecondaryVCenterUnreachable condition.
	secondaryVCenterUnreachable bool
	secondaryVCenterMessage     string
}

// vCenterConnSnapshot is a snapshot of the connection settings needed to reconnect to a vCenter
// after it has been removed from the live cloud config.
type vCenterConnSnapshot struct {
	Hostname string
	Insecure bool
}

// connectFailureClass classifies a reconnect failure against a removed vCenter, so that
// reconcileRemovedVCenters can tell a permanent (mis-)configuration problem apart from a vCenter
// that is simply unreachable right now (e.g. mid maintenance window).
type connectFailureClass string

const (
	// failureClassTransient covers network/timeout/TLS/DNS errors - indistinguishable from a
	// vCenter that is mid-maintenance. Retried until maxTransientRetryWindow elapses.
	failureClassTransient connectFailureClass = "transient"
	// failureClassPermanent covers rejected/missing credentials. Maintenance never removes
	// secret keys or invalidates a password, so there is no window to wait out.
	failureClassPermanent connectFailureClass = "permanent"
)

// vCenterConnectFailure records a non-critical (secondary vCenter) connection failure detected
// during createVCenterConnection/loginToVCenter, so the caller can log/event/condition it without
// aborting the sync for the other, healthy vCenters (Phase 6 fault isolation).
type vCenterConnectFailure struct {
	host string
	err  error
}

// vCenterRemovalState tracks bounded-retry bookkeeping for a single removed vCenter that is
// pending best-effort cleanup.
type vCenterRemovalState struct {
	firstDetected time.Time
	attempts      int
	lastError     error
	lastClass     connectFailureClass
}

// maxTransientRetryWindow bounds how long we keep retrying a removed vCenter that is merely
// unreachable (as opposed to one whose credentials are known-bad). Reported vSphere maintenance
// windows run 6-12h; this is comfortably more than double that. Package-level var so tests can
// override it instead of sleeping.
var maxTransientRetryWindow = 48 * time.Hour

const (
	eventVCenterCleanupStarted      = "VCenterCleanupStarted"
	eventVCenterRemovalCancelled    = "VCenterRemovalCancelled"
	eventVCenterCleanupSucceeded    = "VCenterCleanupSucceeded"
	eventVCenterCleanupAbandoned    = "VCenterCleanupAbandoned"
	eventSecondaryVCenterUnreach    = "SecondaryVCenterUnreachable"
	conditionVCenterRemovalPending  = "VCenterRemovalPending"
	conditionSecondaryVCenterUnrch  = "SecondaryVCenterUnreachable"

	metricResultSuccess   = "success"
	metricResultAbandoned = "abandoned"
)

const (
	cloudConfigNamespace              = "openshift-config"
	infraGlobalName                   = "cluster"
	legacyConfigMapName               = "vsphere-csi-config"
	cloudCredSecretName               = "vmware-vsphere-cloud-credentials"
	metricsCertSecretName             = "vmware-vsphere-csi-driver-controller-metrics-serving-cert"
	webhookSecretName                 = "vmware-vsphere-csi-driver-webhook-secret"
	trustedCAConfigMap                = "vmware-vsphere-csi-driver-trusted-ca-bundle"
	driverConfigSecretName            = "vsphere-csi-config-secret"
	defaultNamespace                  = "openshift-cluster-csi-drivers"
	driverOperandName                 = "vmware-vsphere-csi-driver"
	resyncDuration                    = 20 * time.Minute
	envVMWareVsphereDriverSyncerImage = "VMWARE_VSPHERE_SYNCER_IMAGE"
	storageClassControllerName        = "VMwareVSphereDriverStorageClassController"
	storageClassName                  = "thin-csi"
)

var reEscape = regexp.MustCompile(`["\\]`)

type conditionalControllerInterface interface {
	Run(ctx context.Context, workers int)
}

type conditionalController struct {
	name       string
	controller conditionalControllerInterface
}

func NewVSphereController(
	name, targetNamespace string,
	apiClients utils.APIClient,
	csiConfigManifest []byte,
	secretManifest []byte,
	recorder events.Recorder,
	gates featuregates.FeatureGate,
) factory.Controller {
	kubeInformers := apiClients.KubeInformers
	ocpConfigInformer := apiClients.ConfigInformers
	configMapInformer := kubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
	infraInformer := ocpConfigInformer.Config().V1().Infrastructures()
	scInformer := kubeInformers.InformersFor("").Storage().V1().StorageClasses()
	csiDriverLister := kubeInformers.InformersFor("").Storage().V1().CSIDrivers().Lister()
	csiNodeLister := kubeInformers.InformersFor("").Storage().V1().CSINodes().Lister()
	nodeLister := apiClients.NodeInformer.Lister()

	rc := recorder.WithComponentSuffix("vmware-" + strings.ToLower(name))

	c := &VSphereController{
		name:                    name,
		targetNamespace:         targetNamespace,
		kubeClient:              apiClients.KubeClient,
		operatorClient:          apiClients.OperatorClient,
		configMapLister:         configMapInformer.Lister(),
		secretLister:            apiClients.SecretInformer.Lister(),
		csiNodeLister:           csiNodeLister,
		scLister:                scInformer.Lister(),
		csiDriverLister:         csiDriverLister,
		nodeLister:              nodeLister,
		apiClients:              apiClients,
		eventRecorder:           rc,
		vSphereChecker:          newVSphereEnvironmentChecker(),
		secretManifest:          secretManifest,
		csiConfigManifest:       csiConfigManifest,
		clusterCSIDriverLister:  apiClients.ClusterCSIDriverInformer.Lister(),
		infraLister:             infraInformer.Lister(),
		vCenterConnectionStatus: false,
		featureGates:            gates,
		vCenterConfigSnapshots:  make(map[string]vCenterConnSnapshot),
		previousVCenterHosts:    make(map[string]bool),
		pendingVCenterRemoval:   make(map[string]*vCenterRemovalState),
	}
	c.controllers = []conditionalController{}
	c.createCSIDriver()
	c.createWebHookController()
	c.storageClassController = c.createStorageClassController()

	return factory.New().WithInformers(
		apiClients.OperatorClient.Informer(),
		configMapInformer.Informer(),
		apiClients.SecretInformer.Informer(),
		infraInformer.Informer(),
		scInformer.Informer(),
		apiClients.ClusterCSIDriverInformer.Informer(),
	).WithSync(c.sync).
		ResyncEvery(resyncDuration).
		WithSyncDegradedOnError(apiClients.OperatorClient).ToController(c.name, rc)
}

func (c *VSphereController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	klog.V(4).Infof("%s: sync started", c.name)
	defer klog.V(4).Infof("%s: sync complete", c.name)
	opSpec, opStatus, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	infra, err := c.infraLister.Get(infraGlobalName)
	if err != nil {
		return err
	}

	if infra.Status.PlatformStatus == nil {
		klog.V(4).Infof("Unknown platform: infrastructure status.platformStatus is nil")
		return nil
	}

	if infra.Status.PlatformStatus.Type != ocpv1.VSpherePlatformType {
		klog.V(4).Infof("Unsupported platform: infrastructure status.platformStatus.type is %s", infra.Status.PlatformStatus.Type)
		return nil
	}

	if opSpec.ManagementState != operatorapi.Managed {
		klog.Warningf("%s: ManagementState is %s, skipping", c.name, opSpec.ManagementState)
		if opSpec.ManagementState == operatorapi.Removed {
			// if previously we were managing the operator and now we are not, then we should restart the operator
			if c.currentManagmentState == operatorapi.Managed {
				klog.Errorf("Operator is being removed, restarting the operator")
				os.Exit(0)
			}
			// if we are in removed state, we should remove all conditions
			return c.removeOperands(ctx, opStatus)
		}
		return nil
	}

	c.currentManagmentState = opSpec.ManagementState

	clusterCSIDriver, err := c.clusterCSIDriverLister.Get(utils.VSphereDriverName)
	if err != nil {
		return err
	}

	utils.UpdateMetrics(infra, clusterCSIDriver)

	driverCheckFlag, err := c.driverAlreadyStarted(ctx)
	if err != nil {
		return err
	}

	// if driver was previously started, then start it even if checks are failing
	if driverCheckFlag && !c.operandControllerStarted {
		go c.runConditionalController(ctx)
		c.operandControllerStarted = true
	}

	var connectionResult checks.ClusterCheckResult
	logout := true
	var cleanupConnections []*vclib.VSphereConnection

	// Load config when it has changed or if first time syncing.  For now, we do every time, but in future maybe limit
	// this to only when changed so that we can reduce logging messages.
	c.cloudConfig, err = c.loadCloudConfig(infra)
	if err != nil {
		return err
	}
	c.refreshVCenterConfigSnapshots()

	// Update infra so we have failure domains in the case of an older cluster with out-dated infra definition.
	// The following logic is borrowed from VPD.  We should make util project contain this so its shared and kept in sync
	infra = infra.DeepCopy() // ConvertToPlatformSpec modifies the object in place
	ConvertToPlatformSpec(c.cloudConfig, infra)

	// We no longer use the ConfigMap to store the vSphere config, so make sure to delete it
	c.deleteConfigMapIfExists(ctx, legacyConfigMapName, c.targetNamespace)

	if c.vsphereConnectionFunc != nil {
		c.vSphereConnections, connectionResult, logout = c.vsphereConnectionFunc()
	} else {
		connectionResult = c.loginToVCenter(ctx, infra)
	}

	day2Enabled := c.featureGates != nil && c.featureGates.Enabled(features.FeatureGateVSphereMultiVCenterDay2)
	if day2Enabled {
		cleanupConnections = c.reconcileRemovedVCenters(ctx, infra)
	}

	defer func() {
		klog.V(4).Infof("%s: vcenter-csi logging out from vcenter", c.name)
		for _, vConn := range c.vSphereConnections {
			if vConn != nil && logout {
				err := vConn.Logout(ctx)
				if err != nil {
					klog.Errorf("%s: error closing connection to vCenter API: %v", c.name, err)
				}
			}
		}
		// Cleanup connections are rebuilt from scratch every sync via connectToVCenterHost, so
		// there is no reason to keep the session alive across syncs - always log out.
		for _, vConn := range cleanupConnections {
			if vConn != nil {
				if err := vConn.Logout(ctx); err != nil {
					klog.Errorf("%s: error closing cleanup connection to vCenter API: %v", c.name, err)
				}
			}
		}
		c.vSphereConnections = nil
	}()

	if day2Enabled {
		if err := c.updateSecondaryVCenterCondition(ctx); err != nil {
			klog.Errorf("%s: error updating %s condition: %v", c.name, conditionSecondaryVCenterUnrch, err)
		}
		if err := c.updateVCenterRemovalPendingCondition(ctx); err != nil {
			klog.Errorf("%s: error updating %s condition: %v", c.name, conditionVCenterRemovalPending, err)
		}
	}

	// if we successfully connected to vCenter and previously we couldn't and operator has one or more
	// error conditions, then lets reset exp. backoff so as we can run the full cluster checks
	if connectionResult.CheckError == nil && hasErrorConditions(*opStatus) && !c.vCenterConnectionStatus {
		klog.Infof("resetting exp. backoff after connection established")
		c.vSphereChecker.ResetExpBackoff()
	}

	if connectionResult.CheckError == nil {
		c.vCenterConnectionStatus = true
	} else {
		klog.V(2).Infof("Marking vCenter connection status as false")
		c.vCenterConnectionStatus = false
	}

	blockCSIDriverInstall, err := c.installCSIDriver(ctx, syncContext, infra, clusterCSIDriver, connectionResult, opStatus)
	if err != nil {
		return err
	}

	// only install CSI storageclass if blockCSIDriverInstall is false and CSI driver has been installed.
	if !blockCSIDriverInstall && c.operandControllerStarted {
		storageClassAPIDeps := c.getCheckAPIDependency(infra)
		err = c.storageClassController.Sync(ctx, c.vSphereConnections, cleanupConnections, storageClassAPIDeps)
		// storageclass sync will only return error if somehow updating conditions fails, in which case
		// we can return error here and degrade the cluster
		if err != nil {
			return err
		}
		if day2Enabled {
			c.finalizeCleanupState(cleanupConnections)
		}
	}

	return nil
}

func (c *VSphereController) installCSIDriver(
	ctx context.Context,
	syncContext factory.SyncContext,
	infra *ocpv1.Infrastructure,
	clusterCSIDriver *operatorapi.ClusterCSIDriver,
	connectionResult checks.ClusterCheckResult,
	opStatus *operatorapi.OperatorStatus) (blockCSIDriverInstall bool, err error) {

	// if there was an OCP error we should degrade the cluster or if we previously created CSIDriver
	// but we can't connect to vcenter now, we should also degrade the cluster
	var connectionBlockUpgrade bool
	err, blockCSIDriverInstall, connectionBlockUpgrade = c.blockUpgradeOrDegradeCluster(ctx, connectionResult, infra, opStatus)
	if err != nil {
		return blockCSIDriverInstall, err
	}

	if blockCSIDriverInstall {
		return blockCSIDriverInstall, nil
	}

	err = c.createCSISecret(ctx, syncContext, infra, clusterCSIDriver)

	if err != nil {
		return blockCSIDriverInstall, err
	}

	delay, result, checkRan := c.runClusterCheck(ctx, infra)
	// if checks did not run
	if !checkRan {
		return blockCSIDriverInstall, nil
	}
	queue := syncContext.Queue()
	queueKey := syncContext.QueueKey()

	klog.V(2).Infof("Scheduled the next check in %s", delay)
	time.AfterFunc(delay, func() {
		queue.Add(queueKey)
	})

	var clusterCheckBlockUpgrade bool
	err, blockCSIDriverInstall, clusterCheckBlockUpgrade = c.blockUpgradeOrDegradeCluster(ctx, result, infra, opStatus)
	if err != nil {
		return blockCSIDriverInstall, err
	}

	// if checks failed, we should exit potentially without starting CSI driver
	if blockCSIDriverInstall {
		return blockCSIDriverInstall, nil
	}

	blockUpgrade := connectionBlockUpgrade || clusterCheckBlockUpgrade
	// All checks succeeded, reset any error metrics
	if !blockUpgrade {
		utils.InstallErrorMetric.Reset()
	}

	// if operand was not started previously and block upgrade is false and clusterdegrade is also false
	// then and only then we should start CSI driver operator
	if !c.operandControllerStarted && !blockCSIDriverInstall {
		go c.runConditionalController(ctx)
		c.operandControllerStarted = true
	}
	upgradeableStatus := operatorapi.ConditionTrue
	if blockUpgrade {
		upgradeableStatus = operatorapi.ConditionFalse
	}
	return blockCSIDriverInstall, c.updateConditions(ctx, c.name, result, opStatus, upgradeableStatus, blockCSIDriverInstall)
}

func (c *VSphereController) driverAlreadyStarted(ctx context.Context) (bool, error) {
	csiDriver, err := c.csiDriverLister.Get(utils.VSphereDriverName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		reason := fmt.Errorf("vsphere driver sync failed, unable to verify CSIDriver status: %v", err)
		klog.Errorf("%s", reason.Error())
		return false, reason
	}
	annotations := csiDriver.GetAnnotations()
	if _, ok := annotations[utils.OpenshiftCSIDriverAnnotationKey]; ok {
		return true, nil
	}
	return false, nil
}

func (c *VSphereController) blockUpgradeOrDegradeCluster(
	ctx context.Context,
	result checks.ClusterCheckResult,
	infra *ocpv1.Infrastructure,
	status *operatorapi.OperatorStatus) (err error, blockInstall, blockUpgrade bool) {

	var clusterCondition string
	clusterStatus, result := checks.CheckClusterStatus(result, c.getCheckAPIDependency(infra))
	switch clusterStatus {
	case checks.ClusterCheckDegrade:
		clusterCondition = "degraded"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		updateError := c.updateConditions(ctx, c.name, result, status, operatorapi.ConditionFalse, true)
		return updateError, true, false
	case checks.ClusterCheckUpgradeStateUnknown:
		clusterCondition = "upgrade_unknown"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		updateError := c.updateConditions(ctx, c.name, result, status, operatorapi.ConditionUnknown, true)
		return updateError, true, false
	case checks.ClusterCheckBlockUpgrade:
		clusterCondition = "upgrade_blocked"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		updateError := c.updateConditions(ctx, c.name, result, status, operatorapi.ConditionFalse, false)
		return updateError, false, true
	case checks.ClusterCheckBlockUpgradeDriverInstall:
		clusterCondition = "install_blocked"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		clusterCondition = "upgrade_blocked"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		// Set Upgradeable: true with an extra message
		updateError := c.updateConditions(ctx, c.name, result, status, operatorapi.ConditionFalse, true)
		return updateError, true, true
	}
	return nil, false, false
}

func (c *VSphereController) runConditionalController(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(len(c.controllers))

	for i := range c.controllers {
		go func(index int) {
			cc := c.controllers[index]
			defer klog.Infof("%s controller terminated", cc.name)
			defer wg.Done()
			defer utilruntime.HandleCrash()
			// if conditionController is not running and there were no errors we should run
			// those controllers
			cc.controller.Run(ctx, 1)
		}(i)
	}
	wg.Wait()
}

func (c *VSphereController) runClusterCheck(ctx context.Context, infra *ocpv1.Infrastructure) (time.Duration, checks.ClusterCheckResult, bool) {
	checkerApiClient := c.getCheckAPIDependency(infra)

	checkOpts := checks.NewCheckArgs(c.vSphereConnections, checkerApiClient, c.featureGates)
	return c.vSphereChecker.Check(ctx, checkOpts)
}

func (c *VSphereController) getCheckAPIDependency(infra *ocpv1.Infrastructure) checks.KubeAPIInterface {
	checkerApiClient := &checks.KubeAPIInterfaceImpl{
		Infrastructure:         infra,
		CSINodeLister:          c.csiNodeLister,
		CSIDriverLister:        c.csiDriverLister,
		ClusterCSIDriverLister: c.clusterCSIDriverLister,
		NodeLister:             c.nodeLister,
	}
	return checkerApiClient
}

func (c *VSphereController) loginToVCenter(ctx context.Context, infra *ocpv1.Infrastructure) checks.ClusterCheckResult {
	failures, immediateError := c.createVCenterConnection(ctx, infra)
	if immediateError != nil {
		c.secondaryVCenterUnreachable = false
		c.secondaryVCenterMessage = ""
		return checks.MakeClusterDegradedError(checks.CheckStatusOpenshiftAPIError, immediateError)
	}

	day2Enabled := c.featureGates != nil && c.featureGates.Enabled(features.FeatureGateVSphereMultiVCenterDay2)
	if !day2Enabled {
		// Preserve original behavior exactly: any connection failure degrades/blocks, for every
		// vCenter alike. Phase 6 fault isolation (below) only applies when day2Enabled.
		for _, vConn := range c.vSphereConnections {
			if err := vConn.Connect(ctx); err != nil {
				return checks.ClusterCheckResult{
					CheckError:  err,
					Action:      checks.CheckActionBlockUpgradeOrDegrade,
					CheckStatus: checks.CheckStatusVSphereConnectionFailed,
					Reason:      fmt.Sprintf("Failed to connect to vSphere: %v", err),
				}
			}
		}
		return checks.MakeClusterCheckResultPass()
	}

	workspaceHost := c.resolveWorkspaceHost()
	singleVCenter := len(infra.Spec.PlatformSpec.VSphere.VCenters) <= 1

	var messages []string
	for _, failure := range failures {
		msg := fmt.Sprintf("vCenter %s is unreachable: %v", failure.host, failure.err)
		klog.Warningf("%s", msg)
		c.eventRecorder.Warningf(eventSecondaryVCenterUnreach, "%s", msg)
		messages = append(messages, msg)
	}

	var healthyConnections []*vclib.VSphereConnection
	var lastErr error
	for _, vConn := range c.vSphereConnections {
		err := vConn.Connect(ctx)
		if err != nil {
			critical := singleVCenter || (workspaceHost != "" && vConn.Hostname == workspaceHost)
			if critical {
				c.secondaryVCenterUnreachable = false
				c.secondaryVCenterMessage = ""
				return checks.ClusterCheckResult{
					CheckError:  err,
					Action:      checks.CheckActionBlockUpgradeOrDegrade,
					CheckStatus: checks.CheckStatusVSphereConnectionFailed,
					Reason:      fmt.Sprintf("Failed to connect to vSphere: %v", err),
				}
			}
			msg := fmt.Sprintf("vCenter %s is unreachable: %v", vConn.Hostname, err)
			klog.Warningf("Secondary %s", msg)
			c.eventRecorder.Warningf(eventSecondaryVCenterUnreach, "%s", msg)
			messages = append(messages, msg)
			lastErr = err
			continue
		}
		healthyConnections = append(healthyConnections, vConn)
	}

	totalConfigured := len(infra.Spec.PlatformSpec.VSphere.VCenters)
	if totalConfigured > 0 && len(healthyConnections) == 0 {
		// Every configured vCenter failed - either at credential lookup or Connect() - so there
		// is nothing to fall back on even though none of them individually matched the
		// workspace/single-vCenter "critical" check above.
		c.secondaryVCenterUnreachable = false
		c.secondaryVCenterMessage = ""
		err := lastErr
		if err == nil && len(failures) > 0 {
			err = failures[0].err
		}
		return checks.ClusterCheckResult{
			CheckError:  err,
			Action:      checks.CheckActionBlockUpgradeOrDegrade,
			CheckStatus: checks.CheckStatusVSphereConnectionFailed,
			Reason:      fmt.Sprintf("Failed to connect to vSphere: %v", err),
		}
	}

	c.vSphereConnections = healthyConnections
	c.secondaryVCenterUnreachable = len(messages) > 0
	c.secondaryVCenterMessage = strings.Join(messages, "; ")
	return checks.MakeClusterCheckResultPass()
}

func hasErrorConditions(opStats operatorapi.OperatorStatus) bool {
	conditions := opStats.Conditions
	hasDegradedOrBlockUpgradeConditions := false
	for _, condition := range conditions {
		if strings.HasSuffix(condition.Type, operatorapi.OperatorStatusTypeDegraded) {
			if condition.Status == operatorapi.ConditionTrue {
				hasDegradedOrBlockUpgradeConditions = true
			}
		}

		if strings.HasSuffix(condition.Type, operatorapi.OperatorStatusTypeUpgradeable) {
			if condition.Status == operatorapi.ConditionFalse {
				hasDegradedOrBlockUpgradeConditions = true
			}
		}

		if hasDegradedOrBlockUpgradeConditions {
			break
		}
	}
	return hasDegradedOrBlockUpgradeConditions
}

func (c *VSphereController) createVCenterConnection(ctx context.Context, infra *ocpv1.Infrastructure) ([]vCenterConnectFailure, error) {
	klog.V(3).Infof("Creating vSphere connection")
	day2Enabled := c.featureGates != nil && c.featureGates.Enabled(features.FeatureGateVSphereMultiVCenterDay2)
	workspaceHost := c.resolveWorkspaceHost()
	singleVCenter := len(infra.Spec.PlatformSpec.VSphere.VCenters) <= 1

	var failures []vCenterConnectFailure
	for _, vcenter := range infra.Spec.PlatformSpec.VSphere.VCenters {
		// Phase 6 fault isolation (a non-workspace vCenter's credential problem must not abort
		// connecting to the other, healthy vCenters) only applies when day2Enabled - otherwise
		// preserve the original "abort on first error" behavior exactly.
		critical := !day2Enabled || singleVCenter || (workspaceHost != "" && vcenter.Server == workspaceHost)

		secret, err := c.secretLister.Secrets(c.targetNamespace).Get(cloudCredSecretName)
		if err != nil {
			if critical {
				return failures, err
			}
			failures = append(failures, vCenterConnectFailure{host: vcenter.Server, err: err})
			continue
		}
		userKey := vcenter.Server + "." + "username"
		username, ok := secret.Data[userKey]
		if !ok {
			err := fmt.Errorf("error parsing secret %q: key %q not found", cloudCredSecretName, userKey)
			if critical {
				return failures, err
			}
			failures = append(failures, vCenterConnectFailure{host: vcenter.Server, err: err})
			continue
		}
		passwordKey := vcenter.Server + "." + "password"
		password, ok := secret.Data[passwordKey]
		if !ok {
			err := fmt.Errorf("error parsing secret %q: key %q not found", cloudCredSecretName, passwordKey)
			if critical {
				return failures, err
			}
			failures = append(failures, vCenterConnectFailure{host: vcenter.Server, err: err})
			continue
		}

		vs, err := vclib.NewVSphereConnection(string(username), string(password), vcenter.Server, c.cloudConfig)
		if err != nil {
			wrapped := fmt.Errorf("error creating new vsphere connection: %v", err)
			if critical {
				return failures, wrapped
			}
			failures = append(failures, vCenterConnectFailure{host: vcenter.Server, err: wrapped})
			continue
		}
		c.vSphereConnections = append(c.vSphereConnections, vs)
	}
	return failures, nil
}

func (c *VSphereController) loadCloudConfig(infra *ocpv1.Infrastructure) (*vclib.VSphereConfig, error) {
	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get cloud config: %v", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return nil, fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}

	config := vclib.VSphereConfig{}
	err = config.LoadConfig(cfgString)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// resolveWorkspaceHost returns the hostname of the primary/workspace vCenter, if determinable.
// Legacy ini configs carry it explicitly in Workspace.VCenterIP. YAML configs have no distinct
// "workspace" concept, but carry an equivalent notion of a primary vCenter in Global.VCenterIP.
// Returns "" if neither is available (e.g. cloudConfig not yet loaded).
func (c *VSphereController) resolveWorkspaceHost() string {
	if c.cloudConfig == nil {
		return ""
	}
	if c.cloudConfig.LegacyConfig != nil && c.cloudConfig.LegacyConfig.Workspace.VCenterIP != "" {
		return c.cloudConfig.LegacyConfig.Workspace.VCenterIP
	}
	if c.cloudConfig.Config != nil && c.cloudConfig.Config.Global.VCenterIP != "" {
		return c.cloudConfig.Config.Global.VCenterIP
	}
	return ""
}

// ensureRemovalMaps lazily initializes the removal-tracking maps. This keeps hand-built
// VSphereController literals (as used throughout the test suite) safe to use without every one
// of them needing to set up these maps explicitly.
func (c *VSphereController) ensureRemovalMaps() {
	if c.vCenterConfigSnapshots == nil {
		c.vCenterConfigSnapshots = make(map[string]vCenterConnSnapshot)
	}
	if c.previousVCenterHosts == nil {
		c.previousVCenterHosts = make(map[string]bool)
	}
	if c.pendingVCenterRemoval == nil {
		c.pendingVCenterRemoval = make(map[string]*vCenterRemovalState)
	}
}

// refreshVCenterConfigSnapshots caches connection settings for every vCenter that is currently
// active in c.cloudConfig. Must run every sync, while a vCenter is still active - it is the only
// way connectToVCenterHost can construct a connection to it after it has been removed from
// VCenters and therefore from cloudConfig.Config.VirtualCenter.
func (c *VSphereController) refreshVCenterConfigSnapshots() {
	c.ensureRemovalMaps()
	if c.cloudConfig == nil || c.cloudConfig.Config == nil {
		return
	}
	for host, vc := range c.cloudConfig.Config.VirtualCenter {
		c.vCenterConfigSnapshots[host] = vCenterConnSnapshot{Hostname: vc.VCenterIP, Insecure: vc.InsecureFlag}
	}
}

// classifyConnectError decides whether a reconnect failure against a removed vCenter looks
// permanent (bad/missing credentials) or transient (network/timeout/TLS/DNS - indistinguishable
// from a vCenter that is mid-maintenance). Defaults to transient: an unrecognized error is more
// likely a maintenance-time quirk than a reason to abandon cleanup, and maxTransientRetryWindow
// still bounds the cost either way.
func classifyConnectError(err error) connectFailureClass {
	if err == nil {
		return failureClassTransient
	}
	var netErr net.Error
	if stderrors.As(err, &netErr) {
		return failureClassTransient
	}
	if stderrors.Is(err, syscall.ECONNREFUSED) {
		return failureClassTransient
	}
	msg := strings.ToLower(err.Error())
	permanentSubstrings := []string{
		"incorrect user name or password",
		"login failure",
		"invalidlogin",
		"unauthorized",
		"authentication failed",
		"incorrect user name or password was specified",
	}
	for _, s := range permanentSubstrings {
		if strings.Contains(msg, s) {
			return failureClassPermanent
		}
	}
	return failureClassTransient
}

// connectToVCenterHost reconnects to a vCenter host using cached credentials/config settings,
// bypassing vclib.NewVSphereConnection's config-lookup helpers - those require a live
// cfg.Config.VirtualCenter[host] entry, which no longer exists once host has been removed from
// VCenters. connection settings come from the snapshot taken by refreshVCenterConfigSnapshots
// while host was still active.
func (c *VSphereController) connectToVCenterHost(ctx context.Context, host string) (*vclib.VSphereConnection, connectFailureClass, error) {
	snapshot, ok := c.vCenterConfigSnapshots[host]
	if !ok {
		return nil, failureClassPermanent, fmt.Errorf("no cached connection settings for removed vCenter %s", host)
	}
	secret, err := c.secretLister.Secrets(c.targetNamespace).Get(cloudCredSecretName)
	if err != nil {
		return nil, failureClassTransient, fmt.Errorf("reading cloud credentials: %w", err)
	}
	usernameKey := host + ".username"
	username, ok := secret.Data[usernameKey]
	if !ok {
		return nil, failureClassPermanent, fmt.Errorf("no cached credentials for removed vCenter %s", host)
	}
	passwordKey := host + ".password"
	password, ok := secret.Data[passwordKey]
	if !ok {
		return nil, failureClassPermanent, fmt.Errorf("no cached credentials for removed vCenter %s", host)
	}
	conn := &vclib.VSphereConnection{
		Username: string(username),
		Password: string(password),
		Hostname: snapshot.Hostname,
		Insecure: snapshot.Insecure,
		Config:   c.cloudConfig,
	}
	if err := conn.Connect(ctx); err != nil {
		return nil, classifyConnectError(err), err
	}
	return conn, failureClassTransient, nil
}

// reconcileRemovedVCenters detects vCenters removed from infra.Spec.PlatformSpec.VSphere.VCenters
// and drives best-effort reconnect/cleanup for them, bounded by maxTransientRetryWindow. Returns
// the set of successfully-reconnected cleanup-only connections for this sync; callers must log
// these out at the end of the same sync (a fresh connection is made every sync regardless).
//
// Retrying every host already pending is the primary, steady-state, run-every-sync path -
// detecting newly-removed hosts is the secondary trigger that seeds the map.
func (c *VSphereController) reconcileRemovedVCenters(ctx context.Context, infra *ocpv1.Infrastructure) []*vclib.VSphereConnection {
	c.ensureRemovalMaps()

	currentHosts := make(map[string]bool)
	if infra.Spec.PlatformSpec.VSphere != nil {
		for _, vc := range infra.Spec.PlatformSpec.VSphere.VCenters {
			currentHosts[vc.Server] = true
		}
	}

	// Re-add guard (C3): cancel pending removal for any host that reappeared in VCenters,
	// before anything below builds a cleanup connection for it this sync. Without this, a
	// re-added host would get both an active connection (from createVCenterConnection) and a
	// cleanup connection in the same Sync() call, and the cleanup connection's zero-FD branch
	// would delete the SPBM profile the active connection just verified/recreated.
	for host := range c.pendingVCenterRemoval {
		if currentHosts[host] {
			klog.V(2).Infof("vCenter %s reappeared in VCenters before cleanup finished; cancelling pending removal", host)
			delete(c.pendingVCenterRemoval, host)
			c.eventRecorder.Eventf(eventVCenterRemovalCancelled, "vCenter %s reappeared in spec before cleanup finished", host)
		}
	}

	workspaceHost := c.resolveWorkspaceHost()

	var cleanupConnections []*vclib.VSphereConnection

	attemptHost := func(host string) {
		state, ok := c.pendingVCenterRemoval[host]
		if !ok {
			state = &vCenterRemovalState{firstDetected: time.Now()}
			c.pendingVCenterRemoval[host] = state
			c.eventRecorder.Eventf(eventVCenterCleanupStarted, "vCenter %s was removed; starting best-effort cleanup", host)
		}
		conn, class, err := c.connectToVCenterHost(ctx, host)
		state.attempts++
		state.lastClass = class
		state.lastError = err
		if err != nil {
			klog.V(2).Infof("Reconnect attempt %d for removed vCenter %s failed (%s): %v", state.attempts, host, class, err)
			c.evaluateGiveUp(host, state)
			return
		}
		cleanupConnections = append(cleanupConnections, conn)
	}

	// Retry every host already pending from a previous sync (steady state). Snapshot the key
	// set first: attemptHost/evaluateGiveUp may delete from pendingVCenterRemoval as they run,
	// and newly-seeded hosts (below) must not be double-attempted in the same sync.
	pendingHosts := make([]string, 0, len(c.pendingVCenterRemoval))
	for host := range c.pendingVCenterRemoval {
		pendingHosts = append(pendingHosts, host)
	}
	for _, host := range pendingHosts {
		attemptHost(host)
	}

	// Detect newly-removed hosts and seed + attempt them within this same sync.
	for host := range c.previousVCenterHosts {
		if currentHosts[host] {
			continue
		}
		if _, alreadyHandled := c.pendingVCenterRemoval[host]; alreadyHandled {
			continue
		}
		if workspaceHost != "" && host == workspaceHost {
			klog.Warningf("Primary/workspace vCenter %s was removed from VCenters; skipping automatic reconnect/cleanup (handled by the connection-failure degraded path instead)", host)
			continue
		}
		attemptHost(host)
	}

	c.previousVCenterHosts = currentHosts
	return cleanupConnections
}

// evaluateGiveUp applies the maintenance-aware give-up policy: permanent failures (bad/missing
// credentials) abandon immediately, transient failures (network/timeout - indistinguishable from
// a maintenance window) are retried until maxTransientRetryWindow elapses.
func (c *VSphereController) evaluateGiveUp(host string, state *vCenterRemovalState) {
	giveUp := false
	switch state.lastClass {
	case failureClassPermanent:
		giveUp = true
	case failureClassTransient:
		if time.Since(state.firstDetected) > maxTransientRetryWindow {
			giveUp = true
		}
	}
	if !giveUp {
		return
	}
	klog.Warningf("Giving up on cleanup for removed vCenter %s after %d attempt(s): %v", host, state.attempts, state.lastError)
	c.eventRecorder.Warningf(eventVCenterCleanupAbandoned, "Giving up on cleanup for removed vCenter %s after %d attempt(s): %v", host, state.attempts, state.lastError)
	utils.VCenterRemovalCleanupTotal.WithLabelValues(metricResultAbandoned).Inc()
	delete(c.pendingVCenterRemoval, host)
	delete(c.vCenterConfigSnapshots, host)
	if c.storageClassController != nil {
		c.storageClassController.PurgeVCenterState(host)
	}
}

// finalizeCleanupState checks, for every host we attempted cleanup on this sync, whether
// StorageClassController now considers it fully clean; if so the removal is done and the
// bookkeeping for it is dropped. Otherwise it stays in pendingVCenterRemoval and is retried next
// sync (bounded by evaluateGiveUp).
func (c *VSphereController) finalizeCleanupState(cleanupConnections []*vclib.VSphereConnection) {
	for _, conn := range cleanupConnections {
		if conn == nil {
			continue
		}
		if !c.storageClassController.IsHostFullyClean(conn.Hostname) {
			continue
		}
		if _, wasPending := c.pendingVCenterRemoval[conn.Hostname]; !wasPending {
			// Already finalized (e.g. on a previous sync) - avoid firing a duplicate
			// success event/metric for a host that isn't actually pending anymore.
			continue
		}
		klog.V(2).Infof("Cleanup confirmed complete for removed vCenter %s", conn.Hostname)
		delete(c.pendingVCenterRemoval, conn.Hostname)
		delete(c.vCenterConfigSnapshots, conn.Hostname)
		c.eventRecorder.Eventf(eventVCenterCleanupSucceeded, "Completed cleanup for removed vCenter %s", conn.Hostname)
		utils.VCenterRemovalCleanupTotal.WithLabelValues(metricResultSuccess).Inc()
		c.storageClassController.PurgeVCenterState(conn.Hostname)
	}
}

// updateVCenterRemovalPendingCondition mirrors OrphanCleanupPending: True while any vCenter is
// known-removed-but-not-yet-cleaned-up, so admins can see pending Day-2 work via `oc get
// clusteroperator` without needing to inspect operator logs.
func (c *VSphereController) updateVCenterRemovalPendingCondition(ctx context.Context) error {
	cond := operatorapi.OperatorCondition{
		Type:   c.name + conditionVCenterRemovalPending,
		Status: operatorapi.ConditionFalse,
		Reason: "NoPendingRemovals",
	}
	for host, state := range c.pendingVCenterRemoval {
		if state.attempts == 0 {
			continue
		}
		cond.Status = operatorapi.ConditionTrue
		cond.Reason = "CleanupInProgress"
		var lastErrMsg string
		if state.lastError != nil {
			lastErrMsg = state.lastError.Error()
		}
		cond.Message = fmt.Sprintf("vCenter %s was removed and is pending best-effort cleanup (attempts=%d, lastError=%q)", host, state.attempts, lastErrMsg)
		break
	}
	_, _, err := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(cond))
	return err
}

// updateSecondaryVCenterCondition surfaces Phase 6 fault isolation: a non-workspace vCenter
// being unreachable is visible via this condition, distinct from the main Degraded condition, so
// a maintenance window doesn't page anyone or block upgrades.
func (c *VSphereController) updateSecondaryVCenterCondition(ctx context.Context) error {
	cond := operatorapi.OperatorCondition{
		Type:   c.name + conditionSecondaryVCenterUnrch,
		Status: operatorapi.ConditionFalse,
		Reason: "AllVCentersReachable",
	}
	if c.secondaryVCenterUnreachable {
		cond.Status = operatorapi.ConditionTrue
		cond.Reason = "VCenterUnreachable"
		cond.Message = c.secondaryVCenterMessage
	}
	_, _, err := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(cond))
	return err
}

func (c *VSphereController) updateConditions(
	ctx context.Context,
	name string,
	lastCheckResult checks.ClusterCheckResult,
	status *operatorapi.OperatorStatus,
	upgradeStatus operatorapi.ConditionStatus,
	blockCSIDriverInstall bool) error {

	updateFuncs := []v1helpers.UpdateStatusFunc{}

	// we are degrading using a custom name here because, if we use name + Degraded
	// library-go will override the condition and mark cluster un-degraded.
	// Degrading here with custom name here ensures that - our degrade condition is sticky
	// and only this operator can remove the degraded condition.
	degradeCond := operatorapi.OperatorCondition{
		Type:   "VMwareVSphereOperatorCheck" + operatorapi.OperatorStatusTypeDegraded,
		Status: operatorapi.ConditionFalse,
	}

	if lastCheckResult.Action == checks.CheckActionDegrade {
		klog.Warningf("Marking cluster as degraded: %s %s", lastCheckResult.CheckStatus, lastCheckResult.Reason)
		degradeCond.Status = operatorapi.ConditionTrue
		degradeCond.Reason = string(lastCheckResult.CheckStatus)
		degradeCond.Message = lastCheckResult.Reason
	}

	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(degradeCond))

	allowUpgradeCond := operatorapi.OperatorCondition{
		Type:   name + operatorapi.OperatorStatusTypeUpgradeable,
		Status: operatorapi.ConditionTrue,
	}

	conditionChanged := false
	var blockUpgradeMessage string

	switch upgradeStatus {
	case operatorapi.ConditionFalse:
		blockUpgradeMessage = fmt.Sprintf("Marking cluster un-upgradeable because %s", lastCheckResult.Reason)
		allowUpgradeCond, conditionChanged = c.addUpgradeableBlockCondition(lastCheckResult, name, status, operatorapi.ConditionFalse)
	case operatorapi.ConditionUnknown:
		blockUpgradeMessage = fmt.Sprintf("Marking cluster upgrade status unknown because %s", lastCheckResult.Reason)
		allowUpgradeCond, conditionChanged = c.addUpgradeableBlockCondition(lastCheckResult, name, status, operatorapi.ConditionUnknown)
	default:
		blockUpgradeMessage = lastCheckResult.Reason
		allowUpgradeCond, conditionChanged = c.addUpgradeableBlockCondition(lastCheckResult, name, status, operatorapi.ConditionTrue)
	}

	// Mark operator as disabled if the CSI driver is not running. CSO will then set Progressing=False and Available=True,
	// with proper messages.
	if !c.operandControllerStarted && blockCSIDriverInstall {
		klog.V(4).Infof("Adding %s: True", c.getDisabledConditionName())
		disabledCond := operatorapi.OperatorCondition{
			Type:    c.getDisabledConditionName(),
			Status:  operatorapi.ConditionTrue,
			Message: lastCheckResult.Reason,
		}
		updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(disabledCond))
	} else {
		// Remove the disabled condition
		klog.V(4).Infof("Removing %s", c.getDisabledConditionName())
		updateFuncs = append(updateFuncs, func(status *operatorapi.OperatorStatus) error {
			v1helpers.RemoveOperatorCondition(&status.Conditions, c.getDisabledConditionName())
			return nil
		})
	}

	// VMwareVSphereControllerAvailable handling was removed in 4.16. Remove the stale condition, if it exists.
	// TODO: remove in 4.17
	obsoleteConditionName := c.name + operatorapi.OperatorStatusTypeAvailable
	updateFuncs = append(updateFuncs, func(status *operatorapi.OperatorStatus) error {
		v1helpers.RemoveOperatorCondition(&status.Conditions, obsoleteConditionName)
		return nil
	})

	if len(blockUpgradeMessage) > 0 {
		klog.Warningf("%s", blockUpgradeMessage)
	}

	if conditionChanged && upgradeStatus != operatorapi.ConditionTrue {
		c.eventRecorder.Warningf(string(lastCheckResult.CheckStatus), blockUpgradeMessage)
	}

	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(allowUpgradeCond))
	if _, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, updateFuncs...); updateErr != nil {
		return updateErr
	}

	return nil
}

func (c *VSphereController) addUpgradeableBlockCondition(
	lastCheckResult checks.ClusterCheckResult,
	name string,
	status *operatorapi.OperatorStatus,
	upgradeStatus operatorapi.ConditionStatus) (operatorapi.OperatorCondition, bool) {
	conditionType := name + operatorapi.OperatorStatusTypeUpgradeable

	blockUpgradeCondition := operatorapi.OperatorCondition{
		Type:    conditionType,
		Status:  upgradeStatus,
		Message: lastCheckResult.Reason,
		Reason:  string(lastCheckResult.CheckStatus),
	}

	oldConditions := status.Conditions
	for _, condition := range oldConditions {
		if condition.Type == conditionType {
			if condition.Status != blockUpgradeCondition.Status ||
				condition.Message != blockUpgradeCondition.Message ||
				condition.Reason != blockUpgradeCondition.Reason {
				return blockUpgradeCondition, true
			} else {
				return blockUpgradeCondition, false
			}
		}
	}
	return blockUpgradeCondition, true
}

func (c *VSphereController) createCSISecret(
	ctx context.Context,
	syncContext factory.SyncContext,
	infra *ocpv1.Infrastructure,
	clusterCSIDriver *operatorapi.ClusterCSIDriver) error {

	// TODO: none of our CSI operators check whether they are running in the correct cloud. Is
	// this something we want to change? These operators are supposed to be deployed by CSO, which
	// already does this checking for us.

	// Pass in first vcenter for now.  I think this logic is no longer valid, but need to confirm if we are wanting
	// multi vcenter to support storage migration.
	datastoreURL := ""

	if len(infra.Spec.PlatformSpec.VSphere.VCenters) == 1 {
		datastoreURLs := make(map[string]string)
		for _, connection := range c.vSphereConnections {
			storageApiClient := storageclasscontroller.NewStoragePolicyAPI(ctx, connection, infra, false, false, nil)

			defaultDatastore, err := storageApiClient.GetDefaultDatastore(ctx, infra)

			if err != nil {
				return fmt.Errorf("unable to fetch default datastore url: %v", err)
			}

			datastoreURL = defaultDatastore.Summary.Url
			datastoreURLs[connection.Hostname] = datastoreURL
		}
	}

	requiredSecret, err := c.applyClusterCSIDriverChange(infra, c.cloudConfig, clusterCSIDriver, datastoreURL)
	if err != nil {
		return err
	}

	// TODO: check if configMap has been deployed and set appropriate conditions
	_, _, err = resourceapply.ApplySecret(ctx, c.kubeClient.CoreV1(), syncContext.Recorder(), requiredSecret)
	if err != nil {
		return fmt.Errorf("error applying vsphere csi driver config: %v", err)
	}

	return nil
}

func (c *VSphereController) applyClusterCSIDriverChange(
	infra *ocpv1.Infrastructure,
	sourceCFG *vclib.VSphereConfig,
	clusterCSIDriver *operatorapi.ClusterCSIDriver,
	datastoreURL string) (*corev1.Secret, error) {

	csiConfigString := string(c.csiConfigManifest)

	csiVCenterConfigBytes, err := assets.ReadFile("csi_cloud_config_vcenters.ini")

	if err != nil {
		return nil, err
	}

	// Generate cluster id and append all vcenters.  Also need to inject user/pass for vcenters since driver does
	// not support loading from secret.  It expects user/pass either in the ini file or as an ENV variable.  ENV
	// variable was used in older, single vcenter way where passed into container from operator.
	config := sourceCFG.Config
	var vcenters string

	// Sort keys alphabetically to guarantee order does not change in output config
	var vCenterKeys []string
	for key := range config.VirtualCenter {
		vCenterKeys = append(vCenterKeys, key)
	}
	sort.Strings(vCenterKeys)

	for _, vcenterKey := range vCenterKeys {
		vcenterStr := string(csiVCenterConfigBytes)
		vcenter := config.VirtualCenter[vcenterKey]

		user, password, err := getUserAndPassword(vcenter.VCenterIP, c.apiClients.SecretInformer)
		if err != nil {
			return nil, fmt.Errorf("error getting user and password: %v", err)
		}

		escapedUser, escapedPassword := escapeUserAndPassword(user, password)

		for pattern, value := range map[string]string{
			"${VCENTER}":     vcenter.VCenterIP,
			"${DATACENTERS}": vcenter.Datacenters,
			"${PASSWORD}":    escapedPassword,
			"${USER}":        escapedUser,
		} {
			vcenterStr = strings.ReplaceAll(vcenterStr, pattern, value)
		}
		if len(vCenterKeys) < 2 {
			vcenterStr = fmt.Sprintf("%v\nmigration-datastore-url = \"%v\"", vcenterStr, datastoreURL)
		}
		vcenters = vcenters + "\n" + vcenterStr
	}

	for pattern, value := range map[string]string{
		"${CLUSTER_ID}": infra.Status.InfrastructureName,
		"${VCENTERS}":   vcenters,
	} {
		csiConfigString = strings.ReplaceAll(csiConfigString, pattern, value)
	}

	topologyCategories := utils.GetTopologyCategories(clusterCSIDriver, infra)
	if len(topologyCategories) > 0 {
		topologyCategoryString := strings.Join(topologyCategories, ",")
		csiConfigString = fmt.Sprintf("%v\n[Labels]\ntopology-categories = \"%v\"", csiConfigString, topologyCategoryString)
	}

	snapshotOptions := utils.GetSnapshotOptions(clusterCSIDriver)
	if len(snapshotOptions) > 0 {
		csiConfigString = fmt.Sprintf("%v\n[Snapshot]", csiConfigString)
		for _, opt := range snapshotOptions {
			csiConfigString = fmt.Sprintf("%v\n%v = %v", csiConfigString, opt.Key, opt.Value)
		}
	}
	csiConfigString = fmt.Sprintf("%v\n", csiConfigString)

	requiredSecret := resourceread.ReadSecretV1OrDie(c.secretManifest)
	requiredSecret.Data["cloud.conf"] = []byte(csiConfigString)
	return requiredSecret, nil
}

func (c *VSphereController) createStorageClassController() storageclasscontroller.StorageClassSyncInterface {
	scBytes, err := assets.ReadFile("storageclass.yaml")
	if err != nil {
		panic("unable to read storageclass file")
	}
	storageClassController := storageclasscontroller.NewStorageClassController(
		storageClassControllerName,
		defaultNamespace,
		scBytes,
		c.apiClients.KubeClient,
		c.apiClients.OperatorClient,
		c.scLister,
		c.apiClients.ClusterCSIDriverInformer,
		c.eventRecorder,
		c.featureGates,
	)
	return storageClassController
}

func getUserAndPassword(vcenter string, secretInformer corev1informers.SecretInformer) (string, string, error) {
	secret, err := secretInformer.Lister().Secrets(defaultNamespace).Get(cloudCredSecretName)
	if err != nil {
		return "", "", err
	}

	// CCO generates a secret that contains dynamic keys, for example:
	// oc get secret/vmware-vsphere-cloud-credentials -o json | jq .data
	// {
	//   "vcenter.xyz.vmwarevmc.com.password": "***",
	//   "vcenter.xyz.vmwarevmc.com.username": "***"
	// }
	// So we need to figure those keys out
	var usernameKey, passwordKey string

	usernameKey = vcenter + ".username"
	passwordKey = vcenter + ".password"

	if usernameKey == "" || passwordKey == "" {
		return "", "", fmt.Errorf("could not find vSphere credentials in secret %s/%s", secret.Namespace, secret.Name)
	}

	// Get username and pass from secret created by CCO
	username := string(secret.Data[usernameKey])
	password := string(secret.Data[passwordKey])
	return username, password, nil
}

// The CSI driver expects a password with any quotation marks and backslashes escaped.
// xref: https://github.com/kubernetes-sigs/vsphere-csi-driver/issues/121
// The username in the format "domainName\userName" must be converted to "domainName\\userName"
// xref: https://docs.vmware.com/en/VMware-vSphere-Container-Storage-Plug-in/3.0/vmware-vsphere-csp-getting-started/GUID-BFF39F1D-F70A-4360-ABC9-85BDAFBE8864.html
func escapeUserAndPassword(username, password string) (string, string) {
	escapedUserName := escapeBackslashInUsername(username)
	escapedPassword := escapeQuotesAndBackslashes(password)
	return escapedUserName, escapedPassword
}

// escapeQuotesAndBackslashes escapes double quotes and backslashes in the input string.
func escapeQuotesAndBackslashes(input string) string {
	return reEscape.ReplaceAllString(input, `\$0`)
}

// escapeBackslashInUsername escapes single backslash in the input string like "domainName\userName"
func escapeBackslashInUsername(input string) string {
	regex := `^[a-zA-Z0-9.-]+\\[a-zA-Z0-9._-]+$`
	if match, _ := regexp.MatchString(regex, input); match {
		return escapeQuotesAndBackslashes(input)
	}
	return input
}

func getvCenterName(infra *ocpv1.Infrastructure, configmapLister corelister.ConfigMapLister) (string, error) {
	// This change can only be used in >=4.13 versions of OCP
	vSphereInfraConfig := infra.Spec.PlatformSpec.VSphere
	if vSphereInfraConfig != nil && len(vSphereInfraConfig.VCenters) > 0 {
		return vSphereInfraConfig.VCenters[0].Server, nil
	}

	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := configmapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get cloud config: %v", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return "", fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}

	// Load combo config.
	cfg := vclib.VSphereConfig{}
	err = cfg.LoadConfig(cfgString)
	if err != nil {
		fmt.Println("Returning error")
		return "", err
	}

	// Due to how upstream config handles merging ini and yaml logic, we will check to see if the legacy ini cloud provider
	// is in use first.  This way we can fall back to our old logic of just returning workspace logic.
	if cfg.LegacyConfig != nil {
		fmt.Printf("Returning legacy: %v\n", cfg.LegacyConfig.Workspace.VCenterIP)
		return cfg.LegacyConfig.Workspace.VCenterIP, nil
	}

	// If YAML style config, but cluster has not configured FailureDomains, let's see if we can get first vCenter and
	// return the hostname.  This should not happen, but just in case, we'll get the keys and just return one for now.
	if len(cfg.Config.VirtualCenter) > 0 {
		for k := range cfg.Config.VirtualCenter {
			// just going to return on first key.
			fmt.Printf("Returning a vcenter from map %v\n", cfg.Config.VirtualCenter[k].VCenterIP)
			return cfg.Config.VirtualCenter[k].VCenterIP, nil
		}
	}

	// All hope is lost.  Return an error.
	return "", fmt.Errorf("unable to determine vCenter from config %s/%s", cloudConfigNamespace, cloudConfig.Name)
}

// Ensure the ConfigMap is deleted as it is no longer in use
func (c *VSphereController) deleteConfigMapIfExists(ctx context.Context, name, namespace string) {
	configMap, err := c.configMapLister.ConfigMaps(c.targetNamespace).Get(cloudCredSecretName)
	switch {
	case errors.IsNotFound(err):
		// ConfigMap doesn't exist, no deletion necessary
		return
	case err != nil:
		klog.Errorf("Failed to get ConfigMap %s/%s for deletion: %v", namespace, cloudCredSecretName, err)
		return
	case configMap != nil:
		err := c.kubeClient.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil {
			klog.Errorf("Failed to delete ConfigMap %s/%s: %v", namespace, name, err)
		} else {
			klog.Infof("Successfully deleted ConfigMap %s/%s", namespace, name)
		}
	}
}

func ConvertToPlatformSpec(config *vclib.VSphereConfig, infra *ocpv1.Infrastructure) {
	if infra.Spec.PlatformSpec.VSphere == nil {
		// Add an empty vSphere spec to infra created in OCP 4.10 and earlier.
		// It gets populated from the `config` object below.
		infra.Spec.PlatformSpec.VSphere = &ocpv1.VSpherePlatformSpec{}
	}
	vSphereSpec := infra.Spec.PlatformSpec.VSphere

	if config != nil {
		//   We only need to do this for legacy ini configs.  Yaml configs we expect all to be configured correctly.
		if config.LegacyConfig != nil {
			if len(vSphereSpec.VCenters) != 0 {
				// we need to check if we really need to add to VCenters and FailureDomains.
				configuredVCenters := vCentersToMap(vSphereSpec.VCenters)

				// vcenter is missing from the map, add it...
				for _, vCenter := range config.Config.VirtualCenter {
					if _, ok := configuredVCenters[vCenter.VCenterIP]; !ok {
						klog.Warningf("vCenter %v is missing from vCenter map", vCenter.VCenterIP)
						addVCenter(config, vSphereSpec, vCenter.VCenterIP)
					}
				}

				// If platform spec defined vCenters, but no failure domains, this seems like invalid config.  We can
				// attempt to add failure domain as a failsafe, but only if legacy ini config was used.
				if len(vSphereSpec.FailureDomains) == 0 {
					addFailureDomainsToPlatformSpec(config, vSphereSpec, config.LegacyConfig.Workspace.VCenterIP)
				}
			} else {
				// If we are here, infrastructure resource hasn't been updated for any vCenter.  For multi vcenter support,
				// being here is not supported.  For 1 vCenter we will allow which should be from a very old cluster being
				// upgraded but never having infrastructure config updated to latest standards.
				if len(config.Config.VirtualCenter) == 1 {
					convertIntreeToPlatformSpec(config, vSphereSpec)
				} else {
					klog.Error("infrastructure has not been configured correctly to support multiple vCenters.")
				}
			}
		}
	}
}

func convertIntreeToPlatformSpec(config *vclib.VSphereConfig, platformSpec *ocpv1.VSpherePlatformSpec) {
	// All this logic should only happen if using legacy cloud provider config and admin has not set up failure domain
	legacyCfg := config.LegacyConfig
	if ccmVcenter, ok := legacyCfg.VirtualCenter[legacyCfg.Workspace.VCenterIP]; ok {
		datacenters := strings.Split(ccmVcenter.Datacenters, ",")

		platformSpec.VCenters = append(platformSpec.VCenters, ocpv1.VSpherePlatformVCenterSpec{
			Server:      legacyCfg.Workspace.VCenterIP,
			Datacenters: datacenters,
		})
		addFailureDomainsToPlatformSpec(config, platformSpec, legacyCfg.Workspace.VCenterIP)
	}
}

func addVCenter(config *vclib.VSphereConfig, platformSpec *ocpv1.VSpherePlatformSpec, vCenterName string) {
	// This logic happens if using legacy or new yaml cloud provider config and admin has not set up failure domain
	//legacyCfg := config.LegacyConfig
	if ccmVcenter, ok := config.Config.VirtualCenter[vCenterName]; ok {
		datacenters := strings.Split(ccmVcenter.Datacenters, ",")

		platformSpec.VCenters = append(platformSpec.VCenters, ocpv1.VSpherePlatformVCenterSpec{
			Server:      ccmVcenter.VCenterIP,
			Datacenters: datacenters,
		})
	}
}

func addFailureDomainsToPlatformSpec(config *vclib.VSphereConfig, platformSpec *ocpv1.VSpherePlatformSpec, vcenter string) {
	legacyCfg := config.LegacyConfig
	platformSpec.FailureDomains = append(platformSpec.FailureDomains, ocpv1.VSpherePlatformFailureDomainSpec{
		Name:   "",
		Region: "",
		Zone:   "",
		Server: vcenter,
		Topology: ocpv1.VSpherePlatformTopology{
			Datacenter:   legacyCfg.Workspace.Datacenter,
			Folder:       legacyCfg.Workspace.Folder,
			ResourcePool: legacyCfg.Workspace.ResourcePoolPath,
			Datastore:    legacyCfg.Workspace.DefaultDatastore,
		},
	})
}

func vCentersToMap(vcenters []ocpv1.VSpherePlatformVCenterSpec) map[string]ocpv1.VSpherePlatformVCenterSpec {
	vcenterMap := make(map[string]ocpv1.VSpherePlatformVCenterSpec, len(vcenters))
	for _, v := range vcenters {
		vcenterMap[v.Server] = v
	}
	return vcenterMap
}
