package storageclasscontroller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

const (
	secretName = "vmware-vsphere-cloud-credentials"
	apiTimeout = 10 * time.Minute

	categoryNameTemplate = "openshift-%s"
	policyNameTemplate   = "openshift-storage-policy-%s"
	vim25Prefix          = "urn:vim25:"

	create_tag_api      = "create_tag"
	update_tag_api      = "update_tag"
	attach_tag_api      = "attach_tag"
	create_category_api = "create_category"
	update_category_api = "update_category"
	create_profile_api  = "create_profile"
)

var associatedTypesRaw = []string{"StoragePod", "Datastore", "ResourcePool", "VirtualMachine", "Folder"}

type vCenterInterface interface {
	GetDefaultDatastore(ctx context.Context, infra *v1.Infrastructure) (*mo.Datastore, error)
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
	policyCreated        bool
	// mainly used for verifying test status
	// Keep track of mutable API calls being made
	apiTestInfo map[string]int
}

var _ vCenterInterface = &storagePolicyAPI{}

func NewStoragePolicyAPI(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure) vCenterInterface {
	// TODO: Need to evaluate how to handle new storage policy in multi vcenter env
	storagePolicyAPIClient := &storagePolicyAPI{
		vcenterApiConnection: connection,
		infra:                infra,
		categoryName:         fmt.Sprintf(categoryNameTemplate, infra.Status.InfrastructureName),
		policyName:           fmt.Sprintf(policyNameTemplate, infra.Status.InfrastructureName),
		tagName:              infra.Status.InfrastructureName,
		apiTestInfo:          map[string]int{},
	}
	return storagePolicyAPIClient
}

func (v *storagePolicyAPI) GetDefaultDatastore(ctx context.Context, infra *v1.Infrastructure) (*mo.Datastore, error) {
	vmClient := v.vcenterApiConnection.Client
	config := v.vcenterApiConnection.Config
	finder := find.NewFinder(vmClient.Client, false)

	// Following pattern of older generated ini config, default datastore is from FD[0], else if no FDs are define, we'll
	// grab from deprecated fields
	var dcName, dsName string
	if infra.Spec.PlatformSpec.VSphere != nil && len(infra.Spec.PlatformSpec.VSphere.FailureDomains) > 0 {
		// Due to first FD potentially not matching first vCenter in infra section, lets find correct one.
		for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
			if fd.Server == v.vcenterApiConnection.Hostname {
				dcName = fd.Topology.Datacenter
				dsName = fd.Topology.Datastore
				break
			}
		}
	} else {
		if config != nil {
			dcName = config.GetWorkspaceDatacenter()
			dsName = config.GetDefaultDatastore()
		} else {
			return nil, fmt.Errorf("unable to determine default datastore from current config")
		}
	}
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

	var aggregatedErrors []error

	// Failure domains go across vCenters.  So really we want to only do failure domains that are for this connection's
	// vCenter.
	for _, failureDomain := range failureDomains {
		klog.V(4).Infof("Is failure domain %v part of this connection (%v): %v", failureDomain.Name, v.vcenterApiConnection.Hostname, failureDomain.Server == v.vcenterApiConnection.Hostname)
		if failureDomain.Server == v.vcenterApiConnection.Hostname {
			klog.V(4).Infof("Attempting to attach tag for failure domain %v", failureDomain.Name)
			dataCenter := failureDomain.Topology.Datacenter
			dataStore := failureDomain.Topology.Datastore
			err := v.attachTags(ctx, dataCenter, dataStore)
			if err != nil {
				aggregatedErrors = append(aggregatedErrors, err)
			}
		}
	}

	if len(aggregatedErrors) > 0 {
		return v.policyName, errors.NewAggregate(aggregatedErrors)
	}

	// If we already created storage policy, we should return it
	if v.policyCreated {
		return v.policyName, nil
	}

	err = v.createStorageProfile(ctx)
	if err != nil {
		return v.policyName, fmt.Errorf("error create storage policy profile %s: %v", v.policyName, err)
	}

	return v.policyName, nil
}

func (v *storagePolicyAPI) attachTags(ctx context.Context, dcName, dsName string) error {
	ds, err := v.getDatastore(ctx, dcName, dsName)
	if err != nil {
		return fmt.Errorf("unable to fetch datastore %s: %v", dsName, err)
	}

	// skip creation of tags on a datastore if it already has been tagged.
	if v.policyCreated && v.checkForTagOnDatastore(ctx, ds) {
		return nil
	}

	err = v.createOrUpdateTag(ctx, ds)
	if err != nil {
		return fmt.Errorf("error tagging datastore %s with %s: %v", dsName, v.tagName, err)
	}
	return nil
}

