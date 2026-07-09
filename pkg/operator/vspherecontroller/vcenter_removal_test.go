package vspherecontroller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/testlib"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/utils"
	"github.com/openshift/vmware-vsphere-csi-driver-operator/pkg/operator/vclib"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

func TestClassifyConnectError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected connectFailureClass
	}{
		{
			name:     "nil error is transient",
			err:      nil,
			expected: failureClassTransient,
		},
		{
			name:     "connection refused is transient",
			err:      fmt.Errorf("dial tcp 127.0.0.1:1: %w", syscallConnRefused()),
			expected: failureClassTransient,
		},
		{
			name:     "net.Error timeout is transient",
			err:      fmt.Errorf("wrapped: %w", &net.DNSError{IsTimeout: true, Err: "timeout"}),
			expected: failureClassTransient,
		},
		{
			name:     "invalid login is permanent",
			err:      fmt.Errorf("error logging into vcenter: ServerFaultCode: Login failure"),
			expected: failureClassPermanent,
		},
		{
			name:     "incorrect username or password is permanent",
			err:      fmt.Errorf("Incorrect user name or password was specified"),
			expected: failureClassPermanent,
		},
		{
			name:     "unrecognized error defaults to transient",
			err:      fmt.Errorf("something unexpected happened"),
			expected: failureClassTransient,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyConnectError(test.err)
			if got != test.expected {
				t.Errorf("expected %s, got %s", test.expected, got)
			}
		})
	}
}

func syscallConnRefused() error {
	return &net.OpError{Op: "dial", Err: errors.New("connection refused")}
}

func TestRefreshVCenterConfigSnapshots(t *testing.T) {
	initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}
	commonApiClient := testlib.NewFakeClients(initialObjects, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
	if err := testlib.AddInitialObjects(initialObjects, commonApiClient); err != nil {
		t.Fatalf("error adding initial objects: %v", err)
	}
	ctrl := newVsphereController(commonApiClient)

	infra := testlib.GetZonalMultiVCenterInfra()
	cfg, err := ctrl.loadCloudConfig(infra)
	if err != nil {
		t.Fatalf("unexpected error loading cloud config: %v", err)
	}
	ctrl.cloudConfig = cfg

	ctrl.refreshVCenterConfigSnapshots()

	if len(ctrl.vCenterConfigSnapshots) == 0 {
		t.Fatalf("expected snapshots to be populated for active vCenters")
	}

	// Snapshots must survive a host disappearing from the live cloud config on a later sync -
	// refreshVCenterConfigSnapshots must never delete an entry just because it wasn't present in
	// this particular call.
	ctrl.vCenterConfigSnapshots["some-other-host"] = vCenterConnSnapshot{Hostname: "some-other-host"}
	ctrl.refreshVCenterConfigSnapshots()
	if _, ok := ctrl.vCenterConfigSnapshots["some-other-host"]; !ok {
		t.Errorf("expected refreshVCenterConfigSnapshots to not delete entries for hosts absent from the current VirtualCenter map")
	}
}

