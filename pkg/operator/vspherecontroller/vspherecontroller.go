package vspherecontroller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"gopkg.in/gcfg.v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/legacy-cloud-providers/vsphere"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/storageclasscontroller"

	ocpv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	storagelister "k8s.io/client-go/listers/storage/v1"
	"k8s.io/klog/v2"
)

type VSphereController struct {
	name                     string
	targetNamespace          string
	manifest                 []byte
	eventRecorder            events.Recorder
	kubeClient               kubernetes.Interface
	operatorClient           v1helpers.OperatorClientWithFinalizers
	configMapLister          corelister.ConfigMapLister
	secretLister             corelister.SecretLister
	scLister                 storagelister.StorageClassLister
	infraLister              infralister.InfrastructureLister
	nodeLister               corelister.NodeLister
	csiDriverLister          storagelister.CSIDriverLister
	csiNodeLister            storagelister.CSINodeLister
	apiClients               utils.APIClient
	controllers              []conditionalController
	storageClassController   *storageclasscontroller.StorageClassController
	operandControllerStarted bool
	vSphereConnection        *vclib.VSphereConnection
	vSphereChecker           vSphereEnvironmentCheckInterface
	// creates a new vSphereConnection - mainly used for testing
	vsphereConnectionFunc func() (*vclib.VSphereConnection, checks.ClusterCheckResult)
}

const (
	cloudConfigNamespace              = "openshift-config"
	infraGlobalName                   = "cluster"
	secretName                        = "vmware-vsphere-cloud-credentials"
	trustedCAConfigMap                = "vmware-vsphere-csi-driver-trusted-ca-bundle"
	defaultNamespace                  = "openshift-cluster-csi-drivers"
	driverOperandName                 = "vmware-vsphere-csi-driver"
	resyncDuration                    = 20 * time.Minute
	envVMWareVsphereDriverSyncerImage = "VMWARE_VSPHERE_SYNCER_IMAGE"
	storageClassControllerName        = "VMwareVSphereDriverStorageClassController"
	storageClassName                  = "thin-csi"
)

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
	recorder events.Recorder,
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
		name:            name,
		targetNamespace: targetNamespace,
		kubeClient:      apiClients.KubeClient,
		operatorClient:  apiClients.OperatorClient,
		configMapLister: configMapInformer.Lister(),
		secretLister:    apiClients.SecretInformer.Lister(),
		csiNodeLister:   csiNodeLister,
		scLister:        scInformer.Lister(),
		csiDriverLister: csiDriverLister,
		nodeLister:      nodeLister,
		apiClients:      apiClients,
		eventRecorder:   rc,
		vSphereChecker:  newVSphereEnvironmentChecker(),
		infraLister:     infraInformer.Lister(),
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

	if opSpec.ManagementState != operatorapi.Managed {
		klog.V(4).Infof("%s: ManagementState is %s, skipping", c.name, opSpec.ManagementState)
		return nil
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

	if c.vsphereConnectionFunc != nil {
		c.vSphereConnection, connectionResult = c.vsphereConnectionFunc()
	} else {
		connectionResult = c.loginToVCenter(ctx, infra)
	}
	defer func() {
		klog.V(4).Infof("%s: vcenter-csi logging out from vcenter", c.name)
		if c.vSphereConnection != nil {
			err := c.vSphereConnection.Logout(ctx)
			if err != nil {
				klog.Errorf("%s: error closing connection to vCenter API: %v", c.name, err)
			}
			c.vSphereConnection = nil
		}
	}()

	// if there was an OCP error we should degrade the cluster or if we previously created CSIDriver
	// but we can't connect to vcenter now, we should also degrade the cluster
	err, degradedOrBlockedFromUpgrades := c.blockUpgradeOrDegradeCluster(ctx, connectionResult, infra, opStatus)
	if err != nil {
		return err
	}
	if degradedOrBlockedFromUpgrades {
		return nil
	}

	// sync storage class
	if c.storageClassController != nil {
		storageClassAPIDeps := c.getCheckAPIDependency(infra)
		err = c.storageClassController.Sync(ctx, c.vSphereConnection, storageClassAPIDeps)
		// storageclass sync will only return error if somehow updating conditions fails, in which case
		// we can return error here and degrade the cluster
		if err != nil {
			return err
		}
	}

	delay, result, checkRan := c.runClusterCheck(ctx, infra)

	// if checks did not run
	if !checkRan {
		return nil
	}
	queue := syncContext.Queue()
	queueKey := syncContext.QueueKey()

	klog.V(2).Infof("Scheduled the next check in %s", delay)
	time.AfterFunc(delay, func() {
		queue.Add(queueKey)
	})

	err, degradedOrBlockedFromUpgrades = c.blockUpgradeOrDegradeCluster(ctx, result, infra, opStatus)
	if err != nil {
		return err
	}

	// if checks failed, we should exit potentially without starting CSI driver
	if degradedOrBlockedFromUpgrades {
		return nil
	}

	// if operand was not started previously and block upgrade is false and clusterdegrade is also false
	// then and only then we should start CSI driver operator
	if !c.operandControllerStarted && !degradedOrBlockedFromUpgrades {
		go c.runConditionalController(ctx)
		c.operandControllerStarted = true
	}
	return c.updateConditions(ctx, c.name, checks.MakeClusterCheckResultPass(), opStatus, operatorapi.ConditionTrue)
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
	status *operatorapi.OperatorStatus) (error, bool) {

	var clusterCondition string
	clusterStatus, result := checks.CheckClusterStatus(result, c.getCheckAPIDependency(infra))
	switch clusterStatus {
	case checks.ClusterCheckDegrade:
		clusterCondition = "degraded"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		return result.CheckError, true
	case checks.ClusterCheckUpgradeStateUnknown:
		clusterCondition = "upgrade_unknown"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		updateError := c.updateConditions(ctx, c.name, result, status, operatorapi.ConditionUnknown)
		return updateError, true
	case checks.ClusterCheckBlockUpgrade:
		clusterCondition = "upgrade_blocked"
		utils.InstallErrorMetric.WithLabelValues(string(result.CheckStatus), clusterCondition).Set(1)
		updateError := c.updateConditions(ctx, c.name, result, status, operatorapi.ConditionFalse)
		return updateError, true
	}
	return nil, false
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

	checkOpts := checks.NewCheckArgs(c.vSphereConnection, checkerApiClient)
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

	err := c.vSphereConnection.Connect(ctx)
	if err != nil {
		result := checks.ClusterCheckResult{
			CheckError:   err,
			BlockUpgrade: true,
			CheckStatus:  checks.CheckStatusVSphereConnectionFailed,
			Reason:       fmt.Sprintf("Failed to connect to vSphere: %v", err),
		}
		return result
	}
	return checks.MakeClusterCheckResultPass()
}

