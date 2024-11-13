package cnsmigrationcontroller

import (
	"context"
	"fmt"
	"github.com/davecgh/go-spew/spew"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/vmware/govmomi/vslm"
	"gopkg.in/gcfg.v1"
	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/legacy-cloud-providers/vsphere"
	"net/url"
	"os"
	"regexp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/google/uuid"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	cnstypes "github.com/vmware/govmomi/cns/types"
	cnsvolume "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/volume"
	cnsvsphere "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/vsphere"
	cnsconfig "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/config"
)

const (
	resyncPeriod        = 1 * time.Hour
	vsphereCsiConfig    = "vsphere-csi-config"
	csiConfigLocation   = "/tmp/vsphere-csi.conf"
	EnvVSphereCSIConfig = "VSPHERE_CSI_CONFIG"
	cnsMigrationAck413  = "ack-4.13-kube-127-cns-migration-in-4.14"
)

type CNSMigrationController struct {
	name                     string
	namespace                string
	operatorClient           v1helpers.OperatorClient
	dynamicClient            dynamic.Interface
	kubeClient               kubernetes.Interface
	k8sClient                client.Client
	ApiExtClient             apiextclient.Interface
	pvLister                 corelister.PersistentVolumeLister
	configMapLister          corelister.ConfigMapLister
	managedConfigMapLister   corelister.ConfigMapLister
	secretLister             corelister.SecretLister
	openshiftConfigClientSet configclient.Interface
	eventRecorder            events.Recorder
	config                   *vsphere.VSphereConfig //This config is loaded from config map in openshift-config namespace
	driverConfig             *cnsconfig.Config      //This config is loaded from config map in openshift-cluster-csi-drivers namespace
	vCenter                  *cnsvsphere.VirtualCenter
}

func NewCNSMigrationController(
	name, namespace string,
	apiClients utils.APIClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder) factory.Controller {

	c := &CNSMigrationController{
		name:                     name,
		namespace:                namespace,
		operatorClient:           apiClients.OperatorClient,
		dynamicClient:            apiClients.DynamicClient,
		kubeClient:               apiClients.KubeClient,
		ApiExtClient:             apiClients.ApiExtClient,
		pvLister:                 kubeInformers.InformersFor("").Core().V1().PersistentVolumes().Lister(),
		configMapLister:          kubeInformers.InformersFor(utils.CloudConfigNamespace).Core().V1().ConfigMaps().Lister(),
		managedConfigMapLister:   kubeInformers.InformersFor(utils.ManagedConfigNamespace).Core().V1().ConfigMaps().Lister(),
		secretLister:             apiClients.SecretInformer.Lister(),
		openshiftConfigClientSet: apiClients.ConfigClientSet,
		eventRecorder:            eventRecorder.WithComponentSuffix("cns-migration-controller"),
	}

	return factory.New().
		WithSync(c.sync).
		ResyncEvery(resyncPeriod).
		WithSyncDegradedOnError(c.operatorClient).
		ToController(name, c.eventRecorder)
}

func (c *CNSMigrationController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("CNSMigrationController sync started")
	defer klog.V(4).Infof("CNSMigrationController sync completed")

	failedVolumes, err := c.registerCNSDisks(ctx)
	if err != nil {
		if len(failedVolumes) > 0 {
			// We know that specifically CNS registration failed for some volumes, it can be a long list so log it instead of creating an event.
			err = fmt.Errorf("failed to register CNS volumes: %v", failedVolumes)
			klog.Error(err)
			c.eventRecorder.Warning("CNSMigrationFailed", "Some volumes failed to register with CNS (see CSI Driver Operator logs for details). Admin acknowledgement will be required to upgrade the cluster and it is strongly recommended to resolve the issues before upgrading the cluster.")
			err = c.addAdminAck(ctx)
			if err != nil {
				klog.Errorf("failed to add admin-ack: %v", err)
				return err
			}
			return err
		}
		return fmt.Errorf("error during CNS registration process: %v", err)
	}

	// No issue occurred during CNS registration, make sure there is no admin-ack.
	c.removeAdminAck(ctx)

	return nil
}

