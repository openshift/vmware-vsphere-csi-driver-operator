package checks

import (
	"context"
	"fmt"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type CheckExistingDriver struct{}

func (c *CheckExistingDriver) Check(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	csiDriver, err := checkOpts.apiClient.GetCSIDriver(utils.VSphereDriverName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.checkForCSINode(ctx, checkOpts)
		}
		checkResult := ClusterCheckResult{
			CheckStatus: CheckStatusOpenshiftAPIError,
			CheckError:  err,
			Action:      CheckActionDegrade,
			Reason:      fmt.Sprintf("failed to check for existing csiDriver of type %s: %v", utils.VSphereDriverName, err),
		}
		return []ClusterCheckResult{checkResult}
	}
	annotations := csiDriver.GetAnnotations()
	if _, ok := annotations[utils.OpenshiftCSIDriverAnnotationKey]; !ok {
		reason := fmt.Errorf("found existing unsupported %s driver", utils.VSphereDriverName)
		return []ClusterCheckResult{makeFoundExistingDriverResult(reason)}
	}
	return []ClusterCheckResult{}
}

func (c *CheckExistingDriver) checkForCSINode(ctx context.Context, checkOpts CheckArgs) []ClusterCheckResult {
	csiNodeObjects, err := checkOpts.apiClient.ListCSINodes()
	if err != nil {
		checkResult := ClusterCheckResult{
			CheckStatus: CheckStatusOpenshiftAPIError,
			CheckError:  err,
			Action:      CheckActionDegrade,
			Reason:      fmt.Sprintf("failed to list csi node objects for driver %s: %v", utils.VSphereDriverName, err),
		}
		return []ClusterCheckResult{checkResult}
	}
	for i := range csiNodeObjects {
		csiNode := csiNodeObjects[i]
		drivers := csiNode.Spec.Drivers
		for j := range drivers {
			driver := drivers[j]
			if driver.Name == utils.VSphereDriverName {
				reason := fmt.Errorf("found existing unsupported %s driver on node %s", driver.Name, csiNode.Name)
				return []ClusterCheckResult{makeFoundExistingDriverResult(reason)}
			}
		}
	}
	return []ClusterCheckResult{}
}
