package cnsmigration

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"github.com/vmware/govmomi/cns/types"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/soap"
	vim "github.com/vmware/govmomi/vim25/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

const (
	vSphereCSIDriverName = "csi.vsphere.vmware.com"
	cloudConfigNamespace = "openshift-config"
	clientOperatorName   = "cns-migrator"
	cloudCredSecretName  = "vmware-vsphere-cloud-credentials"
	secretNamespace      = "openshift-cluster-csi-drivers"
)

var (
	minRequirement = utils.PatchVersionRequirements{
		MinimumVersion7Series: "7.0.3",
		MinimumBuild7Series:   23788036,
		MinimumVersion8Series: "8.0.2",
		MinimumBuild8Series:   23504390,
	}
)

type CNSVolumeMigrator struct {
	clientSet                  kubernetes.Interface
	destinationDatastore       string
	sourceDatastore            string
	vSphereConnection          *vclib.VSphereConnection
	kubeConfig                 *rest.Config
	openshiftConfigClientSet   configclient.Interface
	openshiftOperatorClientSet operatorclient.Interface

	// for storing result and matches
	matchingCnsVolumes []types.CnsVolume
	usedVolumeCache    *inUseVolumeStore

	destinationDatastoreObject *object.Datastore

	// for counting migrated volumes and stuff
	migratedVolumes int
	volumesNotFound int
	failedToMigrate int
}

func NewCNSVolumeMigrator(config *rest.Config, dsSource, dsTarget string) *CNSVolumeMigrator {
	return &CNSVolumeMigrator{
		kubeConfig:           config,
		sourceDatastore:      dsSource,
		destinationDatastore: dsTarget,
	}
}

func (c *CNSVolumeMigrator) createOpenshiftClients() {
	// create the clientset
	c.clientSet = kubernetes.NewForConfigOrDie(c.kubeConfig)

	c.openshiftConfigClientSet = configclient.NewForConfigOrDie(rest.AddUserAgent(c.kubeConfig, clientOperatorName))
	c.openshiftOperatorClientSet = operatorclient.NewForConfigOrDie(rest.AddUserAgent(c.kubeConfig, clientOperatorName))
}

func (c *CNSVolumeMigrator) StartMigration(ctx context.Context, volumeFile string) error {
	// find existing CSI based persistent volumes, which same datastore as one mentioned in the source
	c.createOpenshiftClients()

	err := c.loginToVCenter(ctx)
	if err != nil {
		printError("error logging into vcenter: %v", err)
		return err
	}

	// check if the vCenter version is supported
	err = c.checkRequiredVersion()
	if err != nil {
		printError("error checking minimum version of vCenter that supports volume migration: %v", err)
		return err
	}

	printGreenInfo("vCenter version supports CNS volume Migration")

	printGreenInfo("logging successfully to vcenter")

	err = c.getCNSVolumes(ctx, c.sourceDatastore)
	if err != nil {
		printError("error listing cns volumes: %v", err)
		return err
	}
	pvList, err := c.clientSet.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	// TODO: Group this by namespace, so as to reduce memory used by the list
	podList, err := c.clientSet.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	c.usedVolumeCache = NewInUseStore(pvList.Items)
	c.usedVolumeCache.addAllPods(podList.Items)
	err = c.findAndMigrateCSIVolumes(ctx, volumeFile)
	c.printSummary()
	return err
}

func (c *CNSVolumeMigrator) printSummary() {
	printGreenInfo("----------- Migration Summary ------------")
	printGreenInfo("Migrated %d volumes", c.migratedVolumes)
	printGreenInfo("Failed to migrate %d volumes", c.failedToMigrate)
	printGreenInfo("Volumes not found %d", c.volumesNotFound)
	printGreenInfo("----------- End of Summary ------------")
}

