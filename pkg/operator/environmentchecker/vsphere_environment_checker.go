package environmentchecker

import (
	"context"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/environmentchecker/checks"
	"k8s.io/apimachinery/pkg/util/wait"
	"math"
	"time"
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

type VsphereEnvironmentChecker struct {
	backoff   wait.Backoff
	nextCheck time.Time
	lastCheck time.Time
	checkers  []checks.CheckInterface
}

// make sure that VsphereEnvironmentChecker implements the vSphereEnvironmentCheckInterface
var _ vSphereEnvironmentCheckInterface = &VsphereEnvironmentChecker{}

func newVSphereEnvironmentChecker() *VsphereEnvironmentChecker {
	checker := &VsphereEnvironmentChecker{
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

func (v *VsphereEnvironmentChecker) Check(
	ctx context.Context, checkOpts checks.CheckArgs) (time.Duration, checks.ClusterCheckResult, bool) {
	var delay time.Duration
	var checkResult checks.ClusterCheckResult
	if !time.Now().After(v.nextCheck) {
		return delay, checkResult, false
	}

	nextErrorDelay := v.backoff.Step()
	v.lastCheck = time.Now()

	for _, checker := range v.checkers {
		result := checker.Check(ctx, checkOpts)
		// if the check failed
		if result.CheckError != nil {
			v.nextCheck = v.lastCheck.Add(nextErrorDelay)
			return nextErrorDelay, result, true
		}
	}
	v.backoff = defaultBackoff
	v.nextCheck = v.lastCheck.Add(defaultBackoff.Cap)
	return defaultBackoff.Cap, checks.MakeClusterCheckResultPass(), true
}