func TestConnectToVCenterHost(t *testing.T) {
	sim, err := testlib.NewStandaloneSimulator(testlib.DefaultModel)
	if err != nil {
		t.Fatalf("failed to start simulator: %v", err)
	}
	defer sim.Cleanup()

	const host = "removed-vcenter.lan"

	newController := func(secret *runtimeSecret) *VSphereController {
		var objs []runtime.Object
		objs = append(objs, testlib.GetConfigMap())
		if secret != nil {
			objs = append(objs, secret.obj)
		}
		commonApiClient := testlib.NewFakeClients(objs, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetInfraObject()))
		if err := testlib.AddInitialObjects(objs, commonApiClient); err != nil {
			t.Fatalf("error adding initial objects: %v", err)
		}
		ctrl := newVsphereController(commonApiClient)
		return ctrl
	}

	t.Run("no cached snapshot is permanent", func(t *testing.T) {
		ctrl := newController(secretFor(host, sim.Username, sim.Password))
		_, class, err := ctrl.connectToVCenterHost(context.TODO(), host)
		if err == nil {
			t.Fatal("expected error, got none")
		}
		if class != failureClassPermanent {
			t.Errorf("expected permanent failure class, got %s", class)
		}
	})

	t.Run("missing credentials in secret is permanent", func(t *testing.T) {
		// A secret exists, but it has no keys for this particular host.
		ctrl := newController(secretFor("some-other-host.lan", sim.Username, sim.Password))
		ctrl.vCenterConfigSnapshots[host] = vCenterConnSnapshot{Hostname: sim.RealHostname, Insecure: true}
		_, class, err := ctrl.connectToVCenterHost(context.TODO(), host)
		if err == nil {
			t.Fatal("expected error, got none")
		}
		if class != failureClassPermanent {
			t.Errorf("expected permanent failure class, got %s", class)
		}
	})

	t.Run("successful reconnect using cached snapshot+credentials", func(t *testing.T) {
		ctrl := newController(secretFor(host, sim.Username, sim.Password))
		ctrl.vCenterConfigSnapshots[host] = vCenterConnSnapshot{Hostname: sim.RealHostname, Insecure: true}
		conn, _, err := ctrl.connectToVCenterHost(context.TODO(), host)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if conn == nil || conn.Client == nil {
			t.Fatal("expected a connected client")
		}
		defer conn.Logout(context.TODO())
	})

	t.Run("unreachable host is transient", func(t *testing.T) {
		ctrl := newController(secretFor(host, sim.Username, sim.Password))
		// Port 0's listener will be closed immediately below, guaranteeing nothing answers.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to reserve a port: %v", err)
		}
		closedAddr := l.Addr().String()
		l.Close()

		ctrl.vCenterConfigSnapshots[host] = vCenterConnSnapshot{Hostname: closedAddr, Insecure: true}
		_, class, err := ctrl.connectToVCenterHost(context.TODO(), host)
		if err == nil {
			t.Fatal("expected error, got none")
		}
		if class != failureClassTransient {
			t.Errorf("expected transient failure class, got %s", class)
		}
	})
}

type runtimeSecret struct {
	obj runtime.Object
}

func secretFor(host, username, password string) *runtimeSecret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vmware-vsphere-cloud-credentials",
			Namespace: defaultNamespace,
		},
		Data: map[string][]byte{
			host + ".username": []byte(username),
			host + ".password": []byte(password),
		},
	}
	return &runtimeSecret{obj: runtime.Object(secret)}
}

