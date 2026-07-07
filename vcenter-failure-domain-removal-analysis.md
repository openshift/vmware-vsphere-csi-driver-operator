# vCenter and Failure Domain Removal Analysis

## How This Operator Handles vCenter and Failure Domain Removal

**TL;DR:** This operator does not handle removal at all. There is no reconciliation of stale state when vCenters or failure domains are removed.

---

## Architecture: Single vCenter Connection Model

The operator is fundamentally designed around a **single vCenter connection per sync cycle**:

1. **`VSphereConnection`** (`pkg/operator/vclib/connection.go`) holds credentials and a `govmomi.Client` for exactly **one** vCenter host, derived from `cfg.Workspace.VCenterIP`.

2. **`GetDatacenters()`** (`pkg/operator/utils/topology.go:48-62`) explicitly **rejects** multi-vCenter cloud configs:

   ```go
   if len(virtualCenterIPs) != 1 {
       return nil, fmt.Errorf("cloud config must define a single VirtualCenter")
   }
   ```

3. **`createVCenterConnection()`** (`pkg/operator/vspherecontroller/vspherecontroller.go:402`) reads the cloud config, extracts the single workspace `VCenterIP`, looks up that single vCenter's credentials from the secret, and creates one `VSphereConnection`.

Even though the Infrastructure resource can declare multiple vCenters and failure domains, **the operator only ever connects to one vCenter** -- the one in `cfg.Workspace.VCenterIP`.

---

## How Failure Domains Are Consumed

Failure domains are consumed in **two places**, and neither handles removal:

### 1. Storage Policy / Tag Creation (`createZonalStoragePolicy`)

In `pkg/operator/storageclasscontroller/vmware.go:132-161`, when failure domains exist, `createZonalStoragePolicy` iterates over `infra.Spec.PlatformSpec.VSphere.FailureDomains` and:

- For **each** failure domain, calls `attachTags()` which tags the datastore in that failure domain's datacenter with the cluster's tag.
- If the storage policy hasn't been created yet, calls `createStorageProfile()` once.

**On removal of a failure domain**, the next sync simply stops iterating over the removed failure domain. However:

- **Tags on the removed datastore are never cleaned up.** The tag remains attached to the datastore in vCenter forever. The descriptions literally say `"Added by openshift-install do not remove"`.
- **The storage policy is never updated.** The SPBM profile (tag-based placement policy) references the category, not individual datastores. Since all datastores share the same tag name and category, removing a failure domain's datastore doesn't change the policy itself.
- **`deleteStoragePolicy()` exists** (line 402) but is **only called from tests** -- it has zero references in production code.

### 2. CSI Config Map (`applyClusterCSIDriverChange`)

In `pkg/operator/vspherecontroller/vspherecontroller.go:567-610`, the CSI driver cloud config is built from the template `assets/csi_cloud_config.ini`:

```ini
[VirtualCenter "${VCENTER}"]
datacenters = "${DATACENTERS}"
```

This creates a config for **only the single VCenterIP** from the workspace config. It does **not** enumerate vCenters from failure domains. If vCenter A is removed, this config would need to change only if vCenter A was the workspace vCenter -- and that would break everything else too, since the operator can't connect to a different vCenter.

---

## Behavior Matrix: Adding and Removing Failure Domains / vCenters

| Action | Storage Policy | Storage Class | Tags | CSI Config |
|--------|---------------|---------------|------|------------|
| **Add failure domain** | Next sync tags the new datastore; policy unchanged (category-based) | No change -- same `thin-csi` StorageClass with same policy name | Tag created on new datastore | No change (only workspace vCenter in config) |
| **Remove failure domain** | Next sync just stops iterating to the removed domain; no cleanup | No change | **Orphaned tag left on removed datastore** | No change |
| **Add vCenter** | **Not supported** -- operator connects to single vCenter only | No change | Would need to tag datastores on new vCenter, but **can't** because only one connection | No change (template has single `[VirtualCenter]` section) |
| **Remove vCenter** | If it's the workspace vCenter, **operator breaks**. If it's a secondary vCenter (Day-2 multi-vCenter, `VSphereMultiVCenterDay2` gate): operator reconnects to it best-effort (bounded retries, up to 48h) to delete its now-orphaned SPBM profile; abandoned via a `VCenterCleanupAbandoned` event/metric if reconnection never succeeds within that window | No change | Best-effort automatic detach of tags on removed vCenter's datastores (blocked/pending if PVs still reference them, tracked via `OrphanCleanupPending`) instead of orphaned forever | No change |