func (c *CNSVolumeMigrator) findAndMigrateCSIVolumes(ctx context.Context, volumeFile string) error {
	fileBytes, err := os.ReadFile(volumeFile)
	if err != nil {
		msg := fmt.Errorf("error reading file %s: %v", volumeFile, err)
		printErrorObject(msg)
		return msg
	}

	if len(fileBytes) == 0 {
		msg := fmt.Errorf("file %s has no listed volumes", volumeFile)
		return msg
	}

	c.destinationDatastoreObject, err = c.getDatastore(ctx, c.destinationDatastore)
	if err != nil {
		return fmt.Errorf("error finding destination datastore %s: %v", c.destinationDatastore, err)
	}

	volumeLines := strings.Split(string(fileBytes), "\n")
	for _, volumeLine := range volumeLines {
		pvName := strings.TrimSpace(volumeLine)
		if pvName == "" {
			continue
		}
		printGreenInfo("Starting migration for pv %s", pvName)
		pv, err := c.clientSet.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})

		// although we failed to find this PV, we will continue migrating other PVs
		if err != nil {
			msg := fmt.Errorf("error finding pv %s: %v", pvName, err)
			c.volumesNotFound++
			printErrorObject(msg)
			continue
		}

		csiSource := pv.Spec.CSI
		if csiSource != nil && csiSource.Driver == vSphereCSIDriverName {
			if c.checkForDatastore(pv) {
				err = c.migrateVolume(ctx, csiSource.VolumeHandle)
				if err != nil {
					c.failedToMigrate++
					printError("error migrating volume %s with error: %v", pvName, err)
				} else {
					c.migratedVolumes++
					printGreenInfo("successfully migrated pv %s with volumeID: %s", pvName, csiSource.VolumeHandle)
				}
			}
		} else {
			printGreenInfo("pv %s is not a vSphere CSI volume", pvName)
		}
	}
	return nil
}

func (c *CNSVolumeMigrator) migrateVolume(ctx context.Context, volumeID string) error {
	relocatedSpec := types.NewCnsBlockVolumeRelocateSpec(volumeID, c.destinationDatastoreObject.Reference())
	task, err := c.vSphereConnection.CnsClient().RelocateVolume(ctx, relocatedSpec)
	if err != nil {
		// Handle case when target DS is same as source DS, i.e. volume has
		// already relocated.
		if soap.IsSoapFault(err) {
			soapFault := soap.ToSoapFault(err)
			printError("type of fault: %v. SoapFault Info: %v", reflect.TypeOf(soapFault.VimFault()), soapFault)
			_, isAlreadyExistErr := soapFault.VimFault().(vim.AlreadyExists)
			if isAlreadyExistErr {
				// Volume already exists in the target SP, hence return success.
				return nil
			}
		}
		return err
	}
	taskInfo, err := task.WaitForResultEx(ctx)
	if err != nil {
		msg := fmt.Errorf("error waiting for relocation task: %v", err)
		printErrorObject(msg)
		return msg
	}
	results := taskInfo.Result.(types.CnsVolumeOperationBatchResult)
	for _, result := range results.VolumeResults {
		fault := result.GetCnsVolumeOperationResult().Fault
		if fault != nil {
			printError("Error relocating volume %v: %+v ", volumeID, fault)
			return fmt.Errorf(fault.LocalizedMessage)
		}
	}
	return nil
}

func (c *CNSVolumeMigrator) checkForDatastore(pv *v1.PersistentVolume) bool {
	csiSource := pv.Spec.CSI
	vh := csiSource.VolumeHandle
	for _, cnsVolume := range c.matchingCnsVolumes {
		if cnsVolume.VolumeId.Id == vh {
			printGreenInfo("found a volume to migrate: %s", vh)
			pvcName, podName, inUseFlag := c.usedVolumeCache.volumeInUse(volumeHandle(vh))
			if inUseFlag {
				c.failedToMigrate++
				printError("volume %s is being used by pod %s in pvc %s", vh, podName, pvcName)
				return false
			}
			return true
		}
	}
	c.volumesNotFound++
	printInfo("Unable to find volume %s, with handle %s in CNS datastore %s", pv.Name, vh, c.sourceDatastore)
	return false
}