func (v *storagePolicyAPI) createStoragePolicy(ctx context.Context) (string, error) {
	klog.V(1).Info("Checking for policy")
	found, err := v.checkForExistingPolicy(ctx)
	if err != nil {
		return v.policyName, fmt.Errorf("error finding existing policy: %v", err)
	}

	if found {
		klog.V(1).Info("Found policy")
		v.policyCreated = true
	}

	vSphereInfraConfig := v.infra.Spec.PlatformSpec.VSphere
	if vSphereInfraConfig != nil && len(vSphereInfraConfig.FailureDomains) > 0 {
		klog.V(4).Info("Creating zonal policy")
		return v.createZonalStoragePolicy(ctx)
	}

	// Since we create zonal storage policy when in multi vcenter or single vcenter w/ zones, the below is
	// only for case where cluster is upgrade from a version that used a non FailureDomain config.
	dsName := v.vcenterApiConnection.Config.LegacyConfig.Workspace.DefaultDatastore
	dcName := v.vcenterApiConnection.Config.LegacyConfig.Workspace.Datacenter

	err = v.attachTags(ctx, dcName, dsName)
	if err != nil {
		return v.policyName, err
	}

	if !v.policyCreated {
		err = v.createStorageProfile(ctx)
		if err != nil {
			return v.policyName, fmt.Errorf("error create storage policy profile %s: %v", v.policyName, err)
		}
	}

	return v.policyName, nil
}

func (v *storagePolicyAPI) checkForTagOnDatastore(ctx context.Context, dsMo *mo.Datastore) bool {
	tagManager := tags.NewManager(v.vcenterApiConnection.RestClient)
	attachedTags, err := tagManager.GetAttachedTagsOnObjects(ctx, []mo.Reference{dsMo.Reference()})

	if err != nil {
		klog.Errorf("error fetching tags: %v", err)
		return false
	}

	for _, tagObject := range attachedTags {
		tagObjectArray := tagObject.Tags
		for _, tag := range tagObjectArray {
			if tag.Name == v.tagName {
				return true
			}
		}
	}
	return false
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
		v.apiTestInfo[create_category_api]++
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
		v.apiTestInfo[update_category_api]++
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
		v.apiTestInfo[create_tag_api]++
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
		v.apiTestInfo[update_tag_api]++
		err := tagManager.UpdateTag(ctx, tag)
		if err != nil {
			return fmt.Errorf("error updating tag %s: %v", v.tagName, err)
		}
		klog.V(2).Infof("Updated tag %s", v.tagName)
	}

	dsName := ds.Summary.Name
	v.apiTestInfo[attach_tag_api]++
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
	policySpec.Category = "REQUIREMENT"
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

	v.apiTestInfo[create_profile_api]++
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

func (v *storagePolicyAPI) deleteStoragePolicy(ctx context.Context) error {
	rtype := types.PbmProfileResourceType{
		ResourceType: string(types.PbmProfileResourceTypeEnumSTORAGE),
	}

	category := types.PbmProfileCategoryEnumREQUIREMENT

	pbmClient, err := pbm.NewClient(ctx, v.vcenterApiConnection.Client.Client)
	if err != nil {
		return err
	}

	ids, err := pbmClient.QueryProfile(ctx, rtype, string(category))
	if err != nil {
		return err
	}

	profiles, err := pbmClient.RetrieveContent(ctx, ids)
	if err != nil {
		return err
	}

	var foundProfile types.PbmProfileId

	for _, p := range profiles {
		if p.GetPbmProfile().Name == v.policyName {
			foundProfile = p.GetPbmProfile().ProfileId
		}
	}
	_, err = pbmClient.DeleteProfile(ctx, []types.PbmProfileId{foundProfile})
	if err != nil {
		return fmt.Errorf("error deleting profile: %v", err)
	}
	return nil

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
	associatedTypeFinalList := finalAssociatedTypes.List()
	sort.Strings(associatedTypeFinalList)
	return associatedTypeFinalList
}

func appendPrefix(associableTypes []string) []string {
	var appendedTypes []string
	for _, associableType := range associableTypes {
		if strings.HasPrefix(associableType, vim25Prefix) {
			appendedTypes = append(appendedTypes, associableType)
		} else {
			appendedTypes = append(appendedTypes, vim25Prefix+associableType)
		}
	}
	// We are sorting this array because some part of vSphere that applies these tags
	// is confused by ordering and considers same associated types in different order as
	// different associated type.
	sort.Strings(appendedTypes)
	return appendedTypes
}
