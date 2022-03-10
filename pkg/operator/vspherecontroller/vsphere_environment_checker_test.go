package vspherecontroller

import (
	"context"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

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
		clusterCSIDriverObject *testlib.FakeDriverInstance
		expectedBackOffSteps   int
		expectedNextCheck      time.Time
		runCount               int
	}{
		{
			name:                   "when tests are ran successfully, delay should be set to maximum delay",
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			vcenterVersion:         "7.0.2",
			result:                 checks.CheckStatusPass,
			checksRan:              true,
			// should reset the steps back to maximum in defaultBackoff
			expectedBackOffSteps: defaultBackoff.Steps,
			expectedNextCheck:    time.Now().Add(defaultBackoff.Cap),
			runCount:             1,
		},
		{
			name:                   "when tests fail, delay should backoff exponentially",
			initialObjects:         []runtime.Object{testlib.GetConfigMap(), testlib.GetSecret()},
			configObjects:          runtime.Object(testlib.GetInfraObject()),
			clusterCSIDriverObject: testlib.MakeFakeDriverInstance(),
			vcenterVersion:         "6.5.0",
			result:                 checks.CheckStatusDeprecatedVCenter,
			checksRan:              true,
			expectedBackOffSteps:   defaultBackoff.Steps - 1,
			expectedNextCheck:      time.Now().Add(1 * time.Minute),
			runCount:               1,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			commonApiClient := testlib.NewFakeClients(test.initialObjects, test.clusterCSIDriverObject, test.configObjects)
			stopCh := make(chan struct{})
			defer close(stopCh)

			go testlib.StartFakeInformer(commonApiClient, stopCh)
			if err := testlib.AddInitialObjects(test.initialObjects, commonApiClient); err != nil {
				t.Fatalf("error adding initial objects: %v", err)
			}
			testlib.WaitForSync(commonApiClient, stopCh)

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
			// add a sleep so as we can calculate nextCheck accurately
			time.Sleep(5 * time.Second)

			if test.vcenterVersion != "" {
				customizeVCenterVersion(test.vcenterVersion, test.vcenterVersion, conn)
			}
			csiDriverLister := commonApiClient.KubeInformers.InformersFor("").Storage().V1().CSIDrivers().Lister()
			csiNodeLister := commonApiClient.KubeInformers.InformersFor("").Storage().V1().CSINodes().Lister()
			nodeLister := commonApiClient.NodeInformer.Lister()

			checkerApiClient := &checks.KubeAPIInterfaceImpl{
				Infrastructure:  testlib.GetInfraObject(),
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
			if checkRan != test.checksRan {
				t.Fatalf("expected checkRan to be %v got %v", test.checksRan, checkRan)
			}
			if result.CheckStatus != test.result {
				t.Fatalf("expected test status to be %s, got %s", test.result, result.CheckStatus)
			}
			if test.expectedBackOffSteps != checker.backoff.Steps {
				t.Fatalf("expected delay to %v, got %v", test.expectedBackOffSteps, checker.backoff.Steps)
			}
			if !checker.nextCheck.After(test.expectedNextCheck) {
				t.Fatalf("expected nextCheck %v to be after expectedNextCheck %v", checker.nextCheck, test.expectedNextCheck)
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
