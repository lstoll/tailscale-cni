//go:build !linux

package masq

import "fmt"

// Setup is only implemented on Linux (uses nftables).
func Setup(podCIDR, bridgeName, tailscaleInterface string, metadataRedirectPort int) error {
	return fmt.Errorf("masq: nftables only supported on Linux")
}

// Teardown is only implemented on Linux.
func Teardown() error {
	return nil
}