func (c *CNSMigrationController) registerCNSDisks(ctx context.Context) (map[string]error, error) {
	// Find all vSphere intree PVs.
	pvs, err := c.listInTreePVs(ctx)
	if err != nil {
		klog.Errorf("failed to find vSphere intree PVs: %v", err)
		return nil, err
	}
	if len(pvs) == 0 {
		// No vSphere intree PVs found - nothing to do.
		klog.V(4).Infof("no vSphere intree PVs found")
		return nil, nil
	}

	// Get vCenter.
	vCenter, err := c.connectToVCenter(ctx)
	if err != nil {
		klog.Errorf("failed to connect to vCenter: %v", err)
		return nil, err
	}

	datacenterPaths, err := c.getDatacenterPaths(ctx, vCenter, c.config)
	if err != nil {
		klog.Errorf("failed to get datacenter paths: %v", err)
		return nil, err
	}

	manager, err := cnsvolume.GetManager(ctx, vCenter, nil, false, true, false, false)
	if err != nil {
		klog.Errorf("failed to get manager. err: %v", err)
		return nil, err
	}

	re := regexp.MustCompile(`\[([^\[\]]*)\]`)

	failedVolumes := make(map[string]error)
	for _, pv := range pvs {
		volumePath := pv.Spec.VsphereVolume.VolumePath

		if !re.MatchString(volumePath) {
			err = fmt.Errorf("failed to extract datastore name from in-tree volume path: %q", volumePath)
			klog.Error(err)
			failedVolumes[pv.Name] = err
			continue
		}

		datastoreFullPath := re.FindAllString(volumePath, -1)[0]
		vmdkPath := strings.TrimSpace(strings.TrimPrefix(volumePath, datastoreFullPath))
		datastoreFullPath = strings.Trim(strings.Trim(datastoreFullPath, "["), "]")
		datastorePathSplit := strings.Split(datastoreFullPath, "/")
		datastoreName := datastorePathSplit[len(datastorePathSplit)-1]

		var containerClusterArray []cnstypes.CnsContainerCluster
		containerCluster := cnsvsphere.GetContainerCluster(c.driverConfig.Global.ClusterID, c.config.Global.User, cnstypes.CnsClusterFlavorVanilla, "vanilla")
		containerClusterArray = append(containerClusterArray, containerCluster)
		uuid, err := uuid.NewUUID()
		if err != nil {
			klog.Errorf("failed to generate uuid: %v", err)
			failedVolumes[pv.Name] = err
			continue
		}
		createSpec := &cnstypes.CnsVolumeCreateSpec{
			Name:       uuid.String(),
			VolumeType: common.BlockVolumeType,
			Metadata: cnstypes.CnsVolumeMetadata{
				ContainerCluster:      containerCluster,
				ContainerClusterArray: containerClusterArray,
			},
		}

		// Search for the current volume in all datacenters, stop if found.
		var volumeInfo *cnsvolume.CnsVolumeInfo
		for _, datacenter := range datacenterPaths {
			// Check vCenter API Version
			// Format:
			// https://<vc_ip>/folder/<vm_vmdk_path>?dcPath=<datacenter-path>&dsName=<datastoreName>
			backingDiskURLPath := "https://" + c.config.Workspace.VCenterIP + "/folder/" +
				vmdkPath + "?dcPath=" + url.PathEscape(datacenter) + "&dsName=" + url.PathEscape(datastoreName)
			bUseVslmAPIs, err := common.UseVslmAPIs(ctx, vCenter.Client.ServiceContent.About)
			if err != nil {
				klog.Errorf("failed to determine the correct APIs to use for vSphere version %q, Error= %+v", vCenter.Client.ServiceContent.About.ApiVersion, err)
				return nil, err
			}
			if bUseVslmAPIs {
				backingObjectID, err := c.registerDisk(ctx, backingDiskURLPath, volumePath)
				if err != nil {
					klog.Errorf("failed to register %v: %v", volumePath, err)
					return nil, err
				}
				createSpec.BackingObjectDetails = &cnstypes.CnsBlockBackingDetails{BackingDiskId: backingObjectID}
				klog.V(4).Infof("Registering volume: %q using backingDiskId :%q", volumePath, backingObjectID)
			} else {
				createSpec.BackingObjectDetails = &cnstypes.CnsBlockBackingDetails{BackingDiskUrlPath: backingDiskURLPath}
				klog.V(4).Infof("Registering volume: %q using backingDiskURLPath :%q", volumePath, backingDiskURLPath)
			}
			klog.V(6).Infof("vSphere CSI operator registering volume %q with create spec %+v", volumePath, spew.Sdump(createSpec))
			// This is the CNS registration call where we surface the issues before upgrading clusters.
			volumeInfo, _, err = manager.CreateVolume(ctx, createSpec)
			if err != nil {
				err = fmt.Errorf("failed to register volume %q: %+v", volumePath, err)
				klog.Error(err)
				failedVolumes[pv.Name] = err
				continue
			} else {
				klog.V(4).Infof("Successfully registered volume %q as container volume with ID: %q", volumePath, volumeInfo.VolumeID.Id)
				break
			}
		}
	}

	if len(failedVolumes) > 0 {
		return failedVolumes, fmt.Errorf("failed to register %d volumes to CNS", len(failedVolumes))
	}

	return nil, nil
}

