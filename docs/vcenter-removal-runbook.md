# Removing a vCenter (Multi-vCenter Day-2)

This note describes what happens — and what an administrator should expect —
when a secondary vCenter is removed from a multi-vCenter cluster
(`VSphereMultiVCenterDay2` feature gate). It does not apply to the primary
("workspace") vCenter, which cannot be removed while the cluster is running.

## Recommended (deterministic) order: drop failure domains first

The safest, most predictable way to decommission a vCenter is a two-step
process:

1. Remove the vCenter's entries from
   `Infrastructure.spec.platformSpec.vsphere.failureDomains`, but **leave
   its entry in `Infrastructure.spec.platformSpec.vsphere.vCenters`**.
2. Wait for the operator to finish cleanup:
   - The `VMwareVSphereDriverStorageClassControllerOrphanCleanupPending`
     condition on the `storage` ClusterOperator goes to `False`.
   - The vCenter's SPBM storage profile is deleted.
3. Only then remove the vCenter's entry from `vCenters` too.

Because the vCenter is still reachable and still has valid credentials
throughout this process, cleanup happens on the very next sync — there is no
reconnect/retry dance, and no risk of the cleanup window expiring before the
vCenter becomes unreachable.

## What happens if the vCenter entry is removed directly

Sometimes the vCenter entry disappears from `vCenters` (and its failure
domains) in one step — for example, during disaster recovery or when
undoing a botched multi-vCenter addition. The operator handles this too, on
a best-effort basis:

- The operator reconnects to the removed vCenter one more time, using the
  credentials it still has cached, and runs the same tag-detach /
  SPBM-profile-deletion logic used for the recommended path above.
- This reconnect is retried across sync cycles (subject to the same
  per-host backoff as any other vCenter) for up to **48 hours** from the
  moment the removal was first detected.
- While cleanup is pending, the `VCenterRemovalPending` condition is `True`
  and reports which vCenter(s) are still being cleaned up.
- If a *secondary* vCenter is unreachable (not yet removed, just
  temporarily down — e.g. a maintenance window), the operator sets
  `SecondaryVCenterUnreachable=True` instead of degrading the whole
  operator; the workspace vCenter and any other healthy vCenters keep
  syncing normally.
- Outcomes after the 48h window (or as soon as cleanup succeeds):
  - **Success**: the profile is deleted, the tag is detached (unless still
    blocked by bound PVs — see below), a `VCenterCleanupSucceeded` event is
    recorded, and the
    `vsphere_csi_vcenter_removal_cleanup_total{result="success"}` metric is
    incremented. `VCenterRemovalPending` clears.
  - **Abandoned**: if the vCenter never becomes reachable again (or its
    credentials were removed/rotated) within 48h, a
    `VCenterCleanupAbandoned` event is recorded, the
    `vsphere_csi_vcenter_removal_cleanup_total{result="abandoned"}` metric
    is incremented, and `VCenterRemovalPending` clears — the operator stops
    retrying and the tag/profile are left as-is.

### PV safety check

Even when the operator can reach the removed vCenter, it will **not** detach
a tag from a datastore that still has bound PersistentVolumes on it — doing
so could break volume provisioning/attachment for those PVs. In that case:

- The `OrphanCleanupPending` condition stays `True`.
- The tag and SPBM profile are left in place.
- Cleanup resumes automatically once the operator can confirm there are no
  more bound PVs on that datastore, or an administrator can force the
  removal early (see below).

### Forcing cleanup

An administrator can bypass the PV safety check by annotating the
`ClusterCSIDriver` object for this driver:

```shell
oc annotate clustercsidriver csi.vsphere.vmware.com \
  csi.vsphere.vmware.com/force-orphan-cleanup=true
```

Only use this once you have independently confirmed there are no volumes
that still depend on the removed vCenter's datastores — forcing cleanup
skips the safety check entirely.

### Manual cleanup, if automatic cleanup can't complete

If the vCenter has been permanently decommissioned and a
`VCenterCleanupAbandoned` event has been recorded (or you don't want to wait
48h), clean up manually:

1. In vCenter (if it's still reachable through some other means): delete
   the SPBM storage policy profile named
   `openshift-storage-policy-<infrastructure-name>` and detach/delete the
   `openshift-vsphere` category tag from the vCenter's datastores.
2. If the vCenter is gone for good, no further action is required —
   OpenShift no longer references it once it's out of `vCenters`.
3. As defense in depth, also remove that vCenter's
   `<host>.username` / `<host>.password` keys from the
   `vsphere-creds` secret in `kube-system` once you're sure cleanup is no
   longer needed, so the operator stops holding onto credentials for a
   decommissioned host.