func TestReconcileRemovedVCenters(t *testing.T) {
	newController := func() *VSphereController {
		initialObjects := []runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}
		commonApiClient := testlib.NewFakeClients(initialObjects, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
		if err := testlib.AddInitialObjects(initialObjects, commonApiClient); err != nil {
			t.Fatalf("error adding initial objects: %v", err)
		}
		ctrl := newVsphereController(commonApiClient)
		return ctrl
	}

	t.Run("newly removed host is seeded and attempted the same sync", func(t *testing.T) {
		ctrl := newController()
		ctrl.previousVCenterHosts = map[string]bool{"vcenter.lan": true, "vcenter2.lan": true}
		// No snapshot cached for vcenter2.lan -> connectToVCenterHost fails permanently, but the
		// point of this assertion is that it was *attempted* this sync, not deferred to the next.
		infra := testlib.GetInfraObject() // no VCenters at all -> both hosts look removed
		infra.Spec.PlatformSpec.VSphere.VCenters = []configv1.VSpherePlatformVCenterSpec{
			{Server: "vcenter.lan"},
		}
		ctrl.vCenterConfigSnapshots["vcenter2.lan"] = vCenterConnSnapshot{Hostname: "127.0.0.1:1", Insecure: true}

		ctrl.reconcileRemovedVCenters(context.TODO(), infra)

		state, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]
		if !ok {
			t.Fatalf("expected vcenter2.lan to be pending removal")
		}
		if state.attempts != 1 {
			t.Errorf("expected 1 attempt on the same sync it was detected, got %d", state.attempts)
		}
	})

	t.Run("steady state retries every pending host every sync", func(t *testing.T) {
		ctrl := newController()
		ctrl.previousVCenterHosts = map[string]bool{"vcenter2.lan": true}
		ctrl.pendingVCenterRemoval["vcenter2.lan"] = &vCenterRemovalState{firstDetected: time.Now()}
		ctrl.vCenterConfigSnapshots["vcenter2.lan"] = vCenterConnSnapshot{Hostname: "127.0.0.1:1", Insecure: true}

		infra := testlib.GetInfraObject()
		infra.Spec.PlatformSpec.VSphere.VCenters = nil

		ctrl.reconcileRemovedVCenters(context.TODO(), infra)
		ctrl.reconcileRemovedVCenters(context.TODO(), infra)

		state := ctrl.pendingVCenterRemoval["vcenter2.lan"]
		if state.attempts != 2 {
			t.Errorf("expected 2 attempts after 2 syncs, got %d", state.attempts)
		}
	})

	t.Run("re-add guard cancels pending removal", func(t *testing.T) {
		ctrl := newController()
		ctrl.pendingVCenterRemoval["vcenter2.lan"] = &vCenterRemovalState{firstDetected: time.Now()}
		ctrl.previousVCenterHosts = map[string]bool{"vcenter2.lan": true}

		infra := testlib.GetZonalMultiVCenterInfra() // vcenter2.lan is back

		conns := ctrl.reconcileRemovedVCenters(context.TODO(), infra)

		if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; ok {
			t.Errorf("expected pending removal for vcenter2.lan to be cancelled")
		}
		if len(conns) != 0 {
			t.Errorf("expected no cleanup connections for a re-added host, got %d", len(conns))
		}
	})

	t.Run("permanent failure abandons immediately and purges storage class controller state", func(t *testing.T) {
		ctrl := newController()
		fakeSCC := &dummyStorageClassController{}
		ctrl.storageClassController = fakeSCC
		ctrl.previousVCenterHosts = map[string]bool{"vcenter2.lan": true}
		// No snapshot cached -> connectToVCenterHost returns failureClassPermanent immediately.
		infra := testlib.GetInfraObject()
		infra.Spec.PlatformSpec.VSphere.VCenters = nil

		ctrl.reconcileRemovedVCenters(context.TODO(), infra)

		if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; ok {
			t.Errorf("expected permanent failure to abandon immediately, still pending")
		}
	})

	t.Run("transient failure within window keeps retrying, past window gives up", func(t *testing.T) {
		ctrl := newController()
		infra := testlib.GetInfraObject()
		infra.Spec.PlatformSpec.VSphere.VCenters = nil
		ctrl.vCenterConfigSnapshots["vcenter2.lan"] = vCenterConnSnapshot{Hostname: "127.0.0.1:1", Insecure: true}

		// Still within window.
		ctrl.previousVCenterHosts = map[string]bool{"vcenter2.lan": true}
		ctrl.pendingVCenterRemoval["vcenter2.lan"] = &vCenterRemovalState{firstDetected: time.Now().Add(-1 * time.Hour)}
		ctrl.reconcileRemovedVCenters(context.TODO(), infra)
		if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; !ok {
			t.Fatalf("expected host to still be pending within the transient retry window")
		}

		// Simulate a maintenance-length outage: 10h in, well under the 48h cap.
		ctrl.pendingVCenterRemoval["vcenter2.lan"].firstDetected = time.Now().Add(-10 * time.Hour)
		ctrl.reconcileRemovedVCenters(context.TODO(), infra)
		if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; !ok {
			t.Fatalf("expected a maintenance-length (10h) outage to not be abandoned")
		}

		// Past the window.
		ctrl.pendingVCenterRemoval["vcenter2.lan"].firstDetected = time.Now().Add(-49 * time.Hour)
		ctrl.reconcileRemovedVCenters(context.TODO(), infra)
		if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; ok {
			t.Errorf("expected host to be abandoned past maxTransientRetryWindow")
		}
	})

	t.Run("primary workspace vCenter is never auto-reconnected", func(t *testing.T) {
		ctrl := newController()
		cfg, err := ctrl.loadCloudConfig(testlib.GetInfraObject())
		if err != nil {
			t.Fatalf("unexpected error loading cloud config: %v", err)
		}
		ctrl.cloudConfig = cfg // legacy config -> Workspace.VCenterIP == "localhost"

		ctrl.previousVCenterHosts = map[string]bool{"localhost": true}
		infra := testlib.GetInfraObject()
		infra.Spec.PlatformSpec.VSphere.VCenters = nil

		ctrl.reconcileRemovedVCenters(context.TODO(), infra)

		if _, ok := ctrl.pendingVCenterRemoval["localhost"]; ok {
			t.Errorf("expected primary/workspace vCenter to never enter pendingVCenterRemoval")
		}
	})
}

