// Package routes manages system routes for other nodes' pod CIDRs via their
// Tailscale IP. On Linux it uses netlink; on other platforms it no-ops.
package routes

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"syscall"
)

// Manager adds and removes routes so traffic to other nodes' pod CIDRs goes
// via the Tailscale IP of that node.
type Manager struct {
	mu              sync.Mutex
	routes          map[string]string // cidr -> viaIP (routes we've added)
	tailscaleIface  string            // interface name for LinkIndex (e.g. tailscale0)
}

// NewManager returns a route manager. tailscaleIface is the Tailscale interface
// name (e.g. "tailscale0"); routes are added with that interface so the kernel
// can reach the gateway. Use "" to not set an output interface.
func NewManager(tailscaleIface string) *Manager {
	return &Manager{routes: make(map[string]string), tailscaleIface: tailscaleIface}
}

// EnsureRoutes makes the system route table match desired: desired[cidr] = viaIP.
// It adds missing routes and deletes routes we previously added that are no longer in desired.
func (m *Manager) EnsureRoutes(desired map[string]string) error {
	m.mu.Lock()
	current := make(map[string]string)
	for cidr, via := range m.routes {
		current[cidr] = via
	}
	m.mu.Unlock()

	// Add new, update changed, remove stale
	for cidr, via := range desired {
		if cur, ok := current[cidr]; !ok || cur != via {
			if err := m.addRoute(cidr, via); err != nil {
				if isNetworkUnreachable(err) {
					log.Printf("routes: skipping %s via %s (gateway unreachable; will retry)", cidr, via)
					continue
				}
				return fmt.Errorf("add route %s via %s: %w", cidr, via, err)
			}
			m.mu.Lock()
			m.routes[cidr] = via
			m.mu.Unlock()
			delete(current, cidr)
		} else {
			delete(current, cidr)
		}
	}
	for cidr := range current {
		if err := m.delRoute(cidr); err != nil {
			return fmt.Errorf("del route %s: %w", cidr, err)
		}
		m.mu.Lock()
		delete(m.routes, cidr)
		m.mu.Unlock()
	}
	return nil
}

// addRoute adds a route for cidr via the given gateway (platform-specific).
func (m *Manager) addRoute(cidr, via string) error {
	return addRoute(cidr, via, m.tailscaleIface)
}

// delRoute removes the route for cidr (platform-specific).
func (m *Manager) delRoute(cidr string) error {
	return delRoute(cidr)
}

// isNetworkUnreachable reports whether err is ENETUNREACH (gateway not reachable).
func isNetworkUnreachable(err error) bool {
	if errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "network is unreachable")
}
