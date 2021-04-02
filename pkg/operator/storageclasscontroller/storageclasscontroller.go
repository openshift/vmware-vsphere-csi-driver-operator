package storageclasscontroller

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
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
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/pbm"
	"github.com/vmware/govmomi/pbm/types"
	"github.com/vmware/govmomi/vapi/rest"
	"github.com/vmware/govmomi/vapi/tags"
	vim "github.com/vmware/govmomi/vim25/types"
)

const (
	infraGlobalName      = "cluster"
	cloudConfigNamespace = "openshift-config"
	categoryName         = "container-orchestrator"
	associatedType       = "Datastore"
	tagName              = "openshift"
	policyName           = "openshift-storage-policy"
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
		recorder:        rc,
	}
	return factory.New().WithInformers(
		configMapInformer.Informer(),
		secretInformer.Informer(),
		infraInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		time.Minute, // TODO: figure out what's the idead resync time for this controller
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
	err = c.syncStoragePolicy(ctx)
	if err != nil {
		klog.Errorf("error syncing storage policy: %v", err)
		return err
	}
	err = c.syncStorageClass(ctx)
	if err != nil {
		klog.Errorf("error syncing storage class: %v", err)
		return err
	}
	if _, _, err := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(controllerAvailableCondition),
	); err != nil {
		return err
	}
	return nil
}

