package vspherecontroller

import (
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
	"time"
)

const maxEventDuration = time.Hour

type warningEventEmitter struct {
	recorder      events.Recorder
	emittedEvents map[checks.CheckStatusType]time.Time
}

func newWarningEventEmitter(recorder events.Recorder) *warningEventEmitter {
	return &warningEventEmitter{
		recorder:      recorder,
		emittedEvents: map[checks.CheckStatusType]time.Time{},
	}
}

func (w *warningEventEmitter) Warn(lastCheckResult checks.ClusterCheckResult) bool {
	eventType := lastCheckResult.CheckStatus
	if lastEventEmitTime, ok := w.emittedEvents[eventType]; ok {
		// if last time we emitted this event is less than maxEventDuration we do not emit the event
		if time.Now().Sub(lastEventEmitTime) < maxEventDuration {
			return false
		}
	}
	w.recorder.Warningf(string(lastCheckResult.CheckStatus), "Marking cluster un-upgradeable because %s", lastCheckResult.Reason)
	w.emittedEvents[eventType] = time.Now()
	return true
}
