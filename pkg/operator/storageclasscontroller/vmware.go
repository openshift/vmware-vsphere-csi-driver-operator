package storageclasscontroller

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"

	v1 "github.com/openshift/api/config/v1"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/pbm"
	"github.com/vmware/govmomi/pbm/types"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/mo"
	vim "github.com/vmware/govmomi/vim25/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

const (
	secretName = "vmware-vsphere-cloud-credentials"
	apiTimeout = 10 * time.Minute

	categoryNameTemplate = "openshift-%s"
	policyNameTemplate   = "openshift-storage-policy-%s"
	vim25Prefix          = "urn:vim25:"
)

var associatedTypesRaw = []string{"StoragePod", "Datastore", "ResourcePool", "VirtualMachine", "Folder"}

type vCenterInterface interface {
	getDefaultDatastore(ctx context.Context) (*mo.Datastore, error)
	createStoragePolicy(ctx context.Context) (string, error)
	checkForExistingPolicy(ctx context.Context) (bool, error)
	createOrUpdateTag(ctx context.Context, ds *mo.Datastore) error
	createStorageProfile(ctx context.Context) error
}

type storagePolicyAPI struct {
	vcenterApiConnection *vclib.VSphereConnection
	tagManager           *tags.Manager
	infra                *v1.Infrastructure
	policyName           string
	tagName              string
	categoryName         string
}

var _ vCenterInterface = &storagePolicyAPI{}

func newStoragePolicyAPI(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure) vCenterInterface {
	storagePolicyAPIClient := &storagePolicyAPI{
		vcenterApiConnection: connection,
		infra:                infra,
		categoryName:         fmt.Sprintf(categoryNameTemplate, infra.Status.InfrastructureName),
		policyName:           fmt.Sprintf(policyNameTemplate, infra.Status.InfrastructureName),
		tagName:              infra.Status.InfrastructureName,
	}
	return storagePolicyAPIClient
}

