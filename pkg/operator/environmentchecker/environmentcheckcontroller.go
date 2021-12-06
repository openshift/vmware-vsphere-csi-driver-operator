package environmentchecker

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	"gopkg.in/gcfg.v1"
	"k8s.io/legacy-cloud-providers/vsphere"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/storageclasscontroller"

	ocpv1 "github.com/openshift/api/config/v1"
	operatorapi "github.com/openshift/api/operator/v1"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehash"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/environmentchecker/checks"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	storagelister "k8s.io/client-go/listers/storage/v1"
	"k8s.io/klog/v2"
)

type EnvironmentCheckController struct {
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
	apiClients               utils.ApiClients
	controllers              []conditionalController
	storageClassController   *storageclasscontroller.StorageClassController
	operandControllerStarted bool
	vSphereConnection        *vclib.VSphereConnection
	vSphereChecker           vSphereEnvironmentCheckInterface
}

const (
	cloudConfigNamespace              = "openshift-config"
	infraGlobalName                   = "cluster"
	secretName                        = "vmware-vsphere-cloud-credentials"
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

func NewEnvironmentCheckController(
	name, targetNamespace string,
	apiClients utils.ApiClients,
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

	c := &EnvironmentCheckController{
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

func (c *EnvironmentCheckController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	klog.V(4).Infof("%s: sync started", c.name)
	defer klog.V(4).Infof("%s: sync complete", c.name)
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
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

	delay, result, checkRan, ocpError := c.runClusterCheck(ctx, infra)
	if ocpError != nil {
		return ocpError
	}

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

	if c.shouldDegradeCluster(result) {
		return result.CheckError
	}

	// if operand was not started previously and block upgrade is false and clusterdegrade is also false
	// then and only then we should start CSI driver operator
	if !c.operandControllerStarted && !result.BlockUpgrade && !result.ClusterDegrade {
		go c.runConditionalController(ctx)
		c.operandControllerStarted = true
	}

	// sync storage class
	storageClassSyncResult := c.storageClassController.Sync(ctx, c.vSphereConnection)

	if c.shouldDegradeCluster(storageClassSyncResult) {
		return storageClassSyncResult.CheckError
	}

	storageClassUpdateConditionError := c.updateConditions(ctx, storageClassControllerName, storageClassSyncResult)

	if storageClassUpdateConditionError != nil {
		return storageClassUpdateConditionError
	}

	return c.updateConditions(ctx, c.name, result)
}

func (c *EnvironmentCheckController) shouldDegradeCluster(result checks.ClusterCheckResult) bool {
	if result.ClusterDegrade {
		return true
	}

	if result.BlockUpgrade {
		// if we were supposed to only block upgrades but for some reason we have created CSIDriver and storageclass
		// in past and future syncing fails then we should degrade the cluster
		csiDriver, err := c.csiDriverLister.Get(utils.VSphereDriverName)

		// if we can't fetch CSI driver lets not degrade the cluster, if cluster indeed needs degrading we will catch in
		// next sync
		if err != nil {
			return false
		}
		annotations := csiDriver.GetAnnotations()
		if _, ok := annotations[utils.OpenshiftCSIDriverAnnotationKey]; ok {
			return true
		}
		sc, err := c.scLister.Get(storageClassName)
		if err != nil {
			return false
		}
		if sc != nil {
			return true
		}
	}

	return false
}

func (c *EnvironmentCheckController) runConditionalController(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(len(c.controllers))

	for i := range c.controllers {
		go func(index int) {
			cc := c.controllers[index]
			defer klog.Infof("%s controller terminated", cc.name)
			defer wg.Done()
			// if conditionController is not running and there were no errors we should run
			// those controllers
			cc.controller.Run(ctx, 1)
		}(i)
	}
	wg.Wait()
}

func (c *EnvironmentCheckController) runClusterCheck(ctx context.Context,
	infra *ocpv1.Infrastructure) (delay time.Duration, result checks.ClusterCheckResult, checkRan bool, immediateError error) {

	checkerApiClient := &checks.KubeApiInterfaceImpl{
		CSINodeLister:   c.csiNodeLister,
		CSIDriverLister: c.csiDriverLister,
		NodeLister:      c.nodeLister,
	}

	if c.vSphereConnection != nil {
		secretConfigHash, err := c.getSecretConfigMapHash(ctx, infra)
		if err != nil {
			immediateError = err
			return
		}

		if secretConfigHash == c.vSphereConnection.SecretConfigMapHash {
			checkOpts := checks.NewCheckArgs(c.vSphereConnection, checkerApiClient)
			delay, result, checkRan = c.vSphereChecker.Check(ctx, checkOpts)
			return
		}
		klog.V(3).Infof("vSphere configuration changed, disconnecting from vSphere")
		c.vSphereConnection.Logout(ctx)
	}

	immediateError = c.createVCenterConnection(ctx, infra)
	if immediateError != nil {
		return
	}

	err := c.vSphereConnection.Connect(ctx)
	if err != nil {
		result = checks.ClusterCheckResult{
			CheckError:   err,
			BlockUpgrade: true,
			CheckStatus:  checks.CheckStatusVSphereConnectionFailed,
			Reason:       fmt.Sprintf("Failed to connect to vSphere: %v", err),
		}
		return
	}

	checkOpts := checks.NewCheckArgs(c.vSphereConnection, checkerApiClient)
	delay, result, checkRan = c.vSphereChecker.Check(ctx, checkOpts)
	return
}

func (c *EnvironmentCheckController) createVCenterConnection(ctx context.Context, infra *ocpv1.Infrastructure) error {
	klog.V(3).Infof("Creating vSphere connection")
	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get cloud config: %v", err)
	}
	cfgHash, err := resourcehash.GetConfigMapHash(cloudConfigMap)
	if err != nil {
		return fmt.Errorf("failed to get cloud config hash: %v", err)
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
	secretHash, err := resourcehash.GetSecretHash(secret)
	if err != nil {
		return fmt.Errorf("failed to get secret hash: %v", err)
	}
	combinedHash := fmt.Sprintf("%s.%s", cfgHash, secretHash)
	vs := vclib.NewVSphereConnection(string(username), string(password), combinedHash, cfg)
	c.vSphereConnection = vs
	runtime.SetFinalizer(vs, logout)
	return nil
}

func (c *EnvironmentCheckController) getSecretConfigMapHash(ctx context.Context, infra *ocpv1.Infrastructure) (string, error) {
	secret, err := c.secretLister.Secrets(c.targetNamespace).Get(secretName)
	if err != nil {
		return "", err
	}
	secretHash, err := resourcehash.GetSecretHash(secret)
	if err != nil {
		return "", err
	}
	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return "", err
	}
	cfgHash, err := resourcehash.GetConfigMapHash(cloudConfigMap)
	if err != nil {
		return "", err
	}
	return secretHash + "." + cfgHash, nil
}

func (c *EnvironmentCheckController) updateConditions(ctx context.Context, name string, lastCheckResult checks.ClusterCheckResult) error {
	availableCnd := operatorapi.OperatorCondition{
		Type:   name + operatorapi.OperatorStatusTypeAvailable,
		Status: operatorapi.ConditionTrue,
	}

	if lastCheckResult.CheckError != nil {
		availableCnd.Message = lastCheckResult.CheckError.Error()
		availableCnd.Reason = "SyncFailed"
	}
	updateFuncs := []v1helpers.UpdateStatusFunc{}
	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(availableCnd))
	allowUpgradeCond := operatorapi.OperatorCondition{
		Type:   name + operatorapi.OperatorStatusTypeUpgradeable,
		Status: operatorapi.ConditionTrue,
	}

	if lastCheckResult.BlockUpgrade {
		blockUpgradeMessage := fmt.Sprintf("Marking cluster un-upgradeable because %s", lastCheckResult.Reason)
		klog.Warningf(blockUpgradeMessage)
		c.eventRecorder.Warningf(string(lastCheckResult.CheckStatus), "Marking cluster un-upgradeable because %s", lastCheckResult.Reason)
		allowUpgradeCond.Status = operatorapi.ConditionFalse
		allowUpgradeCond.Message = blockUpgradeMessage
		allowUpgradeCond.Reason = string(lastCheckResult.CheckStatus)
	}

	updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(allowUpgradeCond))
	if _, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, updateFuncs...); updateErr != nil {
		return updateErr
	}
	return nil
}

func (c *EnvironmentCheckController) createStorageClassController() *storageclasscontroller.StorageClassController {
	scBytes, err := assets.ReadFile("storageclass.yaml")
	if err != nil {
		panic("unable to read storageclass file")
	}
	storageClassController := storageclasscontroller.NewStorageClassController(
		storageClassControllerName,
		defaultNamespace,
		scBytes,
		c.apiClients.KubeClient,
		c.apiClients.KubeInformers,
		c.apiClients.OperatorClient,
		c.apiClients.ConfigInformers,
		c.eventRecorder,
	)
	return storageClassController
}

func logout(vs *vclib.VSphereConnection) {
	vs.Logout(context.TODO())
}
