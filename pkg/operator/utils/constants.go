package utils

const (
	OpenshiftCSIDriverAnnotationKey = "csi.openshift.io/managed"
	VSphereDriverName               = "csi.vsphere.vmware.com"
	DefaultNamespace                = "openshift-cluster-csi-drivers"
	InfraGlobalName                 = "cluster"
	CloudConfigNamespace            = "openshift-config"
	ManagedConfigNamespace          = "openshift-config-managed"
	SecretName                      = "vmware-vsphere-cloud-credentials"
	AdminGateConfigMap              = "admin-gates"
)
