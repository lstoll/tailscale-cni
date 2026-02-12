// Package cni manages the CNI plugin configuration written to the host.
// The tailscale-cni DaemonSet writes bridge + host-local + portmap config
// so kubelet uses standard CNI plugins for pod networking on the node.
package cni

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

// WriteConflist writes a CNI conflist (list format) so we can chain bridge + portmap.
// dir is the host CNI config directory (e.g. /etc/cni/net.d).
// bridgeName, subnet are used for the bridge and host-local IPAM.
// If clusterCIDR is non-empty, we add a route for it so pods can reach other nodes' pods.
func WriteConflist(dir, name, bridgeName, subnet, clusterCIDR string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	gateway := gatewayFromSubnet(subnet)
	routes := []map[string]string{{"dst": "0.0.0.0/0", "gw": gateway}}
	if clusterCIDR != "" && clusterCIDR != "0.0.0.0/0" {
		routes = append([]map[string]string{{"dst": clusterCIDR}}, routes...)
	}

	conflist := map[string]interface{}{
		"cniVersion": "1.0.0",
		"name":       name,
		"plugins": []map[string]interface{}{
			{
				"type":      "bridge",
				"bridge":    bridgeName,
				"isGateway": true,
				"ipMasq":    false, // we manage masq via nftables
				"ipam": map[string]interface{}{
					"type":   "host-local",
					"subnet": subnet,
					"routes": routes,
				},
			},
			{
				"type": "portmap",
				"capabilities": map[string]bool{
					"portMappings": true,
				},
			},
		},
	}

	data, err := json.MarshalIndent(conflist, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal conflist: %w", err)
	}

	path := filepath.Join(dir, "10-tailscale-cni.conflist")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// gatewayFromSubnet returns the first usable IP in the subnet as gateway (e.g. 10.99.0.0/24 -> 10.99.0.1).
func gatewayFromSubnet(subnet string) string {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return "10.99.0.1"
	}
	addr := prefix.Addr()
	if !addr.Is4() {
		return "10.99.0.1"
	}
	// First address in subnet + 1 (e.g. 10.99.0.0/24 -> 10.99.0.1)
	ip := prefix.Masked().Addr()
	return netip.AddrFrom4([4]byte{
		ip.AsSlice()[0],
		ip.AsSlice()[1],
		ip.AsSlice()[2],
		ip.AsSlice()[3] + 1,
	}).String()
}

// Remove removes our config file from dir.
func Remove(dir string) error {
	path := filepath.Join(dir, "10-tailscale-cni.conflist")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
