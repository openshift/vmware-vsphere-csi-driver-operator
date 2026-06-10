package storageclasscontroller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
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
	GetDefaultDatastore(ctx context.Context) (*mo.Datastore, error)
	createStoragePolicy(ctx context.Context) (string, error)
	checkForExistingPolicy(ctx context.Context) (bool, error)
	createOrUpdateTag(ctx context.Context, ds *mo.Datastore) error
	createStorageProfile(ctx context.Context) error
}

type storagePolicyAPI struct {
	vcenterApiConnection *vclib.VSphereConnection
	connectionManager    *vclib.VSphereConnectionManager
	tagManager           *tags.Manager
	infra                *v1.Infrastructure
	policyName           string
	tagName              string
	categoryName         string
	policyCreated        bool
	// Per-vCenter policy tracking. When using a connection manager,
	// tracks whether the SPBM profile exists on each vCenter.
	policyCreatedPerVC map[string]bool
	// mainly used for verifying test status
	// Keep track of mutable API calls being made
	apiTestInfo map[string]int
}

func (v *storagePolicyAPI) setConnectionManager(connMgr *vclib.VSphereConnectionManager) {
	v.connectionManager = connMgr
	if v.policyCreatedPerVC == nil {
		v.policyCreatedPerVC = make(map[string]bool)
	}
}

var _ vCenterInterface = &storagePolicyAPI{}

