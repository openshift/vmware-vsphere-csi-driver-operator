package vspherecontroller

import (
	"context"
	"math"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

var (
	defaultBackoff = wait.Backoff{
		Duration: time.Minute,
		Factor:   2,
		Jitter:   0.01,
		// Don't limit nr. of steps
		Steps: math.MaxInt32,
		// Maximum interval between checks.
		Cap: time.Hour * 1,
	}
)

type vSphereEnvironmentCheckInterface interface {
	Check(ctx context.Context, connection checks.CheckArgs) (time.Duration, checks.ClusterCheckResult, bool)
}

type vSphereEnvironmentCheckerComposite struct {
	backoff   wait.Backoff
	nextCheck time.Time
	lastCheck time.Time
	checkers  []checks.CheckInterface
}

// make sure that vSphereEnvironmentCheckerComposite implements the vSphereEnvironmentCheckInterface
var _ vSphereEnvironmentCheckInterface = &vSphereEnvironmentCheckerComposite{}

func newVSphereEnvironmentChecker() *vSphereEnvironmentCheckerComposite {
	checker := &vSphereEnvironmentCheckerComposite{
		backoff:   defaultBackoff,
		nextCheck: time.Now(),
	}
	checker.checkers = []checks.CheckInterface{
		&checks.CheckExistingDriver{},
		&checks.VCenterChecker{},
		&checks.NodeChecker{},
	}
	return checker
}

func (v *vSphereEnvironmentCheckerComposite) Check(
	ctx context.Context, checkOpts checks.CheckArgs) (time.Duration, checks.ClusterCheckResult, bool) {
	var delay time.Duration
	var checkResult checks.ClusterCheckResult
	if !time.Now().After(v.nextCheck) {
		return delay, checkResult, false
	}

	nextErrorDelay := v.backoff.Step()
	v.lastCheck = time.Now()

	var allChecks []checks.ClusterCheckResult

	for _, checker := range v.checkers {
		result := checker.Check(ctx, checkOpts)
		allChecks = append(allChecks, result...)
	}

	var blockUpgradeResult checks.ClusterCheckResult
	var degradeClusterResult checks.ClusterCheckResult

	// following checks can either block cluster upgrades or degrade the cluster
	// the severity of degradation is higher than blocking upgrades
	for i := range allChecks {
		result := allChecks[i]
		if result.ClusterDegrade {
			klog.Error("degrading cluster: %s", result.Reason)
			degradeClusterResult = result
		}
		if result.BlockUpgrade {
			klog.Errorf("marking cluster as un-upgradeable: %s", result.Reason)
			blockUpgradeResult = result
		}
	}

	if degradeClusterResult.ClusterDegrade {
		return nextErrorDelay, degradeClusterResult, true
	}

	if blockUpgradeResult.BlockUpgrade {
		return nextErrorDelay, blockUpgradeResult, true
	}

	v.backoff = defaultBackoff
	v.nextCheck = v.lastCheck.Add(defaultBackoff.Cap)
	return defaultBackoff.Cap, checks.MakeClusterCheckResultPass(), true
}
