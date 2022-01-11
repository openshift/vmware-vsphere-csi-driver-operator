package vspherecontroller

import (
	"fmt"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"testing"
	"time"
)

func TestEventEmission(t *testing.T) {
	tests := []struct {
		name           string
		oldEventType   checks.CheckStatusType
		oldTimeStamp   time.Time
		emissionStatus bool
		result         checks.ClusterCheckResult
	}{
		{
			name:           "when no events of that type are emitted",
			oldEventType:   "",
			emissionStatus: true,
			result:         getTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
		},
		{
			name:           "when old recent event of same type already exists",
			oldEventType:   checks.CheckStatusVSphereConnectionFailed,
			oldTimeStamp:   time.Now().Add(-5 * time.Minute),
			emissionStatus: false,
			result:         getTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
		},
		{
			name:           "when old but expired event of same type already exists",
			oldEventType:   checks.CheckStatusVSphereConnectionFailed,
			oldTimeStamp:   time.Now().Add(-2 * time.Hour),
			emissionStatus: true,
			result:         getTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
		},
		{
			name:           "when old but somewhat recent event of same type already exists",
			oldEventType:   checks.CheckStatusVSphereConnectionFailed,
			oldTimeStamp:   time.Now().Add(-59 * time.Minute),
			emissionStatus: false,
			result:         getTestClusterResult(checks.CheckStatusVSphereConnectionFailed),
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			rc := events.NewInMemoryRecorder(testControllerName)
			emitter := newWarningEventEmitter(rc)
			if test.oldEventType != "" {
				emitter.emittedEvents[test.oldEventType] = test.oldTimeStamp
			}

			oldTimeStamp := time.Now()

			if test.emissionStatus {
				oldTimeStamp = emitter.emittedEvents[test.result.CheckStatus]
			}

			emissionStatus := emitter.Warn(test.result)
			if emissionStatus != test.emissionStatus {
				t.Fatalf("expected event emission to be: %v, got %v", test.emissionStatus, emissionStatus)
			}
			// if event was emitted the timestamp must be updated
			if test.emissionStatus {
				newTimeStamp, ok := emitter.emittedEvents[test.result.CheckStatus]
				if !ok {
					t.Fatalf("expected emission record for event: %s", test.result.CheckStatus)
				}
				if !newTimeStamp.After(oldTimeStamp) {
					t.Fatalf("expected new timestamp %q to be after %q", newTimeStamp, oldTimeStamp)
				}
			}
		})
	}
}

func getTestClusterResult(statusType checks.CheckStatusType) checks.ClusterCheckResult {
	return checks.ClusterCheckResult{
		CheckError:  fmt.Errorf("some error"),
		CheckStatus: statusType,
	}
}