// day2Gates returns a FeatureGate with FeatureGateVSphereMultiVCenterDay2 enabled, so
// createVCenterConnection/loginToVCenter exercise their Phase 6 fault-isolation branch instead
// of preserving pre-Day2 "abort on first error" behavior.
func day2Gates() featuregates.FeatureGate {
	return featuregates.NewFeatureGate(
		[]configv1.FeatureGateName{features.FeatureGateVSphereMultiVCenterDay2},
		[]configv1.FeatureGateName{"SomeDisabledFeatureGate"},
	)
}

// multiVCenterConfigMapWithAddrs builds a "cloud-provider-config" ConfigMap matching
// testlib.GetZonalMultiVCenterInfra's two vCenters ("vcenter.lan", "vcenter2.lan"), but pointing
// each at an attacker^Wtest-controlled address instead of a DNS name, so Connect() attempts in
// these tests fail (or succeed, against vcsim) deterministically instead of doing a real DNS
// lookup for a host that doesn't exist.
func multiVCenterConfigMapWithAddrs(addr1, addr2 string) *corev1.ConfigMap {
	config := fmt.Sprintf(`
[Global]
secret-name = "vsphere-creds"
secret-namespace = "kube-system"
insecure-flag = "1"

[Workspace]
server = "localhost"
datacenter = "DC0"
default-datastore = "LocalDS_0"
folder = "/DC0/vm"

[VirtualCenter "vcenter.lan"]
server = %q
datacenters = "DC0"

[VirtualCenter "vcenter2.lan"]
server = %q
datacenters = "DC1"
`, addr1, addr2)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cloud-provider-config",
			Namespace: cloudConfigNamespace,
		},
		Data: map[string]string{"config": config},
	}
}

// closedPort returns the address of a TCP port that is guaranteed to refuse connections:
// reserved, then immediately released, so nothing can be listening on it.
func closedPort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve a port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// TestCreateVCenterConnectionFaultIsolation covers Phase 6: with Day2 enabled and more than one
// configured vCenter, a credential problem on a non-workspace vCenter must be isolated into the
// returned failures slice instead of aborting the whole call - so the other, healthy vCenter
// still gets a connection.
func TestCreateVCenterConnectionFaultIsolation(t *testing.T) {
	infra := testlib.GetZonalMultiVCenterInfra() // workspace host is "localhost"; neither vcenter.lan nor vcenter2.lan is critical.

	newController := func(gates featuregates.FeatureGate, secret *corev1.Secret) *VSphereController {
		// Any two distinct addresses are fine here: this test never calls Connect(), it only
		// needs GetVCenterHostname(vcenter.lan/vcenter2.lan) to resolve.
		objs := []runtime.Object{multiVCenterConfigMapWithAddrs("vcenter.lan:443", "vcenter2.lan:443"), runtime.Object(secret)}
		commonApiClient := testlib.NewFakeClients(objs, testlib.MakeFakeDriverInstance(), runtime.Object(infra))
		if err := testlib.AddInitialObjects(objs, commonApiClient); err != nil {
			t.Fatalf("error adding initial objects: %v", err)
		}
		ctrl := newVsphereControllerWithGates(commonApiClient, gates)
		cfg, err := ctrl.loadCloudConfig(infra)
		if err != nil {
			t.Fatalf("unexpected error loading cloud config: %v", err)
		}
		ctrl.cloudConfig = cfg
		return ctrl
	}

	// A secret with credentials only for vcenter.lan; vcenter2.lan's keys are missing entirely.
	partialSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vmware-vsphere-cloud-credentials", Namespace: defaultNamespace},
		Data: map[string][]byte{
			"vcenter.lan.username": []byte("user"),
			"vcenter.lan.password": []byte("pass"),
		},
	}

	t.Run("day2 enabled isolates the failure and keeps the healthy vCenter", func(t *testing.T) {
		ctrl := newController(day2Gates(), partialSecret)
		failures, err := ctrl.createVCenterConnection(context.TODO(), infra)
		if err != nil {
			t.Fatalf("expected no immediate error, got %v", err)
		}
		if len(failures) != 1 || failures[0].host != "vcenter2.lan" {
			t.Fatalf("expected exactly one isolated failure for vcenter2.lan, got %+v", failures)
		}
		// Hostname holds the resolved VCenterIP from the cloud config (here, "vcenter.lan:443"),
		// not the vcenter.Server key used to look it up.
		if len(ctrl.vSphereConnections) != 1 || ctrl.vSphereConnections[0].Hostname != "vcenter.lan:443" {
			t.Fatalf("expected a connection to still be created for the healthy vcenter.lan, got %+v", ctrl.vSphereConnections)
		}
	})

	t.Run("day2 disabled preserves abort-on-first-error behavior", func(t *testing.T) {
		gates := featuregates.NewFeatureGate(
			[]configv1.FeatureGateName{"SomeEnabledFeatureGate"},
			[]configv1.FeatureGateName{features.FeatureGateVSphereMultiVCenterDay2},
		)
		ctrl := newController(gates, partialSecret)
		_, err := ctrl.createVCenterConnection(context.TODO(), infra)
		if err == nil {
			t.Fatalf("expected an immediate error preserving pre-Day2 behavior")
		}
	})
}

