// Package tailscale wraps the Tailscale LocalAPI to advertise subnet routes
// and ensure accept-routes is enabled so we receive routes to other nodes' pod CIDRs.
package tailscale

import (
	"context"
	"net/netip"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

// Client talks to the Tailscale daemon on the host (via socket).
type Client struct {
	lc *local.Client
}

// NewClient returns a client that uses the default Tailscale socket,
// or the socket at socketPath if non-empty.
func NewClient(socketPath string) *Client {
	lc := &local.Client{}
	if socketPath != "" {
		lc.Socket = socketPath
	}
	return &Client{lc: lc}
}

// Status returns the current Tailscale status (for debugging and to read existing prefs).
func (c *Client) Status(ctx context.Context) (*ipnstate.Status, error) {
	return c.lc.Status(ctx)
}

// AdvertiseRoute advertises the given CIDR as a subnet route from this node.
// The tailnet must allow this (e.g. --advertise-routes on join or ACL).
// It merges with existing AdvertiseRoutes in prefs.
func (c *Client) AdvertiseRoute(ctx context.Context, cidr netip.Prefix) error {
	prefs, err := c.lc.GetPrefs(ctx)
	if err != nil {
		return err
	}
	routes := prefs.AdvertiseRoutes
	seen := false
	for _, r := range routes {
		if r == cidr {
			seen = true
			break
		}
	}
	if seen {
		return nil
	}
	routes = append(routes, cidr)
	mp := &ipn.MaskedPrefs{
		AdvertiseRoutesSet: true,
		Prefs: ipn.Prefs{
			AdvertiseRoutes: routes,
		},
	}
	_, err = c.lc.EditPrefs(ctx, mp)
	return err
}

// UnadvertiseRoute removes the given CIDR from advertised routes.
func (c *Client) UnadvertiseRoute(ctx context.Context, cidr netip.Prefix) error {
	prefs, err := c.lc.GetPrefs(ctx)
	if err != nil {
		return err
	}
	routes := prefs.AdvertiseRoutes
	var newRoutes []netip.Prefix
	for _, r := range routes {
		if r != cidr {
			newRoutes = append(newRoutes, r)
		}
	}
	mp := &ipn.MaskedPrefs{
		AdvertiseRoutesSet: true,
		Prefs: ipn.Prefs{
			AdvertiseRoutes: newRoutes,
		},
	}
	_, err = c.lc.EditPrefs(ctx, mp)
	return err
}

// SetAdvertiseRoutes sets the full list of advertised routes (replacing any existing).
func (c *Client) SetAdvertiseRoutes(ctx context.Context, routes []netip.Prefix) error {
	mp := &ipn.MaskedPrefs{
		AdvertiseRoutesSet: true,
		Prefs: ipn.Prefs{
			AdvertiseRoutes: routes,
		},
	}
	_, err := c.lc.EditPrefs(ctx, mp)
	return err
}

// EnsureAcceptRoutes turns on "accept routes" (RouteAll) so this node installs
// routes for subnets advertised by other tailnet nodes (other nodes' pod CIDRs).
func (c *Client) EnsureAcceptRoutes(ctx context.Context, accept bool) error {
	prefs, err := c.lc.GetPrefs(ctx)
	if err != nil {
		return err
	}
	if prefs.RouteAll == accept {
		return nil
	}
	mp := &ipn.MaskedPrefs{
		RouteAllSet: true,
		Prefs: ipn.Prefs{
			RouteAll: accept,
		},
	}
	_, err = c.lc.EditPrefs(ctx, mp)
	return err
}

// SelfTailscaleIPv4 returns this node's Tailscale IPv4 address from status.
// Using it as the route gateway forces traffic out tailscale0; Tailscale then
// routes it to the peer that advertises the destination subnet.
// Returns zero addr and false if not found.
func SelfTailscaleIPv4(st *ipnstate.Status) (netip.Addr, bool) {
	if st == nil || len(st.TailscaleIPs) == 0 {
		return netip.Addr{}, false
	}
	a := firstIPv4(st.TailscaleIPs)
	return a, a.IsValid()
}

func firstIPv4(addrs []netip.Addr) netip.Addr {
	for _, a := range addrs {
		if a.Is4() {
			return a
		}
	}
	return netip.Addr{}
}