func (c *StorageClassController) syncStoragePolicy(ctx context.Context) error {
	infra, err := c.infraLister.Get(infraGlobalName)
	if err != nil {
		return err
	}

	cloudConfig := infra.Spec.CloudConfig
	cloudConfigMap, err := c.configMapLister.ConfigMaps(cloudConfigNamespace).Get(cloudConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to get cloud config: %w", err)
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

	username, password, err := c.getCredentials(cfg)
	if err != nil {
		klog.Errorf("fjb: getCredentials: %v", err)
		return fmt.Errorf("failed to get credentials: %w", err)
	}

	client, err := c.newClient(ctx, cfg, username, password)
	if err != nil {
		klog.Errorf("fjb: newClient: %v", err)
		return fmt.Errorf("failed to create client: %w", err)
	}

	conn := &vSphereConnection{
		client: client,
		config: cfg,
	}
	ds, err := c.getDefaultDatastore(ctx, conn)
	if err != nil {
		klog.Errorf("error getting default datastore: %v", err)
		return fmt.Errorf("error getting default datastore: %v", err)
	}

	// We need to explicitly create a restclient for managing tags and categories
	// We also need to authenticate with the restClient
	restClient := rest.NewClient(client.Client)
	userInfo := url.UserPassword(username, password)
	err = restClient.Login(ctx, userInfo)
	if err != nil {
		msg := fmt.Sprintf("error logging into vcenter: %v", err)
		klog.Error(msg)
		return fmt.Errorf(msg)
	}

	// create tag manager for managing tags
	tagManager := tags.NewManager(restClient)
	conn.tagManager = tagManager

	tag, err := c.createOrUpdateTag(ctx, conn)
	if err != nil {
		klog.Errorf("error creating or updating tag: %v", err)
		return err
	}
	err = c.applyTag(ctx, conn, tag, ds)
	if err != nil {
		klog.Errorf("error applying tag to datastore: %v", err)
		return err
	}

	err = c.createProfile(ctx, conn)
	if err != nil {
		msg := fmt.Sprintf("error creating storage policy: %v", err)
		klog.Errorf(msg)
		return fmt.Errorf(msg)
	}
	return nil
}

func (c *StorageClassController) syncStorageClass(ctx context.Context) error {
	scString := string(c.manifest)
	pairs := []string{
		"${STORAGE_POLICY_NAME}", policyName,
	}

	policyReplacer := strings.NewReplacer(pairs...)
	scString = policyReplacer.Replace(scString)

	sc := resourceread.ReadStorageClassV1OrDie([]byte(scString))
	_, _, err := resourceapply.ApplyStorageClass(c.kubeClient.StorageV1(), c.recorder, sc)
	return err
}

func (c *StorageClassController) createOrUpdateTag(ctx context.Context, conn *vSphereConnection) (*tags.Tag, error) {
	tagManager := conn.tagManager
	category, err := tagManager.GetCategory(ctx, categoryName)
	if err != nil && !notFoundError(err) {
		return nil, fmt.Errorf("error finding category: %+v", err)
	}
	if category == nil || category.ID == "" {
		category = &tags.Category{
			Name:            categoryName,
			Description:     "Container Orchestrator that uses this datastore",
			AssociableTypes: []string{associatedType},
			Cardinality:     "SINGLE",
		}
		catId, err := tagManager.CreateCategory(ctx, category)
		if err != nil {
			return nil, fmt.Errorf("error creating category %s: %v", categoryName, err)
		}
		category.ID = catId
	}
	tag, err := tagManager.GetTag(ctx, tagName)
	if err != nil && !notFoundError(err) {
		return nil, fmt.Errorf("error finding tag %s: %v", tagName, err)
	}
	if tag == nil || tag.ID == "" {
		tag = &tags.Tag{
			Name:        tagName,
			Description: "Datastore is used by openshift",
			CategoryID:  category.ID,
		}
		tagID, err := tagManager.CreateTag(ctx, tag)
		if err != nil {
			return nil, fmt.Errorf("error creating tag %s: %v", tagName, err)
		}
		tag.ID = tagID
	} else if tag.CategoryID != category.ID {
		tag = &tags.Tag{
			Name:        tagName,
			Description: "Datastore is used by openshift",
			CategoryID:  category.ID,
			ID:          tag.ID,
		}
		err := tagManager.UpdateTag(ctx, tag)
		if err != nil {
			return nil, fmt.Errorf("error updating tag %s: %v", tagName, err)
		}
	}
	return tag, nil
}

func (c *StorageClassController) createProfile(ctx context.Context, conn *vSphereConnection) error {
	rtype := types.PbmProfileResourceType{
		ResourceType: string(types.PbmProfileResourceTypeEnumSTORAGE),
	}

	category := types.PbmProfileCategoryEnumREQUIREMENT

	pbmClient, err := pbm.NewClient(ctx, conn.client.Client)
	if err != nil {
		msg := fmt.Sprintf("error creating pbm client: %v", err)
		klog.Error(msg)
		return fmt.Errorf(msg)
	}

	ids, err := pbmClient.QueryProfile(ctx, rtype, string(category))
	if err != nil {
		msg := fmt.Sprintf("error querying profiles: %v", err)
		klog.Errorf(msg)
		return fmt.Errorf(msg)
	}

	profiles, err := pbmClient.RetrieveContent(ctx, ids)
	if err != nil {
		msg := fmt.Sprintf("error fetching policy profiles: %v", err)
		klog.Errorf(msg)
		return fmt.Errorf(msg)
	}

	for _, p := range profiles {
		if p.GetPbmProfile().Name == policyName {
			klog.Infof("Found existing profile with same name: %s", p.GetPbmProfile().Name)
			return nil
		}
	}

	var policySpec types.PbmCapabilityProfileCreateSpec
	policySpec.Name = policyName
	policySpec.ResourceType.ResourceType = string(types.PbmProfileResourceTypeEnumSTORAGE)

	policyID := fmt.Sprintf("com.vmware.storage.tag.%s.property", categoryName)
	instance := types.PbmCapabilityInstance{
		Id: types.PbmCapabilityMetadataUniqueId{
			Namespace: "http://www.vmware.com/storage/tag",
			Id:        categoryName,
		},
		Constraint: []types.PbmCapabilityConstraintInstance{{
			PropertyInstance: []types.PbmCapabilityPropertyInstance{{
				Id: policyID,
				Value: types.PbmCapabilityDiscreteSet{
					Values: []vim.AnyType{tagName},
				},
			}},
		}},
	}

	policySpec.Constraints = &types.PbmCapabilitySubProfileConstraints{
		SubProfiles: []types.PbmCapabilitySubProfile{{
			Name:       "Tag based placement",
			Capability: []types.PbmCapabilityInstance{instance},
		}},
	}

	pid, err := pbmClient.CreateProfile(ctx, policySpec)
	if err != nil {
		msg := fmt.Sprintf("error creating profile: %v", err)
		klog.Errorf(msg)
		return fmt.Errorf(msg)
	}
	klog.Infof("Successfully created profile %s", pid.UniqueId)
	return nil
}

func (c *StorageClassController) applyTag(ctx context.Context, conn *vSphereConnection, tag *tags.Tag, ds *object.Datastore) error {
	dsName := conn.config.Workspace.DefaultDatastore
	err := conn.tagManager.AttachTag(ctx, tag.ID, ds)
	if err != nil {
		klog.Errorf("error attaching tag %s to datastore %s: %v", tagName, dsName, err)
		return err
	}
	return nil
}

func (c *StorageClassController) getDefaultDatastore(ctx context.Context, conn *vSphereConnection) (*object.Datastore, error) {
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
	return ds, nil
}

func notFoundError(err error) bool {
	errorString := err.Error()
	r := regexp.MustCompile("404")
	return r.MatchString(errorString)
}