// listInTreePVs lists all the in-tree vSphere persistent volumes based on following criteria:
// 1. PV is annotated with "pv.kubernetes.io/provisioned-by: kubernetes.io/vsphere-volume"
// 2. PV has VsphereVolume field
func (c *CNSMigrationController) listInTreePVs(ctx context.Context) ([]*corev1.PersistentVolume, error) {
	pvs, err := c.pvLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("failed to list PVs: %v", err)
		return nil, err
	}

	var filteredVolumes []*corev1.PersistentVolume
	vsphereVolumeNames := []string{} //For logging only
	for _, pv := range pvs {
		if value, ok := pv.Annotations[common.AnnDynamicallyProvisioned]; ok && value == common.InTreePluginName {
			if pv.Spec.VsphereVolume != nil {
				filteredVolumes = append(filteredVolumes, pv)
				vsphereVolumeNames = append(vsphereVolumeNames, pv.Name)
			}
		}
	}
	klog.V(5).Infof("Found %d vSphere intree volumes: %v", len(vsphereVolumeNames), vsphereVolumeNames)

	return filteredVolumes, nil
}

func (c *CNSMigrationController) getDatacenterPaths(ctx context.Context, vCenter *cnsvsphere.VirtualCenter, config *vsphere.VSphereConfig) ([]string, error) {
	datacenters := c.config.Workspace.Datacenter
	datacenterPaths := make([]string, 0)
	if datacenters != "" {
		datacenterPaths = strings.Split(datacenters, ",")
	} else {
		// Get all datacenters from vCenter.
		dcs, err := vCenter.GetDatacenters(ctx)
		if err != nil {
			klog.Errorf("failed to get datacenters from vCenter. err: %v", err)
			return nil, err
		}
		for _, dc := range dcs {
			datacenterPaths = append(datacenterPaths, dc.InventoryPath)
		}
		klog.V(4).Infof("retrieved all datacenters %v from vCenter", datacenterPaths)
	}
	klog.V(4).Infof("found datacenter : %+v", datacenterPaths)

	return datacenterPaths, nil
}