func NewStoragePolicyAPI(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure) vCenterInterface {
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

func (v *storagePolicyAPI) GetDefaultDatastore(ctx context.Context) (*mo.Datastore, error) {
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
	failureDomains := v.infra.Spec.PlatformSpec.VSphere.FailureDomains

	var aggregatedErrors []error

	for _, failureDomain := range failureDomains {
		dataCenter := failureDomain.Topology.Datacenter
		dataStore := failureDomain.Topology.Datastore

		// Resolve the correct connection for this failure domain's vCenter
		conn := v.vcenterApiConnection
		if v.connectionManager != nil && failureDomain.Server != "" {
			if fdConn, ok := v.connectionManager.GetConnection(failureDomain.Server); ok {
				conn = fdConn
			} else {
				aggregatedErrors = append(aggregatedErrors, fmt.Errorf("no connection for vCenter %s (failure domain %s)", failureDomain.Server, failureDomain.Name))
				continue
			}
		}

		err := v.attachTagsOnConnection(ctx, conn, dataCenter, dataStore)
		if err != nil {
			aggregatedErrors = append(aggregatedErrors, err)
		}
	}

	if len(aggregatedErrors) > 0 {
		return v.policyName, errors.NewAggregate(aggregatedErrors)
	}

	// Ensure storage profile exists on every vCenter
	if v.connectionManager != nil && v.connectionManager.Len() > 1 {
		for _, host := range v.connectionManager.Hosts() {
			if v.policyCreatedPerVC[host] {
				continue
			}
			conn, ok := v.connectionManager.GetConnection(host)
			if !ok {
				continue
			}
			found, err := v.checkForExistingPolicyOnConnection(ctx, conn)
			if err != nil {
				aggregatedErrors = append(aggregatedErrors, fmt.Errorf("error checking policy on vCenter %s: %v", host, err))
				continue
			}
			if found {
				v.policyCreatedPerVC[host] = true
				continue
			}
			err = v.createStorageProfileOnConnection(ctx, conn)
			if err != nil {
				aggregatedErrors = append(aggregatedErrors, fmt.Errorf("error creating profile on vCenter %s: %v", host, err))
			} else {
				v.policyCreatedPerVC[host] = true
			}
		}
		if len(aggregatedErrors) > 0 {
			return v.policyName, errors.NewAggregate(aggregatedErrors)
		}
		return v.policyName, nil
	}

	// Single vCenter path (backward compatible)
	if v.policyCreated {
		return v.policyName, nil
	}

	err := v.createStorageProfile(ctx)
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

// attachTagsOnConnection attaches tags to a datastore using a specific vCenter connection.
func (v *storagePolicyAPI) attachTagsOnConnection(ctx context.Context, conn *vclib.VSphereConnection, dcName, dsName string) error {
	ds, err := v.getDatastoreOnConnection(ctx, conn, dcName, dsName)
	if err != nil {
		return fmt.Errorf("unable to fetch datastore %s: %v", dsName, err)
	}

	// skip creation of tags on a datastore if it already has been tagged.
	policyExists := v.policyCreated
	if v.connectionManager != nil {
		policyExists = v.policyCreatedPerVC[conn.Hostname]
	}
	if policyExists && v.checkForTagOnDatastoreOnConnection(ctx, conn, ds) {
		return nil
	}

	err = v.createOrUpdateTagOnConnection(ctx, conn, ds)
	if err != nil {
		return fmt.Errorf("error tagging datastore %s with %s: %v", dsName, v.tagName, err)
	}
	return nil
}

// getDatastoreOnConnection retrieves datastore properties using a specific vCenter connection.
func (v *storagePolicyAPI) getDatastoreOnConnection(ctx context.Context, conn *vclib.VSphereConnection, dcName string, dsName string) (*mo.Datastore, error) {
	vmClient := conn.Client
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
	pc := property.DefaultCollector(vmClient.Client)
	properties := []string{DatastoreInfoProperty, SummaryProperty}
	err = pc.RetrieveOne(ctx, ds.Reference(), properties, &dsMo)
	if err != nil {
		return nil, fmt.Errorf("error getting properties of datastore %s: %v", dsName, err)
	}
	return &dsMo, nil
}

// checkForTagOnDatastoreOnConnection checks if a datastore has the cluster tag using a specific connection.
func (v *storagePolicyAPI) checkForTagOnDatastoreOnConnection(ctx context.Context, conn *vclib.VSphereConnection, dsMo *mo.Datastore) bool {
	tagManager := tags.NewManager(conn.RestClient)
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

// createOrUpdateTagOnConnection creates or updates the tag on a datastore using a specific connection.
func (v *storagePolicyAPI) createOrUpdateTagOnConnection(ctx context.Context, conn *vclib.VSphereConnection, ds *mo.Datastore) error {
	tagManager := tags.NewManager(conn.RestClient)

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
		klog.V(2).Infof("Created category %s on vCenter %s", v.categoryName, conn.Hostname)
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
		klog.V(2).Infof("Updated category %s with associated types on vCenter %s", v.categoryName, conn.Hostname)
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
		klog.V(2).Infof("Created tag %s on vCenter %s", v.tagName, conn.Hostname)
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
		klog.V(2).Infof("Updated tag %s on vCenter %s", v.tagName, conn.Hostname)
	}

	dsName := ds.Summary.Name
	v.apiTestInfo[attach_tag_api]++
	err = tagManager.AttachTag(ctx, tag.ID, ds)
	if err != nil {
		klog.Errorf("error attaching tag %s to datastore %s on vCenter %s: %v", v.tagName, dsName, conn.Hostname, err)
		return err
	}
	return nil
}

// checkForExistingPolicyOnConnection checks for an existing SPBM policy on a specific vCenter.
func (v *storagePolicyAPI) checkForExistingPolicyOnConnection(ctx context.Context, conn *vclib.VSphereConnection) (bool, error) {
	rtype := types.PbmProfileResourceType{
		ResourceType: string(types.PbmProfileResourceTypeEnumSTORAGE),
	}

	category := types.PbmProfileCategoryEnumREQUIREMENT

	pbmClient, err := pbm.NewClient(ctx, conn.Client.Client)
	if err != nil {
		return false, fmt.Errorf("error creating pbm client on vCenter %s: %v", conn.Hostname, err)
	}

	ids, err := pbmClient.QueryProfile(ctx, rtype, string(category))
	if err != nil {
		return false, fmt.Errorf("error querying profiles on vCenter %s: %v", conn.Hostname, err)
	}

	profiles, err := pbmClient.RetrieveContent(ctx, ids)
	if err != nil {
		return false, fmt.Errorf("error fetching policy profiles on vCenter %s: %v", conn.Hostname, err)
	}

	for _, p := range profiles {
		if p.GetPbmProfile().Name == v.policyName {
			klog.V(2).Infof("Found existing profile with same name: %s on vCenter %s", p.GetPbmProfile().Name, conn.Hostname)
			return true, nil
		}
	}
	return false, nil
}

// createStorageProfileOnConnection creates an SPBM profile on a specific vCenter.
func (v *storagePolicyAPI) createStorageProfileOnConnection(ctx context.Context, conn *vclib.VSphereConnection) error {
	pbmClient, err := pbm.NewClient(ctx, conn.Client.Client)
	if err != nil {
		return fmt.Errorf("error creating pbm client on vCenter %s: %v", conn.Hostname, err)
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
		return fmt.Errorf("error creating profile on vCenter %s: %v", conn.Hostname, err)
	}
	klog.V(2).Infof("Successfully created profile %s on vCenter %s", pid.UniqueId, conn.Hostname)
	return nil
}

func (v *storagePolicyAPI) createStoragePolicy(ctx context.Context) (string, error) {
	found, err := v.checkForExistingPolicy(ctx)
	if err != nil {
		return v.policyName, fmt.Errorf("error finding existing policy: %v", err)
	}

	if found {
		v.policyCreated = true
	}

	// Run orphan cleanup regardless of FD count — this handles the
	// transition from zonal (2+ FDs) to non-zonal (0 FDs) as well.
	if found {
		orphans, err := v.findOrphanedTags(ctx)
		if err != nil {
			klog.Warningf("Error detecting orphaned tags: %v", err)
		} else if len(orphans) > 0 {
			klog.V(2).Infof("Found %d orphaned tags, running cleanup", len(orphans))
			v.detachOrphanTags(ctx, orphans)
		}
	}

	vSphereInfraConfig := v.infra.Spec.PlatformSpec.VSphere
	if vSphereInfraConfig != nil && len(vSphereInfraConfig.FailureDomains) > 0 {
		return v.createZonalStoragePolicy(ctx)
	}

	dsName := v.vcenterApiConnection.Config.Workspace.DefaultDatastore
	dcName := v.vcenterApiConnection.Config.Workspace.Datacenter

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

// orphanedDatastore identifies a datastore that has the cluster tag but is not
// in the current failure domain list.
type orphanedDatastore struct {
	// VCenter is the hostname of the vCenter (always workspace vCenter in Phase 3A).
	VCenter string
	// Datacenter is the datacenter containing this datastore.
	Datacenter string
	// Datastore is the datastore name.
	Datastore string
	// Reference is the managed object reference for this datastore.
	Reference vim.ManagedObjectReference
	// HasBoundPVs indicates whether CNS volumes backed by this datastore
	// have matching PVs in the cluster.
	HasBoundPVs bool
}

// findOrphanedTags queries vCenter for all datastores tagged with the cluster tag,
// then compares against the current failure domain list. Any tagged datastore not in
// the current failure domains is an orphan. The comparison key is the full
// (datacenter, datastore) tuple to prevent false matches when datastore names
// collide across datacenters.
func (v *storagePolicyAPI) findOrphanedTags(ctx context.Context) ([]orphanedDatastore, error) {
	conn := v.vcenterApiConnection
	if conn == nil || conn.RestClient == nil {
		return nil, fmt.Errorf("no vCenter connection available for orphan detection")
	}

	tagManager := tags.NewManager(conn.RestClient)

	// Get the tag by name
	tag, err := tagManager.GetTag(ctx, v.tagName)
	if err != nil {
		if notFoundError(err) {
			klog.V(4).Infof("Tag %s not found, no orphans possible", v.tagName)
			return nil, nil
		}
		return nil, fmt.Errorf("error finding tag %s: %v", v.tagName, err)
	}

	if tag == nil || tag.ID == "" {
		return nil, nil
	}

	// Get all objects with this tag
	attachedObjects, err := tagManager.GetAttachedObjectsOnTags(ctx, []string{tag.ID})
	if err != nil {
		return nil, fmt.Errorf("error getting attached objects for tag %s: %v", v.tagName, err)
	}

	// Build set of current failure domain (datacenter, datastore) tuples
	currentFDs := sets.NewString()
	if v.infra.Spec.PlatformSpec.VSphere != nil {
		for _, fd := range v.infra.Spec.PlatformSpec.VSphere.FailureDomains {
			key := fd.Topology.Datacenter + "/" + fd.Topology.Datastore
			currentFDs.Insert(key)
		}
	}

	// Also include the workspace default datastore if no failure domains
	if currentFDs.Len() == 0 && conn.Config != nil {
		workspaceDC := conn.Config.Workspace.Datacenter
		workspaceDS := conn.Config.Workspace.DefaultDatastore
		if workspaceDC != "" && workspaceDS != "" {
			currentFDs.Insert(workspaceDC + "/" + workspaceDS)
		}
	}

	var orphans []orphanedDatastore

	for _, tagResult := range attachedObjects {
		for _, objRef := range tagResult.ObjectIDs {
			// Only interested in Datastore objects
			if objRef.Reference().Type != "Datastore" {
				continue
			}

			// Resolve the datastore's datacenter and name
			dcName, dsName, err := v.resolveDatastoreIdentity(ctx, conn, objRef.Reference())
			if err != nil {
				klog.Warningf("Could not resolve identity for tagged object %s: %v", objRef.Reference(), err)
				continue
			}

			key := dcName + "/" + dsName
			if !currentFDs.Has(key) {
				orphans = append(orphans, orphanedDatastore{
					VCenter:    conn.Hostname,
					Datacenter: dcName,
					Datastore:  dsName,
					Reference:  objRef.Reference(),
				})
			}
		}
	}

	return orphans, nil
}

// resolveDatastoreIdentity determines the datacenter and name for a datastore by its MOR.
func (v *storagePolicyAPI) resolveDatastoreIdentity(ctx context.Context, conn *vclib.VSphereConnection, ref vim.ManagedObjectReference) (datacenterName, datastoreName string, err error) {
	pc := property.DefaultCollector(conn.Client.Client)

	var dsMo mo.Datastore
	err = pc.RetrieveOne(ctx, ref, []string{SummaryProperty}, &dsMo)
	if err != nil {
		return "", "", fmt.Errorf("failed to retrieve datastore properties: %v", err)
	}
	datastoreName = dsMo.Summary.Name

	// Find which datacenter contains this datastore by searching all datacenters
	finder := find.NewFinder(conn.Client.Client, false)
	datacenters, err := finder.DatacenterList(ctx, "*")
	if err != nil {
		return "", "", fmt.Errorf("failed to list datacenters: %v", err)
	}

	for _, dc := range datacenters {
		dcFinder := find.NewFinder(conn.Client.Client, false)
		dcFinder.SetDatacenter(dc)
		dsList, err := dcFinder.DatastoreList(ctx, "*")
		if err != nil {
			continue
		}
		for _, ds := range dsList {
			if ds.Reference() == ref {
				return dc.Name(), datastoreName, nil
			}
		}
	}

	return "", datastoreName, fmt.Errorf("could not find datacenter for datastore %s", datastoreName)
}

// detachOrphanTags removes tags from orphaned datastores that don't have bound PVs.
// For datastores with bound PVs, it skips the detach and logs a warning.
// Returns the list of orphans that could not be cleaned up (PV-blocked).
func (v *storagePolicyAPI) detachOrphanTags(ctx context.Context, orphans []orphanedDatastore) []orphanedDatastore {
	if len(orphans) == 0 {
		return nil
	}

	conn := v.vcenterApiConnection
	if conn == nil || conn.RestClient == nil {
		klog.Errorf("No vCenter connection available for orphan tag detach")
		return orphans
	}

	// Check for force-cleanup annotation on ClusterCSIDriver
	forceCleanup := false
	if v.infra != nil {
		// The annotation is checked on the Infrastructure resource for simplicity
		if v.infra.Annotations != nil {
			if val, ok := v.infra.Annotations["csi.vsphere.vmware.com/force-orphan-cleanup"]; ok && val == "true" {
				klog.V(2).Infof("Force orphan cleanup annotation found, skipping PV safety check")
				forceCleanup = true
			}
		}
	}

	tagManager := tags.NewManager(conn.RestClient)

	tag, err := tagManager.GetTag(ctx, v.tagName)
	if err != nil || tag == nil || tag.ID == "" {
		klog.Errorf("Could not find tag %s for orphan detach: %v", v.tagName, err)
		return orphans
	}

	utils.OrphanTagsDetectedTotal.Add(float64(len(orphans)))

	var pvBlocked []orphanedDatastore

	for i := range orphans {
		orphan := &orphans[i]

		if orphan.HasBoundPVs && !forceCleanup {
			klog.Warningf("Skipping tag detach from datastore %s in datacenter %s: has bound PVs",
				orphan.Datastore, orphan.Datacenter)
			utils.TagOperationsTotal.WithLabelValues("skip", "pv_blocked").Inc()
			pvBlocked = append(pvBlocked, *orphan)
			continue
		}

		ref := orphan.Reference
		err := tagManager.DetachTag(ctx, tag.ID, ref)
		if err != nil {
			klog.Errorf("Failed to detach tag %s from datastore %s in datacenter %s: %v",
				v.tagName, orphan.Datastore, orphan.Datacenter, err)
			utils.TagOperationsTotal.WithLabelValues("detach", "error").Inc()
			pvBlocked = append(pvBlocked, *orphan)
			continue
		}

		klog.V(2).Infof("Successfully detached tag %s from orphaned datastore %s in datacenter %s",
			v.tagName, orphan.Datastore, orphan.Datacenter)
		utils.TagOperationsTotal.WithLabelValues("detach", "success").Inc()
	}

	return pvBlocked
}