// TestLoginToVCenterFaultIsolation covers Phase 6 end-to-end through loginToVCenter: a secondary
// vCenter that is unreachable at the network level must not degrade the cluster, but must be
// surfaced via secondaryVCenterUnreachable/secondaryVCenterMessage, while the workspace/critical
// vCenter's unreachability still degrades as before.
func TestLoginToVCenterFaultIsolation(t *testing.T) {
	infra := testlib.GetZonalMultiVCenterInfra()

	newController := func(gates featuregates.FeatureGate, configMap *corev1.ConfigMap) *VSphereController {
		objs := []runtime.Object{configMap, testlib.GetMultiVCenterSecret()}
		commonApiClient := testlib.NewFakeClients(objs, testlib.MakeFakeDriverInstance(), runtime.Object(infra))
		if err := testlib.AddInitialObjects(objs, commonApiClient); err != nil {
			t.Fatalf("error adding initial objects: %v", err)
		}
		ctrl := newVsphereControllerWithGates(commonApiClient, gates)
		cfg, err := ctrl.loadCloudConfig(infra)
		if err != nil {
			t.Fatalf("unexpected error loading cloud config: %v", err)
		}
		ctrl.cloudConfig = cfg
		return ctrl
	}

	t.Run("secondary vCenter unreachable is isolated, not degraded", func(t *testing.T) {
		// vcenter.lan reachable via vcsim, vcenter2.lan points at a closed port.
		sim, err := testlib.NewStandaloneSimulator(testlib.DefaultModel)
		if err != nil {
			t.Fatalf("failed to start simulator: %v", err)
		}
		defer sim.Cleanup()

		configMap := multiVCenterConfigMapWithAddrs(sim.RealHostname, closedPort(t))
		ctrl := newController(day2Gates(), configMap)

		result := ctrl.loginToVCenter(context.TODO(), infra)
		if result.CheckError != nil {
			t.Fatalf("expected no degrading error, got: %v", result.CheckError)
		}
		if !ctrl.secondaryVCenterUnreachable {
			t.Errorf("expected secondaryVCenterUnreachable to be true")
		}
		if ctrl.secondaryVCenterMessage == "" {
			t.Errorf("expected a non-empty secondaryVCenterMessage")
		}
		if len(ctrl.vSphereConnections) != 1 || ctrl.vSphereConnections[0].Hostname != sim.RealHostname {
			t.Errorf("expected only the healthy connection to remain, got %+v", ctrl.vSphereConnections)
		}
	})

	t.Run("all vCenters unreachable still degrades", func(t *testing.T) {
		configMap := multiVCenterConfigMapWithAddrs(closedPort(t), closedPort(t))
		ctrl := newController(day2Gates(), configMap)

		result := ctrl.loginToVCenter(context.TODO(), infra)
		if result.CheckError == nil {
			t.Fatalf("expected a degrading error when every configured vCenter is unreachable")
		}
		if ctrl.secondaryVCenterUnreachable {
			t.Errorf("expected secondaryVCenterUnreachable to be false when there is no healthy vCenter to fall back on")
		}
	})
}

