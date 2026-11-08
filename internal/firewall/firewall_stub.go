//go:build !linux

package firewall

import "fmt"

func apply(*state) error {
	return fmt.Errorf("firewall: nftables only supported on Linux")
}

// Teardown is only implemented on Linux.
func Teardown() error {
	return nil
}
