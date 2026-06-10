package vclib

import (
	"context"
	"fmt"
	"testing"
)

func TestNewVSphereConnectionManager(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
	if mgr.Len() != 0 {
		t.Errorf("expected 0 connections, got %d", mgr.Len())
	}
	if mgr.GetPrimary() != nil {
		t.Error("expected nil primary")
	}
}

func TestAddAndGetConnection(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	conn := &VSphereConnection{Hostname: "vcenter1.example.com"}

	mgr.AddConnection("vcenter1.example.com", conn)

	got, ok := mgr.GetConnection("vcenter1.example.com")
	if !ok {
		t.Fatal("expected connection to exist")
	}
	if got != conn {
		t.Error("returned connection does not match added connection")
	}

	_, ok = mgr.GetConnection("vcenter2.example.com")
	if ok {
		t.Error("expected connection to not exist")
	}
}

func TestSetAndGetPrimary(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	conn1 := &VSphereConnection{Hostname: "vcenter1.example.com"}
	conn2 := &VSphereConnection{Hostname: "vcenter2.example.com"}

	mgr.AddConnection("vcenter1.example.com", conn1)
	mgr.AddConnection("vcenter2.example.com", conn2)
	mgr.SetPrimary("vcenter1.example.com")

	primary := mgr.GetPrimary()
	if primary != conn1 {
		t.Error("expected primary to be vcenter1")
	}
	if mgr.PrimaryHost() != "vcenter1.example.com" {
		t.Errorf("expected primary host vcenter1.example.com, got %s", mgr.PrimaryHost())
	}
}

func TestGetPrimaryNotSet(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	conn := &VSphereConnection{Hostname: "vcenter1.example.com"}
	mgr.AddConnection("vcenter1.example.com", conn)

	if mgr.GetPrimary() != nil {
		t.Error("expected nil primary when not set")
	}
}

func TestGetPrimaryNotInConnections(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	conn := &VSphereConnection{Hostname: "vcenter1.example.com"}
	mgr.AddConnection("vcenter1.example.com", conn)
	mgr.SetPrimary("vcenter-missing.example.com")

	if mgr.GetPrimary() != nil {
		t.Error("expected nil when primary is not in connections map")
	}
}

func TestHostsSorted(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	mgr.AddConnection("zcenter.example.com", &VSphereConnection{Hostname: "zcenter.example.com"})
	mgr.AddConnection("acenter.example.com", &VSphereConnection{Hostname: "acenter.example.com"})
	mgr.AddConnection("mcenter.example.com", &VSphereConnection{Hostname: "mcenter.example.com"})

	hosts := mgr.Hosts()
	if len(hosts) != 3 {
		t.Fatalf("expected 3 hosts, got %d", len(hosts))
	}
	expected := []string{"acenter.example.com", "mcenter.example.com", "zcenter.example.com"}
	for i, h := range hosts {
		if h != expected[i] {
			t.Errorf("host %d: expected %s, got %s", i, expected[i], h)
		}
	}
}

func TestLen(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	if mgr.Len() != 0 {
		t.Errorf("expected 0, got %d", mgr.Len())
	}
	mgr.AddConnection("a", &VSphereConnection{})
	if mgr.Len() != 1 {
		t.Errorf("expected 1, got %d", mgr.Len())
	}
	mgr.AddConnection("b", &VSphereConnection{})
	if mgr.Len() != 2 {
		t.Errorf("expected 2, got %d", mgr.Len())
	}
}

func TestCategorizeConnectionError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected ConnectionErrorCategory
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: NetworkUnreachable,
		},
		{
			name:     "authentication failure - incorrect password",
			err:      fmt.Errorf("incorrect user name or password"),
			expected: AuthenticationFailed,
		},
		{
			name:     "authentication failure - cannot complete login",
			err:      fmt.Errorf("Cannot complete login due to an incorrect user name or password"),
			expected: AuthenticationFailed,
		},
		{
			name:     "authentication failure - InvalidLogin",
			err:      fmt.Errorf("ServerFaultCode: InvalidLogin"),
			expected: AuthenticationFailed,
		},
		{
			name:     "REST session error",
			err:      fmt.Errorf("error logging into vcenter RESTful services: 401"),
			expected: SessionError,
		},
		{
			name:     "network error",
			err:      fmt.Errorf("dial tcp: connection refused"),
			expected: NetworkUnreachable,
		},
		{
			name:     "unknown error defaults to network",
			err:      fmt.Errorf("something unexpected happened"),
			expected: NetworkUnreachable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := categorizeConnectionError(tc.err)
			if got != tc.expected {
				t.Errorf("expected category %d, got %d", tc.expected, got)
			}
		})
	}
}

func TestConnectionErrorUnwrap(t *testing.T) {
	inner := fmt.Errorf("inner error")
	ce := &ConnectionError{
		Host:     "test-host",
		Category: AuthenticationFailed,
		Err:      inner,
	}

	if ce.Unwrap() != inner {
		t.Error("Unwrap should return inner error")
	}
	if ce.Error() == "" {
		t.Error("Error() should return non-empty string")
	}
}

// TestLogoutAllNilClients verifies LogoutAll does not panic when connections have nil clients.
func TestLogoutAllNilClients(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	mgr.AddConnection("a", &VSphereConnection{Hostname: "a"})
	mgr.AddConnection("b", &VSphereConnection{Hostname: "b"})

	// Should not panic
	mgr.LogoutAll(context.Background())
}

// TestConnectAllNoConnections verifies ConnectAll with empty manager returns no errors.
func TestConnectAllNoConnections(t *testing.T) {
	mgr := NewVSphereConnectionManager()
	errs := mgr.ConnectAll(context.Background())
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errs))
	}
}
