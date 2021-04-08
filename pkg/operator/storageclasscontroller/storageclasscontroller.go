package storageclasscontroller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/gcfg.v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	corelister "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/legacy-cloud-providers/vsphere"

	v1 "github.com/openshift/api/operator/v1"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	infralister "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/mo"
)

const (
	infraGlobalName       = "cluster"
	cloudConfigNamespace  = "openshift-config"
	DatastoreInfoProperty = "info"
	SummaryProperty       = "summary"
	resyncDuration        = 5 * time.Minute
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
}

type vSphereConnection struct {
	client     *govmomi.Client
	config     *vsphere.VSphereConfig
	tagManager *tags.Manager
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
) factory.Controller {
	configMapInformer := kubeInformers.InformersFor(cloudConfigNamespace).Core().V1().ConfigMaps()
	secretInformer := kubeInformers.InformersFor(targetNamespace).Core().V1().Secrets()
	infraInformer := configInformer.Config().V1().Infrastructures()
	rc := recorder.WithComponentSuffix("vmware-" + strings.ToLower(name))
	c := &StorageClassController{
		name:            name,
		targetNamespace: targetNamespace,
		manifest:        manifest,
		kubeClient:      kubeClient,
		operatorClient:  operatorClient,
		configMapLister: configMapInformer.Lister(),
		secretLister:    secretInformer.Lister(),
		infraLister:     infraInformer.Lister(),
	}
	return factory.New().WithInformers(
		configMapInformer.Informer(),
		secretInformer.Informer(),
		infraInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		resyncDuration,
	).WithSyncDegradedOnError(
		operatorClient,
	).ToController(
		c.name,
		rc,
	)
}

func (c *StorageClassController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	opSpec, _, _, err := c.operatorClient.GetOperatorState()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if opSpec.ManagementState != v1.Managed {
		return nil
	}

	controllerAvailableCondition := v1.OperatorCondition{
		Type:   c.name + v1.OperatorStatusTypeAvailable,
		Status: v1.ConditionTrue,
	}

	datastoreURL, err := c.getDatastoreURL(ctx)
	if err != nil {
		klog.Errorf("error fetching datastore: %v", err)
		return err
	}

	err = c.syncStorageClass(ctx, syncContext, datastoreURL)
	if err != nil {
		klog.Errorf("error syncing storage class: %v", err)
		return err
	}
	_, _, err = v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(controllerAvailableCondition),
	)
	return err
}

func (c *StorageClassController) getDatastoreURL(ctx context.Context) (string, error) {
	infra, err := c.infraLister.Get(infraGlobalName)
	if err != nil {
		return "", err
	}

	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get cloud config: %w", err)
	}

	cfgString, ok := cloudConfigMap.Data[infra.Spec.CloudConfig.Key]
	if !ok {
		return "", fmt.Errorf("cloud config %s/%s does not contain key %q", cloudConfigNamespace, cloudConfig.Name, cloudConfig.Key)
	}

	cfg := new(vsphere.VSphereConfig)
	err = gcfg.ReadStringInto(cfg, cfgString)
	if err != nil {
		return "", err
	}

	username, password, err := c.getCredentials(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to get credentials: %w", err)
	}

	client, err := c.newClient(ctx, cfg, username, password)
	if err != nil {
		return "", fmt.Errorf("failed to create client: %w", err)
	}

	conn := &vSphereConnection{
		client: client,
		config: cfg,
	}
	ds, err := c.getDefaultDatastore(ctx, conn)
	if err != nil {
		return "", fmt.Errorf("error getting default datastore: %v", err)
	}
	return ds.Summary.Url, nil
}

func (c *StorageClassController) syncStorageClass(ctx context.Context, syncContext factory.SyncContext, datastoreURL string) error {
	scString := string(c.manifest)
	pairs := []string{
		"${DATASTORE_URL}", datastoreURL,
	}

	policyReplacer := strings.NewReplacer(pairs...)
	scString = policyReplacer.Replace(scString)

	sc := resourceread.ReadStorageClassV1OrDie([]byte(scString))
	_, _, err := resourceapply.ApplyStorageClass(c.kubeClient.StorageV1(), syncContext.Recorder(), sc)
	return err
}

func (c *StorageClassController) getDefaultDatastore(ctx context.Context, conn *vSphereConnection) (*mo.Datastore, error) {
	finder := find.NewFinder(conn.client.Client, false)
	dcName := conn.config.Workspace.Datacenter
	dsName := conn.config.Workspace.DefaultDatastore
	dc, err := finder.Datacenter(ctx, dcName)
	if err != nil {
		return nil, fmt.Errorf("failed to access datacenter %s: %s", dcName, err)
	}

	finder = find.NewFinder(conn.client.Client, false)
	finder.SetDatacenter(dc)
	ds, err := finder.Datastore(ctx, dsName)
	if err != nil {
		return nil, fmt.Errorf("failed to access datastore %s: %s", dsName, err)
	}

	var dsMo mo.Datastore
	pc := property.DefaultCollector(dc.Client())
	properties := []string{DatastoreInfoProperty, SummaryProperty}
	err = pc.RetrieveOne(ctx, ds.Reference(), properties, &dsMo)
	if err != nil {
		return nil, fmt.Errorf("error getting properties of datastore %s: %v", dsName, err)
	}
	return &dsMo, nil
}
