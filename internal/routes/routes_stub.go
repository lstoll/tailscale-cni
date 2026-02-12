//go:build !linux

package routes

// addRoute and delRoute are no-ops on non-Linux; route management is Linux-only.
func addRoute(cidr, via, tailscaleIface string) error { return nil }
func delRoute(cidr string) error                      { return nil }
