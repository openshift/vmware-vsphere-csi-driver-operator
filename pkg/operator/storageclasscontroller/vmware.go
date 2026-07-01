package storageclasscontroller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"

	v1 "github.com/openshift/api/config/v1"
	cnstypes "github.com/vmware/govmomi/cns/types"
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
	failureDomains       []*v1.VSpherePlatformFailureDomainSpec
	policyName           string
	tagName              string
	categoryName         string
	policyCreated        bool
	day2Enabled          bool
	forceCleanup         bool
	recorder             events.Recorder
	unresolvedOrphans    []orphanedDatastore
	// mainly used for verifying test status
	// Keep track of mutable API calls being made
	apiTestInfo map[string]int
}

var _ vCenterInterface = &storagePolicyAPI{}

func NewStoragePolicyAPI(ctx context.Context, connection *vclib.VSphereConnection, infra *v1.Infrastructure, day2Enabled, forceCleanup bool, recorder events.Recorder) vCenterInterface {
	var fds []*v1.VSpherePlatformFailureDomainSpec

	// Get Failure domains to use for this storage policy based on the connection hostname (vCenter)
	if infra.Spec.PlatformSpec.VSphere != nil {
		server := connection.Hostname
		for _, fd := range infra.Spec.PlatformSpec.VSphere.FailureDomains {
			if fd.Server == server {
				fds = append(fds, &fd)
			}
		}
	}

	storagePolicyAPIClient := &storagePolicyAPI{
		vcenterApiConnection: connection,
		infra:                infra,
		failureDomains:       fds,
		categoryName:         fmt.Sprintf(categoryNameTemplate, infra.Status.InfrastructureName),
		policyName:           fmt.Sprintf(policyNameTemplate, infra.Status.InfrastructureName),
		tagName:              infra.Status.InfrastructureName,
		day2Enabled:          day2Enabled,
		forceCleanup:        forceCleanup,
		recorder:            recorder,
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
	if len(v.failureDomains) > 0 {
		dcName = v.failureDomains[0].Topology.Datacenter
		dsName = v.failureDomains[0].Topology.Datastore
	} else {
		if config == nil {
			return nil, fmt.Errorf("unable to determine default datastore from current config")
		}
		var err error
		dcName, err = config.GetWorkspaceDatacenter()
		if err != nil {
			return nil, fmt.Errorf("unable to determine default datacenter from current config: %v", err)
		}
		dsName, err = config.GetDefaultDatastore()
		if err != nil {
			return nil, fmt.Errorf("unable to determine default datastore from current config: %v", err)
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
	var aggregatedErrors []error

	// Process all failure domains for this ctx vCenter
	for _, failureDomain := range v.failureDomains {
		klog.V(4).Infof("Attempting to attach tag for failure domain %v", failureDomain.Name)
		dataCenter := failureDomain.Topology.Datacenter
		dataStore := failureDomain.Topology.Datastore
		err := v.attachTags(ctx, dataCenter, dataStore)
		if err != nil {
			aggregatedErrors = append(aggregatedErrors, err)
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
		utils.TagOperationsTotal.WithLabelValues("attach", "error").Inc()
		return fmt.Errorf("error tagging datastore %s with %s: %v", dsName, v.tagName, err)
	}
	utils.TagOperationsTotal.WithLabelValues("attach", "success").Inc()
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

	// Day-2 lifecycle: orphan cleanup and SPBM profile deletion.
	// Only runs when the VSphereMultiVCenterDay2 feature gate is enabled.
	var unresolved []orphanedDatastore
	orphanDetectionFailed := false
	if v.day2Enabled {
		orphans, orphanErr := v.findOrphanedTags(ctx)
		if orphanErr != nil {
			klog.Warningf("Error detecting orphaned tags: %v", orphanErr)
			orphanDetectionFailed = true
		} else if len(orphans) > 0 {
			klog.V(2).Infof("Found %d orphaned tags, running cleanup", len(orphans))
			unresolved = v.detachOrphanTags(ctx, orphans)
			v.unresolvedOrphans = unresolved
		}
	}

	// If this vCenter has zero failure domains BUT other vCenters have FDs
	// (meaning this vCenter was specifically removed from the FD list), and a
	// policy exists, the policy is non-compliant (no datastores carry the tag).
	// Delete it — unless there are unresolved orphans (PV-blocked or API errors).
	// When NO vCenters have FDs (legacy single-datastore path), keep the policy.
	vSphereInfraConfig := v.infra.Spec.PlatformSpec.VSphere
	globalFDCount := 0
	if vSphereInfraConfig != nil {
		globalFDCount = len(vSphereInfraConfig.FailureDomains)
	}
	if v.day2Enabled && len(v.failureDomains) == 0 && globalFDCount > 0 && v.policyCreated && !orphanDetectionFailed && len(unresolved) == 0 {
		klog.V(2).Infof("vCenter %s has zero failure domains, deleting orphaned SPBM profile %s",
			v.vcenterApiConnection.Hostname, v.policyName)
		if delErr := v.deleteStoragePolicy(ctx); delErr != nil {
			klog.Warningf("Failed to delete orphaned storage policy %s on vCenter %s: %v",
				v.policyName, v.vcenterApiConnection.Hostname, delErr)
			return v.policyName, nil
		}
		v.policyCreated = false
		if v.recorder != nil {
			v.recorder.Eventf("StoragePolicyDeleted", "Deleted orphaned SPBM profile %s from vCenter %s",
				v.policyName, v.vcenterApiConnection.Hostname)
		}
		return "", nil
	}

	if len(v.failureDomains) > 0 {
		klog.V(4).Info("Creating zonal policy")
		return v.createZonalStoragePolicy(ctx)
	}

	// Legacy single-datastore path: only for clusters upgraded from a version
	// that used a non-FailureDomain config.
	if v.vcenterApiConnection.Config == nil || v.vcenterApiConnection.Config.LegacyConfig == nil {
		if globalFDCount > 0 {
			if v.policyCreated {
				return v.policyName, nil
			}
			return "", nil
		}
		return "", fmt.Errorf("no failure domains and no legacy config available")
	}

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
		return fmt.Errorf("%s", msg)
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
		klog.Errorf("%s", msg)
		return fmt.Errorf("%s", msg)
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
		klog.Errorf("%s", msg)
		return false, fmt.Errorf("%s", msg)
	}

	ids, err := pbmClient.QueryProfile(ctx, rtype, string(category))
	if err != nil {
		msg := fmt.Sprintf("error querying profiles: %v", err)
		klog.Errorf("%s", msg)
		return false, fmt.Errorf("%s", msg)
	}

	profiles, err := pbmClient.RetrieveContent(ctx, ids)
	if err != nil {
		msg := fmt.Sprintf("error fetching policy profiles: %v", err)
		klog.Errorf("%s", msg)
		return false, fmt.Errorf("%s", msg)
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

	for _, p := range profiles {
		if p.GetPbmProfile().Name == v.policyName {
			_, err = pbmClient.DeleteProfile(ctx, []types.PbmProfileId{p.GetPbmProfile().ProfileId})
			if err != nil {
				return fmt.Errorf("error deleting profile %s: %v", v.policyName, err)
			}
			return nil
		}
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
	Datacenter  string
	Datastore   string
	Reference   vim.ManagedObjectReference
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

	attachedObjects, err := tagManager.GetAttachedObjectsOnTags(ctx, []string{tag.ID})
	if err != nil {
		return nil, fmt.Errorf("error getting attached objects for tag %s: %v", v.tagName, err)
	}

	// Build set of current failure domain (datacenter, datastore) tuples
	// for THIS vCenter only (v.failureDomains is pre-filtered by
	// NewStoragePolicyAPI to match v.vcenterApiConnection.Hostname).
	currentFDs := sets.NewString()
	for _, fd := range v.failureDomains {
		key := fd.Topology.Datacenter + "/" + fd.Topology.Datastore
		currentFDs.Insert(key)
	}

	// Also include the workspace default datastore if no failure domains
	if currentFDs.Len() == 0 && conn.Config != nil && conn.Config.LegacyConfig != nil {
		workspaceDC := conn.Config.LegacyConfig.Workspace.Datacenter
		workspaceDS := conn.Config.LegacyConfig.Workspace.DefaultDatastore
		if workspaceDC != "" && workspaceDS != "" {
			currentFDs.Insert(workspaceDC + "/" + workspaceDS)
		}
	}

	var orphans []orphanedDatastore

	for _, tagResult := range attachedObjects {
		for _, objRef := range tagResult.ObjectIDs {
			if objRef.Reference().Type != "Datastore" {
				continue
			}

			dcName, dsName, err := v.resolveDatastoreIdentity(ctx, conn, objRef.Reference())
			if err != nil {
				klog.Warningf("Could not resolve identity for tagged object %s: %v", objRef.Reference(), err)
				continue
			}

			key := dcName + "/" + dsName
			if !currentFDs.Has(key) {
				orphan := orphanedDatastore{
					Datacenter: dcName,
					Datastore:  dsName,
					Reference:  objRef.Reference(),
				}
				// Check if any CNS volumes are backed by this datastore
				orphan.HasBoundPVs = v.datastoreHasCnsVolumes(ctx, conn, objRef.Reference())
				orphans = append(orphans, orphan)
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
			klog.V(4).Infof("Failed to list datastores in datacenter %s: %v", dc.Name(), err)
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

// datastoreHasCnsVolumes checks if any CNS volumes are backed by the given datastore.
// Returns true if volumes are found, false if no volumes or the CNS client is unavailable.
// When the CNS client is unavailable (older vCenter), returns true (conservative — skip detach).
func (v *storagePolicyAPI) datastoreHasCnsVolumes(ctx context.Context, conn *vclib.VSphereConnection, datastoreMOR vim.ManagedObjectReference) bool {
	// Login to CNS if not already done
	if conn.CnsClient() == nil {
		if err := conn.LoginToCNS(ctx); err != nil {
			klog.Warningf("CNS API unavailable on vCenter %s, treating datastore as having bound PVs (conservative): %v",
				conn.Hostname, err)
			return true // Conservative: skip detach when CNS unavailable
		}
	}

	cnsClient := conn.CnsClient()
	if cnsClient == nil {
		klog.Warningf("CNS client nil after login on vCenter %s, treating datastore as having bound PVs", conn.Hostname)
		return true
	}

	queryFilter := cnstypes.CnsQueryFilter{
		Datastores: []vim.ManagedObjectReference{datastoreMOR},
	}

	queryResult, err := cnsClient.QueryVolume(ctx, &queryFilter)
	if err != nil {
		klog.Warningf("Error querying CNS volumes for datastore %s: %v, treating as having bound PVs", datastoreMOR, err)
		return true // Conservative on error
	}

	if queryResult != nil && len(queryResult.Volumes) > 0 {
		klog.V(2).Infof("Datastore %s has %d CNS volumes, skipping orphan cleanup", datastoreMOR, len(queryResult.Volumes))
		return true
	}

	return false
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

	if v.forceCleanup {
		klog.V(2).Infof("Force orphan cleanup enabled via ClusterCSIDriver annotation, skipping PV safety check")
	}

	tagManager := tags.NewManager(conn.RestClient)

	tag, err := tagManager.GetTag(ctx, v.tagName)
	if err != nil || tag == nil || tag.ID == "" {
		klog.Errorf("Could not find tag %s for orphan detach: %v", v.tagName, err)
		return orphans
	}

	utils.OrphanTagsDetectedTotal.Add(float64(len(orphans)))

	var unresolved []orphanedDatastore

	for i := range orphans {
		orphan := &orphans[i]

		if orphan.HasBoundPVs && !v.forceCleanup {
			klog.Warningf("Skipping tag detach from datastore %s in datacenter %s: has bound PVs",
				orphan.Datastore, orphan.Datacenter)
			utils.TagOperationsTotal.WithLabelValues("skip", "pv_blocked").Inc()
			if v.recorder != nil {
				v.recorder.Warningf("OrphanTagBlocked", "Orphan tag detach blocked on datastore %s/%s: PVs exist",
					orphan.Datacenter, orphan.Datastore)
			}
			unresolved = append(unresolved, *orphan)
			continue
		}

		err := tagManager.DetachTag(ctx, tag.ID, orphan.Reference)
		if err != nil {
			klog.Errorf("Failed to detach tag %s from datastore %s in datacenter %s: %v",
				v.tagName, orphan.Datastore, orphan.Datacenter, err)
			utils.TagOperationsTotal.WithLabelValues("detach", "error").Inc()
			unresolved = append(unresolved, *orphan)
			continue
		}

		klog.V(2).Infof("Successfully detached tag %s from orphaned datastore %s in datacenter %s",
			v.tagName, orphan.Datastore, orphan.Datacenter)
		utils.TagOperationsTotal.WithLabelValues("detach", "success").Inc()
		if v.recorder != nil {
			v.recorder.Eventf("OrphanTagDetached", "Detached orphan tag from datastore %s/%s",
				orphan.Datacenter, orphan.Datastore)
		}
	}

	return unresolved
}
