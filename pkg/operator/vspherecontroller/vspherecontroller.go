package vspherecontroller

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
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
}

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

	// Load config when it has changed or if first time syncing.  For now, we do every time, but in future maybe limit
	// this to only when changed so that we can reduce logging messages.
	c.cloudConfig, err = c.loadCloudConfig(infra)
	if err != nil {
		return err
	}

	// Update infra so we have failure domains in the case of an older cluster with out-dated infra definition.
	/*if len(infra.Spec.PlatformSpec.VSphere.VCenters) == 0 && c.cloudConfig.LegacyConfig != nil {
		klog.V(2).Infof("converting older in-tree config to platform spec")
		convertIntreeToPlatformSpec(c.cloudConfig, infra.Spec.PlatformSpec.VSphere)
	}*/
	// The following logic is borrowed from VPD.  We should make util project contain this so its shared and kept in sync
	ConvertToPlatformSpec(c.cloudConfig, infra)

	// We no longer use the ConfigMap to store the vSphere config, so make sure to delete it
	c.deleteConfigMapIfExists(ctx, legacyConfigMapName, c.targetNamespace)

	if c.vsphereConnectionFunc != nil {
		c.vSphereConnections, connectionResult, logout = c.vsphereConnectionFunc()
	} else {
		connectionResult = c.loginToVCenter(ctx, infra)
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
		c.vSphereConnections = nil
	}()

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
		err = c.storageClassController.Sync(ctx, c.vSphereConnections, storageClassAPIDeps)
		// storageclass sync will only return error if somehow updating conditions fails, in which case
		// we can return error here and degrade the cluster
		if err != nil {
			return err
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
		klog.Errorf(reason.Error())
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
		Infrastructure:  infra,
		CSINodeLister:   c.csiNodeLister,
		CSIDriverLister: c.csiDriverLister,
		NodeLister:      c.nodeLister,
	}
	return checkerApiClient
}

func (c *VSphereController) loginToVCenter(ctx context.Context, infra *ocpv1.Infrastructure) checks.ClusterCheckResult {
	immediateError := c.createVCenterConnection(ctx, infra)
	if immediateError != nil {
		return checks.MakeClusterDegradedError(checks.CheckStatusOpenshiftAPIError, immediateError)
	}

	for _, vConn := range c.vSphereConnections {
		err := vConn.Connect(ctx)
		if err != nil {
			result := checks.ClusterCheckResult{
				CheckError:  err,
				Action:      checks.CheckActionBlockUpgradeOrDegrade,
				CheckStatus: checks.CheckStatusVSphereConnectionFailed,
				Reason:      fmt.Sprintf("Failed to connect to vSphere: %v", err),
			}
			return result
		}
	}
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

func (c *VSphereController) createVCenterConnection(ctx context.Context, infra *ocpv1.Infrastructure) error {
	klog.V(3).Infof("Creating vSphere connection")
	for _, vcenter := range infra.Spec.PlatformSpec.VSphere.VCenters {
		secret, err := c.secretLister.Secrets(c.targetNamespace).Get(cloudCredSecretName)
		if err != nil {
			return err
		}
		userKey := vcenter.Server + "." + "username"
		username, ok := secret.Data[userKey]
		if !ok {
			return fmt.Errorf("error parsing secret %q: key %q not found", cloudCredSecretName, userKey)
		}
		passwordKey := vcenter.Server + "." + "password"
		password, ok := secret.Data[passwordKey]
		if !ok {
			return fmt.Errorf("error parsing secret %q: key %q not found", cloudCredSecretName, passwordKey)
		}

		vs := vclib.NewVSphereConnection(string(username), string(password), vcenter.Server, c.cloudConfig)
		c.vSphereConnections = append(c.vSphereConnections, vs)
	}
	return nil
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
		klog.Warningf(blockUpgradeMessage)
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
			storageApiClient := storageclasscontroller.NewStoragePolicyAPI(ctx, connection, infra)

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

	// Validate config.  We used to fail when calling utils.GetDataCenters, but now we will let config validate itself.
	err := sourceCFG.ValidateConfig(c.featureGates)
	if err != nil {
		return nil, err
	}

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

		user, password, err := getUserAndPassword(defaultNamespace, cloudCredSecretName, vcenter.VCenterIP, infra, c.configMapLister, c.apiClients.SecretInformer, c.featureGates)
		if err != nil {
			return nil, err
		}

		for pattern, value := range map[string]string{
			"${VCENTER}":     vcenter.VCenterIP,
			"${DATACENTERS}": vcenter.Datacenters,
			"${PASSWORD}":    password,
			"${USER}":        user,
		} {
			vcenterStr = strings.ReplaceAll(vcenterStr, pattern, value)
		}
		if len(vCenterKeys) < 2 {
			vcenterStr = fmt.Sprintf("%v\nmigration-datastore-url = %v", vcenterStr, datastoreURL)
		}
		vcenters = vcenters + "\n" + vcenterStr
	}

	for pattern, value := range map[string]string{
		"${CLUSTER_ID}": infra.Status.InfrastructureName,
		"${VCENTERS}":   vcenters,
	} {
		csiConfigString = strings.ReplaceAll(csiConfigString, pattern, value)
	}

	csiConfig, err := newINIConfig(csiConfigString)
	if err != nil {
		return nil, err
	}

	topologyCategories := utils.GetTopologyCategories(clusterCSIDriver, infra)
	if len(topologyCategories) > 0 {
		topologyCategoryString := strings.Join(topologyCategories, ",")
		csiConfig.Set("Labels", "topology-categories", topologyCategoryString)
	}

	snapshotOptions := utils.GetSnapshotOptions(clusterCSIDriver)
	if len(snapshotOptions) > 0 {
		for k, v := range snapshotOptions {
			csiConfig.Set("Snapshot", k, v)
		}
	}

	requiredSecret := resourceread.ReadSecretV1OrDie(c.secretManifest)
	requiredSecret.Data["cloud.conf"] = []byte(csiConfig.String())
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

func getUserAndPassword(namespace string, secretName string, vcenter string, infra *ocpv1.Infrastructure, configMapLister corelister.ConfigMapLister, secretInformer corev1informers.SecretInformer, featureGates featuregates.FeatureGate,
) (string, string, error) {
	secret, err := secretInformer.Lister().Secrets(namespace).Get(secretName)
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

	if len(secret.Data) > 2 && !featureGates.Enabled(features.FeatureGateVSphereMultiVCenters) {
		klog.Warningf("CSI driver can only connect to one vcenter, more than 1 set of credentials found for CSI driver")
	}

	usernameKey = vcenter + ".username"
	passwordKey = vcenter + ".password"

	if usernameKey == "" || passwordKey == "" {
		return "", "", fmt.Errorf("could not find vSphere credentials in secret %s/%s", secret.Namespace, secret.Name)
	}

	// Get username and pass from secret created by CCO
	username := string(secret.Data[usernameKey])
	password := string(secret.Data[passwordKey])

	// The CSI driver expects a password with any quotation marks and backslashes escaped.
	// xref: https://github.com/kubernetes-sigs/vsphere-csi-driver/issues/121
	return username, escapeQuotesAndBackslashes(password), nil
}

// escapeQuotesAndBackslashes escapes double quotes and backslashes in the input string.
func escapeQuotesAndBackslashes(input string) string {
	return reEscape.ReplaceAllString(input, `\$0`)
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
	vSphereSpec := infra.Spec.PlatformSpec.VSphere

	if config != nil {
		//   We only need to do this for legacy ini configs.  Yaml configs we expect all to be configured correctly.
		if vSphereSpec != nil && config.LegacyConfig != nil {
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
