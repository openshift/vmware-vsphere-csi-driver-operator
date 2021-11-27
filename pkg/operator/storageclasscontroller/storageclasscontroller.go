package storageclasscontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/environmentchecker/checks"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	"k8s.io/component-base/metrics"

	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
)

const (
	infraGlobalName       = "cluster"
	cloudConfigNamespace  = "openshift-config"
	DatastoreInfoProperty = "info"
	SummaryProperty       = "summary"
	resyncDuration        = 20 * time.Minute
	failureReason         = "failure_reason"
)

type driverInstallationFailure string

const (
	vcenterConnectionError     driverInstallationFailure = "vcenter_connection_error"
	vsphereOldESXIVersion      driverInstallationFailure = "vsphere_old_esxi_version"
	vsphereOldVcenterVersion   driverInstallationFailure = "vsphere_old_vsphere_version"
	vsphereOldHWVersion        driverInstallationFailure = "vsphere_old_hw_version"
	vsphereAPIError            driverInstallationFailure = "vsphere_api_error"
	vsphereExistingDriverFound driverInstallationFailure = "vsphere_driver_already_exists"
)

var (
	installErrorMetric = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Name:           "vsphere_csi_driver_error",
			Help:           "vSphere driver installation error",
			StabilityLevel: metrics.STABLE,
		},
		[]string{failureReason},
	)
)

type StorageClassController struct {
	name            string
	targetNamespace string
	manifest        []byte
	kubeClient      kubernetes.Interface
	operatorClient  v1helpers.OperatorClient
	configMapLister corelister.ConfigMapLister
	secretLister    corelister.SecretLister
	infraLister     infralister.InfrastructureLister
	recorder        events.Recorder
}

type vSphereConnection struct {
	sharedConnection *vclib.VSphereConnection
	restClient       *rest.Client
	tagManager       *tags.Manager
}

func NewStorageClassController(
	name,
	targetNamespace string,
	manifest []byte,
	kubeClient kubernetes.Interface,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	operatorClient v1helpers.OperatorClient,
	configInformer configinformers.SharedInformerFactory,
	recorder events.Recorder,
) *StorageClassController {
	configMapInformer := kubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
	secretInformer := kubeInformers.InformersFor(targetNamespace).Core().V1().Secrets()
	infraInformer := configInformer.Config().V1().Infrastructures()
	c := &StorageClassController{
		name:            name,
		targetNamespace: targetNamespace,
		manifest:        manifest,
		kubeClient:      kubeClient,
		operatorClient:  operatorClient,
		configMapLister: configMapInformer.Lister(),
		secretLister:    secretInformer.Lister(),
		infraLister:     infraInformer.Lister(),
		recorder:        recorder,
	}
	return c
}

func (c *StorageClassController) Sync(ctx context.Context, connection *vclib.VSphereConnection) checks.ClusterCheckResult {
	policyName, syncResult := c.syncStoragePolicy(ctx, connection)
	if syncResult.CheckError != nil {
		klog.Errorf("error syncing storage policy: %v", syncResult.Reason)
		return syncResult
	}

	err := c.syncStorageClass(ctx, policyName)
	if err != nil {
		klog.Errorf("error syncing storage class: %v", err)
		return checks.MakeClusterDegradedError(checks.CheckStatusOpenshiftAPIError, err)
	}
	return checks.MakeClusterCheckResultPass()
}

func (c *StorageClassController) syncStoragePolicy(ctx context.Context, connection *vclib.VSphereConnection) (string, checks.ClusterCheckResult) {
	infra, err := c.infraLister.Get(infraGlobalName)
	if err != nil {
		reason := fmt.Errorf("error listing infra objects: %v", err)
		return "", checks.MakeClusterDegradedError(checks.CheckStatusOpenshiftAPIError, reason)
	}

	apiClient, err := newStoragePolicyAPI(ctx, connection, infra)
	if err != nil {
		reason := fmt.Errorf("error connecting to vcenter API: %v", err)
		return "", checks.MakeGenericVCenterAPIError(reason)
	}

	// we expect all API calls to finish within apiTimeout or else operator might be stuck
	tctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	policyName, err := apiClient.createStoragePolicy(tctx)
	if err != nil {
		return "", checks.MakeGenericVCenterAPIError(err)
	}
	return policyName, checks.MakeClusterCheckResultPass()
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
