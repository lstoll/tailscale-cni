//go:build linux

package firewall

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestApplyKernel applies the table to the running kernel. It needs root and
// CAP_NET_ADMIN, so it only runs with TSCNI_NFT_TEST=1, ideally in a throwaway
// network namespace or container:
//
//	docker run --rm --privileged -v $PWD:/src -w /src golang:1 \
//	  sh -c 'apt-get update -qq && apt-get install -y -qq nftables >/dev/null && TSCNI_NFT_TEST=1 go test -v -run Kernel ./internal/firewall'
func TestApplyKernel(t *testing.T) {
	if os.Getenv("TSCNI_NFT_TEST") != "1" {
		t.Skip("set TSCNI_NFT_TEST=1 to apply rules to the kernel")
	}
	m, err := NewManager(Config{
		ClusterCIDR:      netip.MustParsePrefix("10.42.0.0/16"),
		Bridge:           "cni0",
		TailscaleIface:   "tailscale0",
		IngressIsolation: true,
		AllowedSources:   []netip.Prefix{netip.MustParsePrefix("100.89.18.80/32")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetPodCIDR(netip.MustParsePrefix("10.42.6.0/24")); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Second apply replaces an existing table.
	if err := m.SetClusterNodeIPs([]netip.Addr{
		netip.MustParseAddr("100.64.0.2"), netip.MustParseAddr("100.64.0.1"),
	}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	out, err := exec.Command("nft", "list", "table", "ip", tableName).CombinedOutput()
	if err != nil {
		t.Fatalf("nft list: %v\n%s", err, out)
	}
	t.Logf("\n%s", out)
	for _, want := range []string{
		`ip saddr 10.42.6.0/24 ip daddr 10.42.0.0/16 accept`,
		`ip saddr 10.42.6.0/24 oifname != "cni0" masquerade`,
		`ct state established,related accept`,
		`ct status dnat accept`,
		`iifname "tailscale0" ip saddr 10.42.0.0/16 accept`,
		`iifname "tailscale0" ip saddr @cluster_nodes accept`,
		`ip saddr 100.89.18.80 accept`,
		`drop`,
		`100.64.0.1`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("ruleset missing %q", want)
		}
	}

	if err := Teardown(); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if err := exec.Command("nft", "list", "table", "ip", tableName).Run(); err == nil {
		t.Error("table still exists after teardown")
	}
}
