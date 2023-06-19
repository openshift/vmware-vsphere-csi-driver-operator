package vspherecontroller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"

	"github.com/openshift/library-go/pkg/operator/resource/resourcehash"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	"github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/klog/v2"
)

func (c *VSphereController) createCSIDriver() {
	csiControllerSet := csicontrollerset.NewCSIControllerSet(
		c.operatorClient,
		c.eventRecorder,
	).WithLogLevelController().WithManagementStateController(
		driverOperandName,
		false,
	).WithStaticResourcesController(
		"VMwareVSphereDriverStaticResourcesController",
		c.kubeClient,
		c.apiClients.DynamicClient,
		c.apiClients.KubeInformers,
		assets.ReadFile,
		[]string{
			"rbac/privileged_role.yaml",
			"rbac/controller_privileged_binding.yaml",
			"rbac/node_privileged_binding.yaml",
			"rbac/csi_driver_controller_role.yaml",
			"rbac/csi_driver_controller_binding.yaml",
			"rbac/csi_driver_node_cluster_role.yaml",
			"rbac/csi_driver_node_cluster_binding.yaml",
			"rbac/csi_driver_node_role.yaml",
			"rbac/csi_driver_node_binding.yaml",
			"rbac/attacher_role.yaml",
			"rbac/attacher_binding.yaml",
			"rbac/provisioner_role.yaml",
			"rbac/provisioner_binding.yaml",
			"rbac/resizer_role.yaml",
			"rbac/resizer_binding.yaml",
			"rbac/kube_rbac_proxy_role.yaml",
			"rbac/kube_rbac_proxy_binding.yaml",
			"rbac/prometheus_role.yaml",
			"rbac/prometheus_rolebinding.yaml",
			"rbac/snapshotter_role.yaml",
			"rbac/snapshotter_binding.yaml",
			"controller_sa.yaml",
			"controller_pdb.yaml",
			"node_sa.yaml",
			"csidriver.yaml",
			"service.yaml",
			"ca_configmap.yaml",
			// Static assets used by the webhook
			"webhook/config.yaml",
			"webhook/sa.yaml",
			"webhook/service.yaml",
			"webhook/configuration.yaml",
			"webhook/rbac/role.yaml",
			"webhook/rbac/rolebinding.yaml",
			"webhook/pdb.yaml",
		},
	).WithConditionalStaticResourcesController(
		"VMwareVSphereDriverConditionalStaticResourcesController",
		c.kubeClient,
		c.apiClients.DynamicClient,
		c.apiClients.KubeInformers,
		assets.ReadFile,
		[]string{
			"volumesnapshotclass.yaml",
		},
		// Only install when CRD exists.
		func() bool {
			name := "volumesnapshotclasses.snapshot.storage.k8s.io"
			_, err := c.apiClients.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), name, metav1.GetOptions{})
			return err == nil
		},
		// Don't ever remove.
		func() bool {
			return false
		},
	).WithCSIConfigObserverController(
		"VMwareVSphereDriverCSIConfigObserverController",
		c.apiClients.ConfigInformers,
	).WithCSIDriverControllerService(
		"VMwareVSphereDriverControllerServiceController",
		assets.ReadFile,
		"controller.yaml",
		c.apiClients.KubeClient,
		c.apiClients.KubeInformers.InformersFor(defaultNamespace),
		c.apiClients.ConfigInformers,
		[]factory.Informer{
			c.apiClients.SecretInformer.Informer(),
			c.apiClients.ConfigMapInformer.Informer(),
			c.apiClients.NodeInformer.Informer(),
		},
		WithVSphereCredentials(defaultNamespace, cloudCredSecretName, c.apiClients.SecretInformer),
		WithSyncerImageHook("vsphere-syncer"),
		WithLogLevelDeploymentHook(),
		c.topologyHook,
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithCABundleDeploymentHook(
			defaultNamespace,
			trustedCAConfigMap,
			c.apiClients.ConfigMapInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			cloudCredSecretName,
			c.apiClients.SecretInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			metricsCertSecretName,
			c.apiClients.SecretInformer,
		),
		csidrivercontrollerservicecontroller.WithReplicasHook(c.nodeLister),
	).WithCSIDriverNodeService(
		"VMwareVSphereDriverNodeServiceController",
		assets.ReadFile,
		"node.yaml",
		c.apiClients.KubeClient,
		c.apiClients.KubeInformers.InformersFor(defaultNamespace),
		[]factory.Informer{c.apiClients.ConfigMapInformer.Informer()},
		WithLogLevelDaemonSetHook(),
		csidrivernodeservicecontroller.WithObservedProxyDaemonSetHook(),
		csidrivernodeservicecontroller.WithCABundleDaemonSetHook(
			defaultNamespace,
			trustedCAConfigMap,
			c.apiClients.ConfigMapInformer,
		),
		WithConfigMapDaemonSetAnnotationHook("vsphere-csi-config", defaultNamespace, c.apiClients.ConfigMapInformer),
	).WithServiceMonitorController(
		"VMWareVSphereDriverServiceMonitorController",
		c.apiClients.DynamicClient,
		assets.ReadFile,
		"servicemonitor.yaml",
	)
	c.controllers = append(c.controllers, conditionalController{
		name:       driverOperandName,
		controller: csiControllerSet,
	})
}

