package vspherecontroller

import (
	"context"
	"k8s.io/apimachinery/pkg/runtime"
	"testing"
	"time"

	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vspherecontroller/checks"
)

func TestEnvironmentCheck(t *testing.T) {
	tests := []struct {
		name                   string
		vcenterVersion         string
		checksRan              bool
		result                 checks.CheckStatusType
		initialObjects         []runtime.Object
		configObjects          runtime.Object
		clusterCSIDriverObject *fakeDriverInstance
		expectedBackOffSteps   int
		runCount               int
	}{
		{
			name:                   "when tests are ran successfully, delay should be set to maximum delay",
			initialObjects:         []runtime.Object{getConfigMap(), getSecret()},
			configObjects:          runtime.Object(getInfraObject()),
			clusterCSIDriverObject: makeFakeDriverInstance(),
			vcenterVersion:         "7.0.2",
			result:                 checks.CheckStatusPass,
			checksRan:              true,
			// should reset the steps back to maximum in defaultBackoff
			expectedBackOffSteps: defaultBackoff.Steps,
			runCount:             1,
		},
		{
			name:                   "when tests fail, delay should backoff exponentially",
			initialObjects:         []runtime.Object{getConfigMap(), getSecret()},
			configObjects:          runtime.Object(getInfraObject()),
			clusterCSIDriverObject: makeFakeDriverInstance(),
			vcenterVersion:         "6.5.0",
			result:                 checks.CheckStatusDeprecatedVCenter,
			checksRan:              true,
			expectedBackOffSteps:   defaultBackoff.Steps - 2,
			runCount:               2,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			commonApiClient := newFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)
			stopCh := make(chan struct{})
			defer close(stopCh)

			go startFakeInformer(commonApiClient, stopCh)
			if err := addInitialObjects(test.initialObjects, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}
			waitForSync(commonApiClient, stopCh)

			checker := newVSphereEnvironmentChecker()
			conn, cleanUpFunc, connError := setupSimulator(defaultModel)
			if connError != nil {
				t.Fatalf("unexpected error while connecting to simulator: %v", connError)
			}

			defer func() {
				if cleanUpFunc != nil {
					cleanUpFunc()
				}
			}()

			if test.vcenterVersion != "" {
				customizeVCenterVersion(test.vcenterVersion, test.vcenterVersion, conn)
			}
			csiDriverLister := commonApiClient.KubeInformers.InformersFor("").Storage().V1().CSIDrivers().Lister()
			csiNodeLister := commonApiClient.KubeInformers.InformersFor("").Storage().V1().CSINodes().Lister()
			nodeLister := commonApiClient.NodeInformer.Lister()

			checkerApiClient := &checks.KubeAPIInterfaceImpl{
				Infrastructure:  getInfraObject(),
				CSINodeLister:   csiNodeLister,
				CSIDriverLister: csiDriverLister,
				NodeLister:      nodeLister,
			}
			checkOpts := checks.NewCheckArgs(conn, checkerApiClient)
			var result checks.ClusterCheckResult
			var checkRan bool
			for i := 0; i < test.runCount; i++ {
				_, result, checkRan = checker.Check(context.TODO(), checkOpts)
			}
			if result.CheckStatus != test.result {
				t.Fatalf("expected test status to be %s, got %s", test.result, result.CheckStatus)
			}
			if checkRan != test.checksRan {
				t.Fatalf("expected checkRan to be %v got %v", test.checksRan, checkRan)
			}
			if test.expectedBackOffSteps != checker.backoff.Steps {
				t.Fatalf("expected delay to %v, got %v", test.expectedBackOffSteps, checker.backoff.Steps)
			}
		})
	}
}

// compareTimeDiff checks if two time durations are within Factor duration
func compareTimeDiffWithinTimeFactor(t1, t2 time.Duration) bool {
	allowedTimeFactor := defaultBackoff.Duration - 30*time.Second
	if t1 <= t2 {
		maxTime := time.Duration(float64(t1) + float64(allowedTimeFactor))
		return (t2 < maxTime)
	} else {
		maxTime := time.Duration(float64(t2) + float64(allowedTimeFactor))
		return (t1 < maxTime)
	}
}
