//go:build linux

package routes

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
)

// Routes are added to the default (main) table; we omit Table so the kernel uses it.
// The gateway is our own Tailscale IP (to force egress out tailscale0), which is on-link.

func addRoute(cidr, via, tailscaleIface string) error {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return fmt.Errorf("parse cidr: %w", err)
	}
	gw, err := netip.ParseAddr(via)
	if err != nil {
		return fmt.Errorf("parse via: %w", err)
	}
	// netlink expects *net.IPNet and net.IP
	ipNet := net.IPNet{
		IP:   prefix.Addr().AsSlice(),
		Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
	}
	route := &netlink.Route{
		Dst: &ipNet,
		Gw:  gw.AsSlice(),
	}
	if tailscaleIface != "" {
		link, err := netlink.LinkByName(tailscaleIface)
		if err != nil {
			return fmt.Errorf("link %s: %w", tailscaleIface, err)
		}
		route.LinkIndex = link.Attrs().Index
	}
	err = netlink.RouteAdd(route)
	if err != nil {
		// EEXIST: route already exists
		if errors.Is(err, syscall.EEXIST) {
			return nil
		}
		return err
	}
	return nil
}

func delRoute(cidr string) error {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return fmt.Errorf("parse cidr: %w", err)
	}
	ipNet := &net.IPNet{
		IP:   prefix.Addr().AsSlice(),
		Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen()),
	}
	route := &netlink.Route{Dst: ipNet}
	err = netlink.RouteDel(route)
	if err != nil {
		// ESRCH or ENOENT: route already gone
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		return err
	}
	return nil
}