func (c *VSphereController) createVCenterConnection(ctx context.Context, infra *ocpv1.Infrastructure) error {
	klog.V(3).Infof("Creating vSphere connection")
	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get cloud config: %v", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}
	cfg := new(vsphere.VSphereConfig)
	err = gcfg.ReadStringInto(cfg, cfgString)
	if err != nil {
		return err
	}

	secret, err := c.secretLister.Secrets(c.targetNamespace).Get(secretName)
	if err != nil {
		return err
	}
	userKey := cfg.Workspace.VCenterIP + "." + "username"
	username, ok := secret.Data[userKey]
	if !ok {
		return fmt.Errorf("error parsing secret %q: key %q not found", secretName, userKey)
	}
	passwordKey := cfg.Workspace.VCenterIP + "." + "password"
	password, ok := secret.Data[passwordKey]
	if !ok {
		return fmt.Errorf("error parsing secret %q: key %q not found", secretName, passwordKey)
	}

	vs := vclib.NewVSphereConnection(string(username), string(password), cfg)
	c.vSphereConnection = vs
	return nil
}

func (c *VSphereController) updateConditions(
	ctx context.Context,
	name string,
	lastCheckResult checks.ClusterCheckResult,
	status *operatorapi.OperatorStatus,
	upgradeStatus operatorapi.ConditionStatus) error {
	availableCnd := operatorapi.OperatorCondition{
		Type:   name + operatorapi.OperatorStatusTypeAvailable,
		Status: operatorapi.ConditionTrue,
	}

	updateFuncs := []v1helpers.UpdateStatusFunc{}
	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(availableCnd))
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
		conditionChanged = false
		blockUpgradeMessage = ""
	}

	if len(blockUpgradeMessage) > 0 {
		klog.Warningf(blockUpgradeMessage)
	}
	if conditionChanged {
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

func (c *VSphereController) createStorageClassController() *storageclasscontroller.StorageClassController {
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
		c.eventRecorder,
	)
	return storageClassController
}