func (v *storagePolicyAPI) getDefaultDatastore(ctx context.Context) (*mo.Datastore, error) {
	vmClient := v.vcenterApiConnection.Client
	config := v.vcenterApiConnection.Config
	finder := find.NewFinder(vmClient.Client, false)
	dcName := config.Workspace.Datacenter
	dsName := config.Workspace.DefaultDatastore
	dc, err := finder.Datacenter(ctx, dcName)
	if err != nil {
		return nil, fmt.Errorf("failed to access datacenter %s: %s", dcName, err)
	}

	finder = find.NewFinder(vmClient.Client, false)
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

func (v *storagePolicyAPI) getDatastore(ctx context.Context, dcName string, dsName string) (*mo.Datastore, error) {
	vmClient := v.vcenterApiConnection.Client
	finder := find.NewFinder(vmClient.Client, false)

	dc, err := finder.Datacenter(ctx, dcName)
	if err != nil {
		return nil, fmt.Errorf("failed to access datacenter %s: %s", dcName, err)
	}

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

func (v *storagePolicyAPI) createZonalStoragePolicy(ctx context.Context) (string, error) {
	var err error
	failureDomains := v.infra.Spec.PlatformSpec.VSphere.FailureDomains
	if len(failureDomains) <= 1 {
		return "", fmt.Errorf("cluster has only one failure domain defined")
	}

	for _, failureDomain := range failureDomains {
		dataCenter := failureDomain.Topology.Datacenter
		dataStore := failureDomain.Topology.Datastore
		ds, err := v.getDatastore(ctx, dataCenter, dataStore)
		if err != nil {
			return v.policyName, fmt.Errorf("unable to fetch datastore %s: %v", dataStore, err)
		}
		err = v.createOrUpdateTag(ctx, ds)
		if err != nil {
			return v.policyName, fmt.Errorf("error creating or updating tag %s: %v", v.tagName, err)
		}
	}
	err = v.createStorageProfile(ctx)
	if err != nil {
		return v.policyName, fmt.Errorf("error create storage policy profile %s: %v", v.policyName, err)
	}

	return v.policyName, nil
}

func (v *storagePolicyAPI) createStoragePolicy(ctx context.Context) (string, error) {
	found, err := v.checkForExistingPolicy(ctx)
	if err != nil {
		return v.policyName, fmt.Errorf("error finding existing policy: %v", err)
	}

	if found {
		klog.V(3).Infof("found existing storage policy %s", v.policyName)
		return v.policyName, nil
	}

	vSphereInfraConfig := v.infra.Spec.PlatformSpec.VSphere
	if vSphereInfraConfig != nil && len(vSphereInfraConfig.FailureDomains) > 1 {
		return v.createZonalStoragePolicy(ctx)
	}

	dsName := v.vcenterApiConnection.Config.Workspace.DefaultDatastore
	ds, err := v.getDefaultDatastore(ctx)
	if err != nil {
		return v.policyName, fmt.Errorf("error fetching default datastore %s: %v", dsName, err)
	}
	err = v.createOrUpdateTag(ctx, ds)
	if err != nil {
		return v.policyName, fmt.Errorf("error creating or applying tag %s: %v", v.tagName, err)
	}

	err = v.createStorageProfile(ctx)
	if err != nil {
		return v.policyName, fmt.Errorf("error create storage policy profile %s: %v", v.policyName, err)
	}

	return v.policyName, nil
}

func (v *storagePolicyAPI) createOrUpdateTag(ctx context.Context, ds *mo.Datastore) error {
	// create tag manager for managing tags
	tagManager := tags.NewManager(v.vcenterApiConnection.RestClient)

	category, err := tagManager.GetCategory(ctx, v.categoryName)
	if err != nil && !notFoundError(err) {
		return fmt.Errorf("error finding category: %+v", err)
	}

	associatedTypes := appendPrefix(associatedTypesRaw)
	if category == nil || category.ID == "" {
		klog.Warningf("Unexpected missing category %s - creating it", v.categoryName)
		category = &tags.Category{
			Name:            v.categoryName,
			Description:     "Added by openshift-install do not remove",
			AssociableTypes: associatedTypes,
			Cardinality:     "SINGLE",
		}
		catId, err := tagManager.CreateCategory(ctx, category)
		if err != nil {
			return fmt.Errorf("error creating category %s: %v", v.categoryName, err)
		}
		klog.V(2).Infof("Created category %s", v.categoryName)
		category.ID = catId
	} else {
		existingAssociatedTypes := category.AssociableTypes
		associatedTypes = updateAssociatedTypes(existingAssociatedTypes)
		category.AssociableTypes = associatedTypes
		klog.V(4).Infof("Final categories are: %+v", associatedTypes)
		err := tagManager.UpdateCategory(ctx, category)
		if err != nil {
			return fmt.Errorf("error updating category %s: %v", v.categoryName, err)
		}
		klog.V(2).Infof("Updated category %s with associated types", v.categoryName)
	}

	tag, err := tagManager.GetTag(ctx, v.tagName)
	if err != nil && !notFoundError(err) {
		return fmt.Errorf("error finding tag %s: %v", v.tagName, err)
	}
	if tag == nil || tag.ID == "" {
		klog.Warningf("Unexpected missing tag %s - creating it", v.tagName)
		tag = &tags.Tag{
			Name:        v.tagName,
			Description: "Added by openshift-install do not remove",
			CategoryID:  category.ID,
		}
		tagID, err := tagManager.CreateTag(ctx, tag)
		if err != nil {
			return fmt.Errorf("error creating tag %s: %v", v.tagName, err)
		}
		klog.V(2).Infof("Created tag %s", v.tagName)
		tag.ID = tagID
	} else if tag.CategoryID != category.ID {
		tag = &tags.Tag{
			Name:        v.tagName,
			Description: "Added by openshift-install do not remove",
			CategoryID:  category.ID,
			ID:          tag.ID,
		}
		err := tagManager.UpdateTag(ctx, tag)
		if err != nil {
			return fmt.Errorf("error updating tag %s: %v", v.tagName, err)
		}
		klog.V(2).Infof("Updated tag %s", v.tagName)
	}

	dsName := ds.Name
	err = tagManager.AttachTag(ctx, tag.ID, ds)
	if err != nil {
		klog.Errorf("error attaching tag %s to datastore %s: %v", v.tagName, dsName, err)
		return err
	}
	return nil

}

func (v *storagePolicyAPI) createStorageProfile(ctx context.Context) error {
	pbmClient, err := pbm.NewClient(ctx, v.vcenterApiConnection.Client.Client)
	if err != nil {
		msg := fmt.Sprintf("error creating pbm client: %v", err)
		klog.Error(msg)
		return fmt.Errorf(msg)
	}

	var policySpec types.PbmCapabilityProfileCreateSpec
	policySpec.Name = v.policyName
	policySpec.ResourceType.ResourceType = string(types.PbmProfileResourceTypeEnumSTORAGE)

	policyID := fmt.Sprintf("com.vmware.storage.tag.%s.property", v.categoryName)
	instance := types.PbmCapabilityInstance{
		Id: types.PbmCapabilityMetadataUniqueId{
			Namespace: "http://www.vmware.com/storage/tag",
			Id:        v.categoryName,
		},
		Constraint: []types.PbmCapabilityConstraintInstance{{
			PropertyInstance: []types.PbmCapabilityPropertyInstance{{
				Id: policyID,
				Value: types.PbmCapabilityDiscreteSet{
					Values: []vim.AnyType{v.tagName},
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
	klog.V(2).Infof("Successfully created profile %s", pid.UniqueId)
	return nil
}

func (v *storagePolicyAPI) checkForExistingPolicy(ctx context.Context) (bool, error) {
	rtype := types.PbmProfileResourceType{
		ResourceType: string(types.PbmProfileResourceTypeEnumSTORAGE),
	}

	category := types.PbmProfileCategoryEnumREQUIREMENT

	pbmClient, err := pbm.NewClient(ctx, v.vcenterApiConnection.Client.Client)
	if err != nil {
		msg := fmt.Sprintf("error creating pbm client: %v", err)
		klog.Error(msg)
		return false, fmt.Errorf(msg)
	}

	ids, err := pbmClient.QueryProfile(ctx, rtype, string(category))
	if err != nil {
		msg := fmt.Sprintf("error querying profiles: %v", err)
		klog.Errorf(msg)
		return false, fmt.Errorf(msg)
	}

	profiles, err := pbmClient.RetrieveContent(ctx, ids)
	if err != nil {
		msg := fmt.Sprintf("error fetching policy profiles: %v", err)
		klog.Errorf(msg)
		return false, fmt.Errorf(msg)
	}

	for _, p := range profiles {
		if p.GetPbmProfile().Name == v.policyName {
			klog.V(2).Infof("Found existing profile with same name: %s", p.GetPbmProfile().Name)
			return true, nil
		}
	}
	return false, nil
}

func notFoundError(err error) bool {
	errorString := err.Error()
	r := regexp.MustCompile("404")
	return r.MatchString(errorString)
}

func updateAssociatedTypes(associatedTypes []string) []string {
	incomingTypesSet := sets.NewString(appendPrefix(associatedTypes)...)
	additionTypes := appendPrefix(associatedTypesRaw)
	finalAssociatedTypes := incomingTypesSet.Insert(additionTypes...)
	return finalAssociatedTypes.List()
}

func appendPrefix(associableTypes []string) []string {
	var appendedTypes []string
	for _, associableType := range associableTypes {
		appendedTypes = append(appendedTypes, vim25Prefix+associableType)
	}
	return appendedTypes
}