func (c *CNSMigrationController) registerDisk(ctx context.Context, path string, name string) (string, error) {
	err := c.vCenter.ConnectVslm(ctx)
	if err != nil {
		klog.Errorf("ConnectVslm failed with err: %+v", err)
		return "", err
	}
	globalObjectManager := vslm.NewGlobalObjectManager(c.vCenter.VslmClient)
	vStorageObject, err := globalObjectManager.RegisterDisk(ctx, path, name)
	if err != nil {
		alreadyExists, objectID := cnsvsphere.IsAlreadyExists(err)
		if alreadyExists {
			klog.V(4).Infof("vStorageObject: %q, already exists and registered as FCD, returning success", objectID)
			return objectID, nil
		}
		klog.Errorf("failed to register virtual disk %q as first class disk with err: %v", path, err)
		return "", err
	}
	return vStorageObject.Config.Id.Id, nil
}

func (c *CNSMigrationController) addAdminAck(ctx context.Context) error {
	adminGate, err := c.managedConfigMapLister.ConfigMaps(utils.ManagedConfigNamespace).Get(utils.AdminGateConfigMap)
	if err != nil {
		klog.Errorf("failed to get admin-gate configmap: %v", err)
		return err
	}

	klog.V(2).Infof("Updating admin-gates to require admin-ack for vSphere CSI migration")

	_, ok := adminGate.Data[cnsMigrationAck413]
	if !ok {
		adminGate.Data[cnsMigrationAck413] = "vSphere CSI migration will be enabled in Openshift-4.14. Your cluster appears to be using in-tree vSphere volumes and is on a vSphere version that has CSI migration related bugs. See - https://access.redhat.com/node/7011683 for more information, before upgrading to 4.14."

	}
	_, _, err = resourceapply.ApplyConfigMap(ctx, c.kubeClient.CoreV1(), c.eventRecorder, adminGate)
	return err
}

func (c *CNSMigrationController) removeAdminAck(ctx context.Context) error {
	adminGate, err := c.managedConfigMapLister.ConfigMaps(utils.ManagedConfigNamespace).Get(utils.AdminGateConfigMap)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get admin-gate configmap: %v", err)
	}

	_, ok := adminGate.Data[cnsMigrationAck413]
	// nothing needs to be done if key doesn't exist
	if !ok {
		return nil
	}
	klog.V(2).Infof("removing admin-gates that is required for CSI migration")

	delete(adminGate.Data, cnsMigrationAck413)
	_, _, err = resourceapply.ApplyConfigMap(ctx, c.kubeClient.CoreV1(), c.eventRecorder, adminGate)
	return err
}

func (c *CNSMigrationController) connectToVCenter(ctx context.Context) (*cnsvsphere.VirtualCenter, error) {
	err := c.configureVSphereConnection(ctx)
	if err != nil {
		klog.Errorf("failed to configure vSphere connection: %v", err)
		return nil, err
	}

	err = c.connect(ctx)
	if err != nil {
		klog.Errorf("failed to connect to vCenter: %v", err)
		return nil, err
	}

	return c.vCenter, nil
}

func (c *CNSMigrationController) connect(ctx context.Context) error {
	// configureVSphereConnection has to be run before attempting to connect.
	if c.config == nil || c.driverConfig == nil {
		return fmt.Errorf("vSphere connection configuration is missing")
	}

	// Get vSphere configuration from the ConfigMap in CSI driver namespace instead of the global one in openshift-config namespace.
	// This is because we need to create vCenter configs with `GetVirtualCenterConfigs` which can only parse this config.
	csiConfigMap, err := c.kubeClient.CoreV1().ConfigMaps(utils.DefaultNamespace).Get(ctx, vsphereCsiConfig, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("failed to get configmap %s: %v", vsphereCsiConfig, err)
		return err
	}
	// The function for connecting to vCenter (virtualCenter.Connect) assumes that configuration is stored as a file under path in VSPHERE_CSI_CONFIG.
	err = c.writeCsiConfig(csiConfigMap, csiConfigLocation)
	if err != nil {
		klog.Errorf("failed to write csi driver config %v: %v", csiConfigLocation, err)
		return err
	}
	os.Setenv(EnvVSphereCSIConfig, csiConfigLocation)

	virtualCenterConfigs, err := cnsvsphere.GetVirtualCenterConfigs(ctx, c.driverConfig)
	if err != nil {
		klog.Errorf("failed to get virtual center config: %v", err)
		return err
	}

	virtualCenterConfig := virtualCenterConfigs[0]
	virtualCenter := &cnsvsphere.VirtualCenter{
		ClientMutex: &sync.Mutex{},
		Config:      virtualCenterConfig,
	}

	err = virtualCenter.Connect(ctx)
	if err != nil {
		klog.Errorf("failed to connect to vCenter: %v", err)
		return err
	}

	c.vCenter = virtualCenter

	return nil
}

