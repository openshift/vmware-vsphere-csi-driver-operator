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
	ResetExpBackoff()
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

	overallResult := checks.ClusterCheckResult{
		Action: checks.CheckActionPass,
	}

	// following checks can either block cluster upgrades or degrade the cluster
	// the severity of degradation is higher than blocking upgrades
	for i := range allChecks {
		result := allChecks[i]
		if result.Action >= overallResult.Action {
			overallResult = result
		}
	}

	if overallResult.Action > checks.CheckActionPass {
		// Everything else than pass needs a quicker re-check
		klog.Warningf("Overall check result: %s: %s", checks.ActionToString(overallResult.Action), overallResult.Reason)
		v.nextCheck = v.lastCheck.Add(nextErrorDelay)
		return nextErrorDelay, overallResult, true
	}

	// Slow re-check when everything looks OK
	klog.V(2).Infof("Overall check result: %s: %s", checks.ActionToString(overallResult.Action), overallResult.Reason)
	v.backoff = defaultBackoff
	v.nextCheck = v.lastCheck.Add(defaultBackoff.Cap)
	return defaultBackoff.Cap, checks.MakeClusterCheckResultPass(), true
}

func (v *vSphereEnvironmentCheckerComposite) ResetExpBackoff() {
	v.nextCheck = time.Now()
	v.backoff = defaultBackoff
}
