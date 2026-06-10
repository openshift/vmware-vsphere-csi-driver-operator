package vclib

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

// ConnectionErrorCategory classifies the type of connection failure.
type ConnectionErrorCategory int

const (
	// CredentialsMissing indicates the secret does not contain credentials for this vCenter.
	CredentialsMissing ConnectionErrorCategory = iota
	// NetworkUnreachable indicates TCP connection to vCenter failed.
	NetworkUnreachable
	// AuthenticationFailed indicates credentials were rejected by vCenter.
	AuthenticationFailed
	// SessionError indicates the REST session could not be established.
	SessionError
)

// ConnectionError is a typed error from ConnectAll, carrying the hostname and failure category.
type ConnectionError struct {
	Host     string
	Category ConnectionErrorCategory
	Err      error
}

func (e *ConnectionError) Error() string {
	return fmt.Sprintf("connection to vCenter %s failed: %v", e.Host, e.Err)
}

func (e *ConnectionError) Unwrap() error {
	return e.Err
}

// VSphereConnectionManager manages connections to multiple vCenters.
type VSphereConnectionManager struct {
	connections map[string]*VSphereConnection // keyed by vCenter hostname
	primary     string                        // hostname of the workspace/primary vCenter
	mu          sync.RWMutex
}

// NewVSphereConnectionManager creates a new empty connection manager.
func NewVSphereConnectionManager() *VSphereConnectionManager {
	return &VSphereConnectionManager{
		connections: make(map[string]*VSphereConnection),
	}
}

// GetConnection returns the connection for the given vCenter hostname, if it exists.
func (m *VSphereConnectionManager) GetConnection(vcenterHost string) (*VSphereConnection, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	conn, ok := m.connections[vcenterHost]
	return conn, ok
}

// GetPrimary returns the connection for the primary (workspace) vCenter.
// Returns nil if no primary is set or the primary is not in the connection map.
func (m *VSphereConnectionManager) GetPrimary() *VSphereConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.primary == "" {
		return nil
	}
	return m.connections[m.primary]
}

// SetPrimary sets the hostname of the primary (workspace) vCenter.
func (m *VSphereConnectionManager) SetPrimary(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.primary = host
}

// PrimaryHost returns the hostname of the primary vCenter.
func (m *VSphereConnectionManager) PrimaryHost() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.primary
}

// AddConnection adds a connection for the given vCenter hostname.
func (m *VSphereConnectionManager) AddConnection(host string, conn *VSphereConnection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connections[host] = conn
}

// ConnectAll connects to all registered vCenters. Returns a slice of ConnectionError for any
// that failed. Connections that succeed are usable even if others fail.
func (m *VSphereConnectionManager) ConnectAll(ctx context.Context) []*ConnectionError {
	m.mu.RLock()
	hosts := make([]string, 0, len(m.connections))
	for h := range m.connections {
		hosts = append(hosts, h)
	}
	m.mu.RUnlock()

	var (
		errs []*ConnectionError
		mu   sync.Mutex
		wg   sync.WaitGroup
	)

	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			m.mu.RLock()
			conn := m.connections[h]
			m.mu.RUnlock()

			if conn == nil {
				mu.Lock()
				errs = append(errs, &ConnectionError{
					Host:     h,
					Category: CredentialsMissing,
					Err:      fmt.Errorf("no connection object for host %s", h),
				})
				mu.Unlock()
				return
			}

			err := conn.Connect(ctx)
			if err != nil {
				category := categorizeConnectionError(err)
				klog.Errorf("Failed to connect to vCenter %s: %v", h, err)
				mu.Lock()
				errs = append(errs, &ConnectionError{
					Host:     h,
					Category: category,
					Err:      err,
				})
				mu.Unlock()
			}
		}(host)
	}

	wg.Wait()
	return errs
}

// LogoutAll logs out from all connected vCenters.
func (m *VSphereConnectionManager) LogoutAll(ctx context.Context) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for host, conn := range m.connections {
		if conn != nil && conn.Client != nil {
			if err := conn.Logout(ctx); err != nil {
				klog.Errorf("Failed to logout from vCenter %s: %v", host, err)
			}
		}
	}
}

// Hosts returns a sorted list of all vCenter hostnames in the manager.
func (m *VSphereConnectionManager) Hosts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	hosts := make([]string, 0, len(m.connections))
	for h := range m.connections {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// Len returns the number of connections in the manager.
func (m *VSphereConnectionManager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connections)
}

// categorizeConnectionError attempts to classify a connection error.
// This is best-effort; errors that don't match known patterns are categorized as NetworkUnreachable.
func categorizeConnectionError(err error) ConnectionErrorCategory {
	if err == nil {
		return NetworkUnreachable
	}
	errStr := err.Error()
	// Check for authentication failures
	if strings.Contains(errStr, "incorrect user name or password") ||
		strings.Contains(errStr, "Cannot complete login") ||
		strings.Contains(errStr, "InvalidLogin") {
		return AuthenticationFailed
	}
	// Check for REST session errors
	if strings.Contains(errStr, "RESTful services") ||
		strings.Contains(errStr, "rest session") {
		return SessionError
	}
	return NetworkUnreachable
}