---

## Topology Awareness

Topology categories are derived from failure domains in `GetInfraTopologyCategories()` (`pkg/operator/utils/topology.go:18-27`):

```go
if len(failureDomains) > 1 {
    return []string{"openshift-zone", "openshift-region"}
}
```

This means:

- If you go from **2+ failure domains to 1**, topology categories become empty, `improved-volume-topology` gets dropped from the feature config, and the provisioner loses `--feature-gates=Topology=true, --strict-topology` args.
- If you go from **1 to 0 failure domains**, the operator switches from the zonal policy path to the single-datastore path in `createStoragePolicy()`.

**Neither transition cleans up the old state.** The storage policy created for zonal placement is never deleted, and the `thin-csi` StorageClass continues referencing whatever policy name was set.

---

## Key Gaps and Risks

### 1. No Orphan Cleanup

Removing a failure domain leaves vSphere tags on datastores. These are harmless (they don't affect placement since the datastore is no longer in the failure domain list) but they accumulate indefinitely.

### 2. No Multi-vCenter Support in the Operator Connection Layer

Despite the Infrastructure API allowing multiple vCenters in failure domains, this operator hard-errors if the cloud config has more than one `VirtualCenter`, and the CSI config template only has a single `[VirtualCenter]` section.

### 3. `deleteStoragePolicy` is Dead Code in Production

The function exists, it works (it's tested), but nothing calls it outside of test cleanup. There is no production path that removes a storage policy.

### 4. No Transition Handling for Topology Mode Changes

Switching from multi-failure-domain to single-failure-domain (or vice versa) doesn't migrate or clean up the storage policy. It relies on `checkForExistingPolicy` finding the named policy and setting `policyCreated = true` to skip re-creation, but the existing policy may reference a tag-based placement that no longer matches reality.

### 5. StorageClass Parameters Are Effectively Immutable

The `thin-csi` StorageClass parameters (specifically `StoragePolicyName`) are set once via template substitution. If the policy name were to change (it won't -- it's derived from `InfrastructureName`), the StorageClass wouldn't be updated because `ApplyStorageClass` uses server-side apply which won't change immutable fields on existing StorageClasses.

---

## Relevant Source Files

| File | Role |
|------|------|
| `pkg/operator/vclib/connection.go` | Single vCenter connection management |
| `pkg/operator/vspherecontroller/vspherecontroller.go` | Main sync loop, CSI config map assembly, single vCenter login |
| `pkg/operator/storageclasscontroller/vmware.go` | Storage policy and tag creation, zonal policy logic, unused `deleteStoragePolicy` |
| `pkg/operator/storageclasscontroller/storageclasscontroller.go` | Storage class sync, policy name substitution into StorageClass manifest |
| `pkg/operator/utils/topology.go` | Failure domain enumeration, topology category derivation, single-VirtualCenter enforcement |
| `pkg/operator/driverfeaturescontroller.go` | Topology feature flag (`improved-volume-topology`) |
| `pkg/operator/vspherecontroller/driver_starter.go` | Provisioner topology args (`--feature-gates=Topology=true`) |
| `assets/csi_cloud_config.ini` | CSI config template (single `[VirtualCenter]` section) |
| `assets/storageclass.yaml` | StorageClass template with `${STORAGE_POLICY_NAME}` substitution |