func (c *VSphereController) topologyHook(opSpec *operatorapi.OperatorSpec, deployment *appsv1.Deployment) error {
	clusterCSIDriver, err := c.apiClients.ClusterCSIDriverInformer.Lister().Get(utils.VSphereDriverName)
	if err != nil {
		return err
	}

	infra, err := c.infraLister.Get(infraGlobalName)
	if err != nil {
		return err
	}
	topologyCategories := utils.GetTopologyCategories(clusterCSIDriver, infra)
	if len(topologyCategories) > 0 {
		containers := deployment.Spec.Template.Spec.Containers
		for i := range containers {
			if containers[i].Name != "csi-provisioner" {
				continue
			}

			containers[i].Args = append(containers[i].Args, "--feature-gates=Topology=true", "--strict-topology")
		}
		deployment.Spec.Template.Spec.Containers = containers
	}
	return nil
}

func WithVSphereCredentials(
	namespace string,
	secretName string,
	secretInformer corev1informers.SecretInformer,
) deploymentcontroller.DeploymentHookFunc {
	return func(opSpec *operatorapi.OperatorSpec, deployment *appsv1.Deployment) error {
		secret, err := secretInformer.Lister().Secrets(namespace).Get(secretName)
		if err != nil {
			return err
		}

		// CCO generates a secret that contains dynamic keys, for example:
		// oc get secret/vmware-vsphere-cloud-credentials -o json | jq .data
		// {
		//   "vcenter.xyz.vmwarevmc.com.password": "***",
		//   "vcenter.xyz.vmwarevmc.com.username": "***"
		// }
		// So we need to figure those keys out
		var usernameKey, passwordKey string

		if len(secret.Data) > 2 {
			klog.Warningf("CSI driver can only connect to one vcenter, more than 1 set of credentials found for CSI driver")
		}

		for k := range secret.Data {
			if strings.HasSuffix(k, ".username") {
				usernameKey = k
			} else if strings.HasSuffix(k, ".password") {
				passwordKey = k
			}
		}
		if usernameKey == "" || passwordKey == "" {
			return fmt.Errorf("could not find vSphere credentials in secret %s/%s", secret.Namespace, secret.Name)
		}

		// Add to csi-driver and vsphere-syncer containers the vSphere credentials, as env vars.
		containers := deployment.Spec.Template.Spec.Containers
		for i := range containers {
			if containers[i].Name != "csi-driver" && containers[i].Name != "vsphere-syncer" {
				continue
			}
			containers[i].Env = append(
				containers[i].Env,
				newSecretEnvVar(secretName, "VSPHERE_USER", usernameKey),
				newSecretEnvVar(secretName, "VSPHERE_PASSWORD", passwordKey),
			)
		}
		deployment.Spec.Template.Spec.Containers = containers
		return nil
	}
}

func WithSyncerImageHook(containerName string) deploymentcontroller.DeploymentHookFunc {
	return func(opSpec *operatorapi.OperatorSpec, deployment *appsv1.Deployment) error {
		containers := deployment.Spec.Template.Spec.Containers
		for i := range containers {
			if containers[i].Name == containerName {
				containers[i].Image = os.Getenv(envVMWareVsphereDriverSyncerImage)
			}
		}
		deployment.Spec.Template.Spec.Containers = containers
		return nil
	}
}