func TestFinalizeCleanupStateAndLogout(t *testing.T) {
	commonApiClient := testlib.NewFakeClients([]runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
	ctrl := newVsphereController(commonApiClient)
	fakeSCC := &dummyStorageClassController{fullyCleanHosts: map[string]bool{"vcenter2.lan": true}}
	ctrl.storageClassController = fakeSCC
	ctrl.pendingVCenterRemoval["vcenter2.lan"] = &vCenterRemovalState{firstDetected: time.Now(), attempts: 3}
	ctrl.vCenterConfigSnapshots["vcenter2.lan"] = vCenterConnSnapshot{Hostname: "vcenter2.lan"}

	conn := &vclib.VSphereConnection{Hostname: "vcenter2.lan"}
	ctrl.finalizeCleanupState([]*vclib.VSphereConnection{conn})

	if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; ok {
		t.Errorf("expected pending removal to be cleared once IsHostFullyClean is true")
	}
	if _, ok := ctrl.vCenterConfigSnapshots["vcenter2.lan"]; ok {
		t.Errorf("expected config snapshot to be cleared once IsHostFullyClean is true")
	}
}

// TestUpdateVCenterRemovalPendingCondition covers Phase 7's condition: True while any host has
// attempts > 0 in pendingVCenterRemoval (i.e. an in-progress cleanup we haven't finished),
// False/absent once nothing is pending.
func TestUpdateVCenterRemovalPendingCondition(t *testing.T) {
	commonApiClient := testlib.NewFakeClients([]runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
	ctrl := newVsphereController(commonApiClient)

	if err := ctrl.updateVCenterRemovalPendingCondition(context.TODO()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond := getCondition(t, ctrl, conditionVCenterRemovalPending)
	if cond.Status != operatorapi.ConditionFalse {
		t.Fatalf("expected condition to be False with nothing pending, got %s", cond.Status)
	}

	// A host that was just seeded (attempts == 0, i.e. attemptHost hasn't run yet this sync)
	// must not flip the condition true - it isn't a confirmed pending cleanup yet.
	ctrl.pendingVCenterRemoval["vcenter2.lan"] = &vCenterRemovalState{firstDetected: time.Now()}
	if err := ctrl.updateVCenterRemovalPendingCondition(context.TODO()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond = getCondition(t, ctrl, conditionVCenterRemovalPending)
	if cond.Status != operatorapi.ConditionFalse {
		t.Errorf("expected condition to stay False for a host with 0 attempts, got %s", cond.Status)
	}

	ctrl.pendingVCenterRemoval["vcenter2.lan"].attempts = 2
	ctrl.pendingVCenterRemoval["vcenter2.lan"].lastError = fmt.Errorf("boom")
	if err := ctrl.updateVCenterRemovalPendingCondition(context.TODO()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond = getCondition(t, ctrl, conditionVCenterRemovalPending)
	if cond.Status != operatorapi.ConditionTrue {
		t.Fatalf("expected condition to be True once a host has attempts > 0, got %s", cond.Status)
	}
	for _, want := range []string{"vcenter2.lan", "attempts=2", "boom"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("expected condition message %q to contain %q", cond.Message, want)
		}
	}

	delete(ctrl.pendingVCenterRemoval, "vcenter2.lan")
	if err := ctrl.updateVCenterRemovalPendingCondition(context.TODO()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond = getCondition(t, ctrl, conditionVCenterRemovalPending)
	if cond.Status != operatorapi.ConditionFalse {
		t.Errorf("expected condition to go back to False once cleanup completes, got %s", cond.Status)
	}
}

func getCondition(t *testing.T, ctrl *VSphereController, conditionSuffix string) *operatorapi.OperatorCondition {
	t.Helper()
	_, status, _, err := ctrl.operatorClient.GetOperatorState()
	if err != nil {
		t.Fatalf("failed to get operator state: %v", err)
	}
	cond := testlib.GetMatchingCondition(status.Conditions, testControllerName+conditionSuffix)
	if cond == nil {
		t.Fatalf("expected condition %q to be set", testControllerName+conditionSuffix)
	}
	return cond
}

// TestUpdateSecondaryVCenterCondition covers Phase 6/7's fault-isolation condition: True with
// secondaryVCenterMessage as its message while secondaryVCenterUnreachable is set, False once
// cleared.
func TestUpdateSecondaryVCenterCondition(t *testing.T) {
	commonApiClient := testlib.NewFakeClients([]runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
	ctrl := newVsphereController(commonApiClient)

	if err := ctrl.updateSecondaryVCenterCondition(context.TODO()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond := getCondition(t, ctrl, conditionSecondaryVCenterUnrch)
	if cond.Status != operatorapi.ConditionFalse {
		t.Fatalf("expected condition to be False by default, got %s", cond.Status)
	}

	ctrl.secondaryVCenterUnreachable = true
	ctrl.secondaryVCenterMessage = "vCenter vcenter2.lan is unreachable: boom"
	if err := ctrl.updateSecondaryVCenterCondition(context.TODO()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond = getCondition(t, ctrl, conditionSecondaryVCenterUnrch)
	if cond.Status != operatorapi.ConditionTrue || cond.Message != ctrl.secondaryVCenterMessage {
		t.Errorf("expected condition True with the secondary vCenter message, got status=%s message=%q", cond.Status, cond.Message)
	}

	ctrl.secondaryVCenterUnreachable = false
	ctrl.secondaryVCenterMessage = ""
	if err := ctrl.updateSecondaryVCenterCondition(context.TODO()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cond = getCondition(t, ctrl, conditionSecondaryVCenterUnrch)
	if cond.Status != operatorapi.ConditionFalse {
		t.Errorf("expected condition to go back to False once all vCenters are reachable, got %s", cond.Status)
	}
}

// TestEvaluateGiveUpFiresAbandonedEventAndMetric covers Phase 7's abandonment path: the
// VCenterCleanupAbandoned event fires exactly once and the
// vsphere_csi_vcenter_removal_cleanup_total{result="abandoned"} metric increments by 1.
func TestEvaluateGiveUpFiresAbandonedEventAndMetric(t *testing.T) {
	legacyregistry.Reset()
	utils.VCenterRemovalCleanupTotal.Reset()

	commonApiClient := testlib.NewFakeClients([]runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
	ctrl := newVsphereController(commonApiClient)
	recorder := ctrl.eventRecorder.(events.InMemoryRecorder)

	state := &vCenterRemovalState{firstDetected: time.Now(), attempts: 5, lastClass: failureClassPermanent, lastError: fmt.Errorf("bad credentials")}
	ctrl.pendingVCenterRemoval["vcenter2.lan"] = state

	ctrl.evaluateGiveUp("vcenter2.lan", state)

	if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; ok {
		t.Errorf("expected host to be removed from pendingVCenterRemoval after giving up")
	}
	if !hasEvent(recorder, eventVCenterCleanupAbandoned) {
		t.Errorf("expected a %s event to be recorded", eventVCenterCleanupAbandoned)
	}

	expected := `
		# HELP vsphere_csi_vcenter_removal_cleanup_total [ALPHA] Total number of best-effort cleanup attempts for removed vCenters, by result
		# TYPE vsphere_csi_vcenter_removal_cleanup_total counter
		vsphere_csi_vcenter_removal_cleanup_total{result="abandoned"} 1
	`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), "vsphere_csi_vcenter_removal_cleanup_total"); err != nil {
		t.Errorf("wrong metrics: %s", err)
	}
}

// TestFinalizeCleanupStateFiresSuccessEventAndMetricOnce covers both the success path's
// event/metric, and Phase 7's idempotency requirement: calling finalizeCleanupState again for a
// host that's no longer pending (already finalized) must not fire a duplicate event or
// increment the metric again - the "re-run against an already-cleaned host is a no-op" case,
// e.g. after an operator pod restart harmlessly re-attempts cleanup on an already-clean host.
func TestFinalizeCleanupStateFiresSuccessEventAndMetricOnce(t *testing.T) {
	legacyregistry.Reset()
	utils.VCenterRemovalCleanupTotal.Reset()

	commonApiClient := testlib.NewFakeClients([]runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
	ctrl := newVsphereController(commonApiClient)
	recorder := ctrl.eventRecorder.(events.InMemoryRecorder)
	fakeSCC := &dummyStorageClassController{fullyCleanHosts: map[string]bool{"vcenter2.lan": true}}
	ctrl.storageClassController = fakeSCC
	ctrl.pendingVCenterRemoval["vcenter2.lan"] = &vCenterRemovalState{firstDetected: time.Now(), attempts: 2}

	conn := &vclib.VSphereConnection{Hostname: "vcenter2.lan"}
	ctrl.finalizeCleanupState([]*vclib.VSphereConnection{conn})

	if _, ok := ctrl.pendingVCenterRemoval["vcenter2.lan"]; ok {
		t.Fatalf("expected host to be cleared from pendingVCenterRemoval")
	}
	successEvents := countEvents(recorder, eventVCenterCleanupSucceeded)
	if successEvents != 1 {
		t.Fatalf("expected exactly 1 %s event, got %d", eventVCenterCleanupSucceeded, successEvents)
	}

	// Re-run: host is no longer pending (as if a fresh pod re-detected an already-clean
	// vCenter). Must be a true no-op: no error, no additional event, no metric bump.
	ctrl.finalizeCleanupState([]*vclib.VSphereConnection{conn})

	successEvents = countEvents(recorder, eventVCenterCleanupSucceeded)
	if successEvents != 1 {
		t.Errorf("expected still exactly 1 %s event after re-running against an already-clean host, got %d", eventVCenterCleanupSucceeded, successEvents)
	}

	expected := `
		# HELP vsphere_csi_vcenter_removal_cleanup_total [ALPHA] Total number of best-effort cleanup attempts for removed vCenters, by result
		# TYPE vsphere_csi_vcenter_removal_cleanup_total counter
		vsphere_csi_vcenter_removal_cleanup_total{result="success"} 1
	`
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), "vsphere_csi_vcenter_removal_cleanup_total"); err != nil {
		t.Errorf("wrong metrics: %s", err)
	}
}

// TestReconcileRemovedVCentersFiresCleanupStartedEvent covers the VCenterCleanupStarted event:
// it must fire exactly once, on the sync a removed host is first detected/seeded - not on every
// subsequent retry of the same host.
func TestReconcileRemovedVCentersFiresCleanupStartedEvent(t *testing.T) {
	commonApiClient := testlib.NewFakeClients([]runtime.Object{testlib.GetConfigMap(), testlib.GetMultiVCenterSecret()}, testlib.MakeFakeDriverInstance(), runtime.Object(testlib.GetZonalMultiVCenterInfra()))
	ctrl := newVsphereController(commonApiClient)
	recorder := ctrl.eventRecorder.(events.InMemoryRecorder)

	ctrl.previousVCenterHosts = map[string]bool{"vcenter2.lan": true}
	ctrl.vCenterConfigSnapshots["vcenter2.lan"] = vCenterConnSnapshot{Hostname: "127.0.0.1:1", Insecure: true}
	infra := testlib.GetInfraObject()
	infra.Spec.PlatformSpec.VSphere.VCenters = nil

	ctrl.reconcileRemovedVCenters(context.TODO(), infra)
	ctrl.reconcileRemovedVCenters(context.TODO(), infra)

	if got := countEvents(recorder, eventVCenterCleanupStarted); got != 1 {
		t.Errorf("expected exactly 1 %s event across 2 syncs of the same pending host, got %d", eventVCenterCleanupStarted, got)
	}
}

func hasEvent(recorder events.InMemoryRecorder, reason string) bool {
	return countEvents(recorder, reason) > 0
}

func countEvents(recorder events.InMemoryRecorder, reason string) int {
	count := 0
	for _, e := range recorder.Events() {
		if e.Reason == reason {
			count++
		}
	}
	return count
}