// configureVSphereConnection loads the configuration data from configmaps into CNSMigrationController
func (c *CNSMigrationController) configureVSphereConnection(ctx context.Context) error {

	infra, err := c.openshiftConfigClientSet.ConfigV1().Infrastructures().Get(ctx, utils.InfraGlobalName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting infrastructure object: %v", err)
		return err
	}

	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(utils.CloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		klog.Errorf("error getting config map %v/%v: %v", utils.CloudConfigNamespace, cloudConfig.Name, err)
		return err
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		err := fmt.Errorf("cloud config %s/%s does not contain key %q", utils.CloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
		klog.Errorf(err.Error())
		return err
	}

	var config vsphere.VSphereConfig
	err = gcfg.ReadStringInto(&config, cfgString)
	if err != nil {
		klog.Errorf("error parsing vSphere config: %v", err)
		return err
	}

	secret, err := c.kubeClient.CoreV1().Secrets(utils.DefaultNamespace).Get(ctx, utils.SecretName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting secret %v/%v: %v", utils.SecretName, utils.DefaultNamespace, err)
		return err
	}

	userKey := config.Workspace.VCenterIP + "." + "username"
	username, ok := secret.Data[userKey]
	if !ok {
		err := fmt.Errorf("error parsing secret %q: key %q not found", utils.SecretName, userKey)
		klog.Errorf(err.Error())
		return err
	}
	os.Setenv("VSPHERE_USER", string(username))

	config.Global.User = string(username)
	passwordKey := config.Workspace.VCenterIP + "." + "password"
	password, ok := secret.Data[passwordKey]
	if !ok {
		err := fmt.Errorf("error parsing secret %q: key %q not found", utils.SecretName, passwordKey)
		klog.Errorf(err.Error())
		return err
	}
	os.Setenv("VSPHERE_PASSWORD", string(password))
	c.config = &config

	// With credentials set now we can call ReadConfig to prepare the driver config.
	cm, err := c.kubeClient.CoreV1().ConfigMaps(utils.DefaultNamespace).Get(ctx, vsphereCsiConfig, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting configmap %s: %v", vsphereCsiConfig, err)
		return err
	}
	cfgString, ok = cm.Data["cloud.conf"]
	if !ok {
		return fmt.Errorf("error getting csi config: cloud.conf key not found in configmap")
	}
	driverConfig, err := cnsconfig.ReadConfig(ctx, strings.NewReader(cfgString))
	if err != nil {
		klog.Errorf("error reading driver config: %v", err)
		return err
	}
	c.driverConfig = driverConfig

	return nil
}

func (c *CNSMigrationController) writeCsiConfig(configMap *corev1.ConfigMap, filePath string) error {
	cfgString, ok := configMap.Data["cloud.conf"]
	if !ok {
		return fmt.Errorf("error writing csi configmap")
	}
	file, err := os.Create(filePath)
	if err != nil {
		klog.Errorf("error creating file %s: %v", filePath, err)
		return err
	}
	defer file.Close()
	_, err = file.WriteString(cfgString)
	if err != nil {
		klog.Errorf("error writing to file %s: %v", filePath, err)
		return err
	}
	return nil
}
