// Package firewall manages the tailscale-cni nftables table on the node.
//
// It does two things:
//
//   - NAT: traffic from our pods to other pods keeps its pod IP. Everything
//     else leaving the node (internet, or non-cluster tailnet devices) is
//     masqueraded as the node, so the pod network looks like a private network
//     behind the host.
//   - Ingress isolation (optional): traffic entering the pod bridge from
//     outside the node is dropped unless it comes from another pod, another
//     cluster node, an allowlisted CIDR, is a reply to a connection a pod
//     started, or was DNAT'd by a Service/hostPort (kube-proxy, portmap).
package firewall

import (
	"fmt"
	"net/netip"
	"slices"
	"sync"
)

// Config is the static part of the firewall setup.
type Config struct {
	ClusterCIDR    netip.Prefix // all pod CIDRs in the cluster
	Bridge         string       // pod bridge, e.g. cni0
	TailscaleIface string       // e.g. tailscale0

	// IngressIsolation drops traffic into pods from outside the cluster.
	IngressIsolation bool
	// AllowedSources may reach pods from any interface when isolation is on.
	AllowedSources []netip.Prefix
}

// state is everything the table is built from.
type state struct {
	Config
	PodCIDR        netip.Prefix // this node's pod CIDR
	ClusterNodeIPs []netip.Addr // other cluster nodes' Tailscale IPs, sorted
}

// Manager applies the firewall table and skips work when nothing changed.
// The table is only written once the node's pod CIDR is known.
type Manager struct {
	mu      sync.Mutex
	desired state
	applied *state
}

// NewManager returns a Manager for the given static config.
func NewManager(cfg Config) (*Manager, error) {
	if !cfg.ClusterCIDR.IsValid() || !cfg.ClusterCIDR.Addr().Is4() {
		return nil, fmt.Errorf("firewall: cluster CIDR must be a valid IPv4 prefix")
	}
	for _, p := range cfg.AllowedSources {
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("firewall: allowed source %s must be IPv4", p)
		}
	}
	cfg.ClusterCIDR = cfg.ClusterCIDR.Masked()
	return &Manager{desired: state{Config: cfg}}, nil
}

// SetPodCIDR sets this node's pod CIDR and applies the table if it changed.
func (m *Manager) SetPodCIDR(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is4() {
		return fmt.Errorf("firewall: pod CIDR must be a valid IPv4 prefix")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desired.PodCIDR = p.Masked()
	return m.applyLocked()
}

// SetClusterNodeIPs sets the other cluster nodes' Tailscale IPs and applies
// the table if they changed.
func (m *Manager) SetClusterNodeIPs(ips []netip.Addr) error {
	ips = slices.Clone(ips)
	slices.SortFunc(ips, netip.Addr.Compare)
	ips = slices.Compact(ips)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desired.ClusterNodeIPs = ips
	return m.applyLocked()
}

func (m *Manager) applyLocked() error {
	if !m.desired.PodCIDR.IsValid() {
		return nil // not ready yet
	}
	if m.applied != nil && m.applied.equal(&m.desired) {
		return nil
	}
	if err := apply(&m.desired); err != nil {
		return err
	}
	s := m.desired
	s.ClusterNodeIPs = slices.Clone(s.ClusterNodeIPs)
	s.AllowedSources = slices.Clone(s.AllowedSources)
	m.applied = &s
	return nil
}

func (s *state) equal(o *state) bool {
	return s.ClusterCIDR == o.ClusterCIDR &&
		s.Bridge == o.Bridge &&
		s.TailscaleIface == o.TailscaleIface &&
		s.IngressIsolation == o.IngressIsolation &&
		slices.Equal(s.AllowedSources, o.AllowedSources) &&
		s.PodCIDR == o.PodCIDR &&
		slices.Equal(s.ClusterNodeIPs, o.ClusterNodeIPs)
}
