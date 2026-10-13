package vspherecontroller

import (
	"context"
	"fmt"
	"strings"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/assets"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

// prefix for conditions used by all conditions
const (
	vsphereOperatorPrefix = "VMwareVSphere"
)

var untouchableConditions = sets.New[string](
	"VMwareVSphereControllerDegraded",
	"VMwareVSphereDriverFeatureConfigControllerDegraded",
	"VMWareVSphereDriverServiceMonitorControllerDegraded",
)

func (c *VSphereController) removeOperands(ctx context.Context, status *operatorapi.OperatorStatus) error {
	assetsToBeDeleted := []string{
		"rbac/privileged_role.yaml",
		"rbac/controller_privileged_binding.yaml",
		"rbac/node_privileged_binding.yaml",
		"rbac/csi_driver_controller_role.yaml",
		"rbac/csi_driver_controller_binding.yaml",
		"rbac/csi_driver_node_cluster_role.yaml",
		"rbac/csi_driver_node_cluster_binding.yaml",
		"rbac/csi_driver_node_role.yaml",
		"rbac/csi_driver_node_binding.yaml",
		"rbac/main_attacher_binding.yaml",
		"rbac/main_provisioner_binding.yaml",
		"rbac/volumesnapshot_reader_provisioner_binding.yaml",
		"rbac/main_resizer_binding.yaml",
		"rbac/storageclass_reader_resizer_binding.yaml",
		"rbac/kube_rbac_proxy_role.yaml",
		"rbac/kube_rbac_proxy_binding.yaml",
		"rbac/prometheus_role.yaml",
		"rbac/prometheus_rolebinding.yaml",
		"rbac/main_snapshotter_binding.yaml",
		"rbac/lease_leader_election_role.yaml",
		"rbac/lease_leader_election_rolebinding.yaml",
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
		// dynamic assets used by controller
		"volumesnapshotclass.yaml",
		"controller.yaml",
		"node.yaml",
		"servicemonitor.yaml",
		"webhook/deployment.yaml",
		"vsphere_cloud_config_secret.yaml",
		"vsphere_features_config.yaml",
	}

	compositeClient := resourceapply.NewKubeClientHolder(c.kubeClient).WithDynamicClient(c.apiClients.DynamicClient)
	applyResult := resourceapply.DeleteAll(ctx, compositeClient, c.eventRecorder, assets.ReadFile, assetsToBeDeleted...)
	allErrors := []error{}
	for _, result := range applyResult {
		if result.Error != nil {
			allErrors = append(allErrors, result.Error)
		}
	}
	if len(allErrors) > 0 {
		return utilerrors.NewAggregate(allErrors)
	}

	err := c.storageClassController.SyncRemove(ctx)
	if err != nil {
		return fmt.Errorf("error removing storageclass: %v", err)
	}
	return c.removeConditions(ctx, status)
}

func (c *VSphereController) getDisabledConditionName() string {
	return c.name + "Disabled"
}

func (c *VSphereController) removeConditions(ctx context.Context, status *operatorapi.OperatorStatus) error {
	updateFuncs := []v1helpers.UpdateStatusFunc{}
	matchingConditions := []operatorapi.OperatorCondition{}

	// make sure to not remove disabled condition
	untouchableConditions.Insert(c.getDisabledConditionName())

	originalConditions := status.DeepCopy().Conditions
	for _, condition := range originalConditions {
		if strings.HasPrefix(condition.Type, vsphereOperatorPrefix) && !untouchableConditions.Has(condition.Type) {
			klog.Infof("Removing condition %s", condition.Type)
			matchingConditions = append(matchingConditions, condition)
		}
	}
	updateFuncs = append(updateFuncs, func(status *operatorapi.OperatorStatus) error {
		for _, condition := range matchingConditions {
			v1helpers.RemoveOperatorCondition(&status.Conditions, condition.Type)
		}
		return nil
	})
	// also add a condition to indicate that the operator is disabled
	updateFuncs = append(updateFuncs, c.addDisabledCondition())
	_, _, err := v1helpers.UpdateStatus(ctx, c.operatorClient, updateFuncs...)
	return err
}

func (c *VSphereController) addDisabledCondition() v1helpers.UpdateStatusFunc {
	return func(status *operatorapi.OperatorStatus) error {
		disabledConditionName := c.getDisabledConditionName()
		disabledCond := operatorapi.OperatorCondition{
			Type:    disabledConditionName,
			Status:  operatorapi.ConditionTrue,
			Message: "Operator is disabled",
		}
		v1helpers.SetOperatorCondition(&status.Conditions, disabledCond)
		return nil
	}
}