// WithLogLevelDeploymentHook sets the X_CSI_DEBUG and LOGGER_LEVEL environment variables
// when CR.LogLevel is Debug or higher.
func WithLogLevelDeploymentHook() deploymentcontroller.DeploymentHookFunc {
	return func(opSpec *operatorapi.OperatorSpec, deployment *appsv1.Deployment) error {
		deployment.Spec.Template.Spec.Containers = maybeAppendDebug(deployment.Spec.Template.Spec.Containers, opSpec)
		return nil
	}
}

// WithLogLevelDaemonSetHook sets the X_CSI_DEBUG and LOGGER_LEVEL environment variables
// when CR.LogLevel is Debug or higher.
func WithLogLevelDaemonSetHook() csidrivernodeservicecontroller.DaemonSetHookFunc {
	return func(opSpec *operatorapi.OperatorSpec, ds *appsv1.DaemonSet) error {
		ds.Spec.Template.Spec.Containers = maybeAppendDebug(ds.Spec.Template.Spec.Containers, opSpec)
		return nil
	}
}

func WithConfigMapDaemonSetAnnotationHook(configMapName, namespace string, configMapInformer corev1informers.ConfigMapInformer) csidrivernodeservicecontroller.DaemonSetHookFunc {
	return func(opSpec *operatorapi.OperatorSpec, ds *appsv1.DaemonSet) error {
		inputHashes, err := resourcehash.MultipleObjectHashStringMapForObjectReferenceFromLister(
			configMapInformer.Lister(),
			nil,
			resourcehash.NewObjectRef().ForConfigMap().InNamespace(namespace).Named(configMapName),
		)
		if err != nil {
			return fmt.Errorf("invalid dependency reference: %w", err)
		}

		return addObjectHash(ds, inputHashes)
	}
}

func addObjectHash(daemonSet *appsv1.DaemonSet, inputHashes map[string]string) error {
	if daemonSet == nil {
		return fmt.Errorf("invalid daemonSet: %v", daemonSet)
	}
	if daemonSet.Annotations == nil {
		daemonSet.Annotations = map[string]string{}
	}
	if daemonSet.Spec.Template.Annotations == nil {
		daemonSet.Spec.Template.Annotations = map[string]string{}
	}
	for k, v := range inputHashes {
		annotationKey := fmt.Sprintf("operator.openshift.io/dep-%s", k)
		if len(annotationKey) > 63 {
			hash := sha256.Sum256([]byte(k))
			annotationKey = fmt.Sprintf("operator.openshift.io/dep-%x", hash)
			annotationKey = annotationKey[:63]
		}
		daemonSet.Annotations[annotationKey] = v
		daemonSet.Spec.Template.Annotations[annotationKey] = v
	}
	return nil
}

// maybeAppendDebug works like the append() builtin; it returns a new slice of containers
// with the logging env vars properly set (or not).
func maybeAppendDebug(containers []v1.Container, opSpec *operatorapi.OperatorSpec) []v1.Container {
	// Don't set the debug option when the current level is lower than debug
	if loglevel.LogLevelToVerbosity(opSpec.LogLevel) < loglevel.LogLevelToVerbosity(operatorapi.Debug) {
		return containers
	}
	for i := range containers {
		if containers[i].Name != "csi-driver" && containers[i].Name != "vsphere-syncer" {
			continue
		}
		containers[i].Env = append(
			containers[i].Env,
			v1.EnvVar{Name: "X_CSI_DEBUG", Value: "true"},
		)
		containers[i].Env = append(
			containers[i].Env,
			v1.EnvVar{Name: "LOGGER_LEVEL", Value: "DEVELOPMENT"},
		)
	}
	return containers
}
func newSecretEnvVar(secretName, envVarName, key string) v1.EnvVar {
	return v1.EnvVar{
		Name: envVarName,
		ValueFrom: &v1.EnvVarSource{
			SecretKeyRef: &v1.SecretKeySelector{
				LocalObjectReference: v1.LocalObjectReference{
					Name: secretName,
				},
				Key: key,
			},
		},
	}
}