func (c *CNSVolumeMigrator) getCNSVolumes(ctx context.Context, dsName string) error {
	ds, err := c.getDatastore(ctx, dsName)
	if err != nil {
		return err
	}

	cnsQueryFilter := types.CnsQueryFilter{
		Datastores: []vim.ManagedObjectReference{ds.Reference()},
	}
	for {
		res, err := c.vSphereConnection.CnsClient().QueryVolume(ctx, cnsQueryFilter)
		if err != nil {
			return err
		}
		c.matchingCnsVolumes = append(c.matchingCnsVolumes, res.Volumes...)
		if res.Cursor.Offset == res.Cursor.TotalRecords || len(res.Volumes) == 0 {
			break
		}
		cnsQueryFilter.Cursor = &res.Cursor
	}
	return nil
}

func (c *CNSVolumeMigrator) getDatastore(ctx context.Context, dsName string) (*object.Datastore, error) {
	finder := find.NewFinder(c.vSphereConnection.VimClient(), false)
	dc, err := finder.Datacenter(ctx, c.vSphereConnection.DefaultDatacenter())
	if err != nil {
		return nil, fmt.Errorf("can't find datacenters %s", c.vSphereConnection.DefaultDatacenter())
	}

	finder.SetDatacenter(dc)
	ds, err := finder.Datastore(ctx, dsName)
	if err != nil {
		return nil, fmt.Errorf("error finding datastore %s in datacenter %s", dsName, c.vSphereConnection.DefaultDatacenter())
	}
	return ds, nil
}

func (c *CNSVolumeMigrator) loginToVCenter(ctx context.Context) error {
	infra, err := c.openshiftConfigClientSet.ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		printError("error getting infrastructure object: %v", infra)
		return err
	}

	klog.V(3).Infof("Creating vSphere connection")

	// TODO: Here we need to change to new style config w/ INI and YAML support.
	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.clientSet.CoreV1().ConfigMaps(cloudConfigNamespace).Get(ctx, cloudConfig.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get cloud config: %v", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}

	config := new(vclib.VSphereConfig)
	err = config.LoadConfig(cfgString)
	if err != nil {
		return err
	}

	// For multi vCenter, this tool only handles 1 vCenter.  Until parameter specifying which vCenter, we'll just assume
	// INI config is in use and just grab from workspace default vCenter for now.
	if len(config.Config.VirtualCenter) > 1 {
		return fmt.Errorf("multiple vCenters detected in cloud config")
	}

	var vCenter string
	if config.LegacyConfig != nil {
		vCenter = config.LegacyConfig.Workspace.VCenterIP
	} else {
		// If we are here, its YAML cloud config with one vCenter.  We can grab from map.
		for vCenterName := range config.Config.VirtualCenter {
			vCenter = vCenterName
		}
	}

	secret, err := c.clientSet.CoreV1().Secrets(secretNamespace).Get(ctx, cloudCredSecretName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	userKey := vCenter + "." + "username"
	username, ok := secret.Data[userKey]
	if !ok {
		return fmt.Errorf("error parsing secret %q: key %q not found", cloudCredSecretName, userKey)
	}
	passwordKey := vCenter + "." + "password"
	password, ok := secret.Data[passwordKey]
	if !ok {
		return fmt.Errorf("error parsing secret %q: key %q not found", cloudCredSecretName, passwordKey)
	}
	vs := vclib.NewVSphereConnection(string(username), string(password), vCenter, config)
	c.vSphereConnection = vs

	if err = c.vSphereConnection.Connect(ctx); err != nil {
		return fmt.Errorf("error connecting to vcenter: %v", err)
	}

	if err = c.vSphereConnection.LoginToCNS(ctx); err != nil {
		return fmt.Errorf("error logging into CNS: %v", err)
	}
	return nil
}

func (c *CNSVolumeMigrator) checkRequiredVersion() error {
	apiVersion, build, err := c.vSphereConnection.GetVersionInfo()
	if err != nil {
		return fmt.Errorf("error getting vCenter version: %v", err)
	}
	match, message, err := utils.CheckForMinimumPatchedVersion(minRequirement, apiVersion, build)
	if err != nil {
		return fmt.Errorf("error checking for minimum version: %v", err)
	}
	if !match {
		return fmt.Errorf("vCenter version %s:%s is not supported: minimum supported version is - %s", apiVersion, build, message)
	}
	return nil
}
