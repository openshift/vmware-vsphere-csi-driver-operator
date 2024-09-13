# Moving CNS volumes between datastores


If you are running out of space on the current datastore or want to move your volumes to a more performant datastores, moving volumes that back PersistentVolume(PV)’s in the Openshift cluster to a different datastore makes sense.

cns-migration is a tool designed to accomplish exactly that in a safe manner. Please note that - only PVs which are not in use by a Pod can be migrated with the current version of CNS migration tool. So, you must ensure that workloads using these persistentvolume(PVs) are scaled down before migrating them. The tool will check if the PVs are in-use, such PVs will be skipped and reported to the user as non migratable. cns-migration tool also can’t migrate volumes across datacenters or multiple vcenter installation.

`cns-migration` tool can only migrate CSI volumes and it can’t migrate older VMDK style volumes between datastores.

Moving CNS volumes in vSphere requires underlying support from the vSphere platform. Following versions of vCenter are required to support this feature:

vCenter 7.0 Update 3o - https://docs.vmware.com/en/VMware-vSphere/7.0/rn/vsphere-vcenter-server-70u3o-release-notes/index.html


vCenter 8.0 Update 2 -  https://docs.vmware.com/en/VMware-vSphere/8.0/rn/vsphere-vcenter-server-802-release-notes/index.html

Before using this feature, please make sure that your underlying vCenter version meets the minimum requirements.

### Compile and build cns-migration

`make`

### Create list of persisentvolumes to migrate

To run the tool, we need to make a text file that contains the names of PVs we want to move to a new datastore. Name of each PV must be specified on a new line:

```
pvc-aaaaab21121
pvc-21212133233
```

Please ensure that - these are persistentvolume(PV) names, not pvc names.

To migrate all CSI PersistentVolume(PV) in the cluster, we can extract pv names via following command:


```oc get pv -o json|jq -r '.items[] | select(.spec.csi.driver=="csi.vsphere.vmware.com")| .metadata.name'```


### Now all PVs in the aforementioned file can be migrated via:

```
export KUBECONFIG=<path-to-kube-config>

./cns-migration --source <source_datastore> --destination <destination_datastore> -volume-file <path_to_file_with_pv_names>```

#### For example:

```./cns-migration --source samsung-nvme --destination foobar -volume-file ./pv.txt```
