//go:build linux

// Package masq configures nftables to masquerade traffic from the pod bridge
// that leaves the node via the host's default route (e.g. to the internet).
// Standard CNI behavior: we do not masq traffic that stays on the bridge (cni0)
// or goes out Tailscale (pod-to-pod across nodes).
package masq

import (
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

const (
	tableName  = "tailscale-cni"
	chainName  = "masq"
	ifnameSize = 16 // IFNAMSIZ on Linux
)

// Setup reconciles the tailscale-cni nftables table to the desired state: a
// single NAT chain that masquerades traffic from podCIDR leaving via any
// interface other than the bridge (bridgeName) or Tailscale (tailscaleInterface).
// Traffic to the internet via the host's default route gets SNAT'd; pod-to-pod
// and pod-to-tailscale do not. Ingress to pods is not filtered here; use
// Tailscale ACLs to control who can reach your cluster's pod CIDRs.
//
// Reconcile semantics: we always delete the table (if it exists) then recreate
// it from scratch. That guarantees no stale chains, rules, or sets remain from
// previous runs or from removed features.
func Setup(podCIDR, bridgeName, tailscaleInterface string) error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables conn: %w", err)
	}

	prefix, err := netip.ParsePrefix(podCIDR)
	if err != nil {
		return fmt.Errorf("pod CIDR: %w", err)
	}
	if !prefix.Addr().Is4() {
		return fmt.Errorf("pod CIDR must be IPv4")
	}

	// Remove existing table (and all chains/rules in it). Ignore error from Flush:
	// table may not exist yet (first run) or kernel may return ENOENT; either way
	// the create phase below will apply the desired state.
	conn.DelTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName})
	_ = conn.Flush()

	// Create table, chain, and rule from desired state only.
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName}
	conn.AddTable(table)

	chain := &nftables.Chain{
		Name:     chainName,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityRef(99), // before NATSource (100)
	}
	conn.AddChain(chain)

	// Mask for prefix (e.g. /24 -> 255.255.255.0).
	bits := prefix.Bits()
	if bits < 0 || bits > 32 {
		return fmt.Errorf("invalid prefix bits: %d", bits)
	}
	mask := netmask4(bits)
	network := prefix.Masked().Addr().AsSlice()

	// Rule: ip saddr in podCIDR, oifname != bridgeName, oifname != tailscaleInterface -> masquerade
	exprs := []expr.Any{
		// Load ip saddr (offset 12, 4 bytes) into reg 1
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12,
			Len:          4,
		},
		// Mask reg 1 with prefix mask
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           mask,
			Xor:            []byte{0, 0, 0, 0},
		},
		// cmp reg 1 eq network
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     network,
		},
		// Load oifname into reg 2
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 2},
		// cmp reg 2 neq bridgeName (padded to 16 bytes)
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 2,
			Data:     padIfname(bridgeName),
		},
		// Load oifname into reg 2 again
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 2},
		// cmp reg 2 neq tailscaleInterface
		&expr.Cmp{
			Op:       expr.CmpOpNeq,
			Register: 2,
			Data:     padIfname(tailscaleInterface),
		},
		&expr.Masq{},
	}

	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: chain,
		Exprs: exprs,
	})

	return conn.Flush()
}

// Teardown removes the tailscale-cni nftables table.
func Teardown() error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName}
	conn.DelTable(table)
	return conn.Flush()
}

func netmask4(bits int) []byte {
	var m [4]byte
	for i := 0; i < 4 && bits > 0; i++ {
		n := 8
		if bits < 8 {
			n = bits
		}
		m[i] = ^(0xff >> n)
		bits -= 8
	}
	return m[:]
}

func padIfname(name string) []byte {
	b := make([]byte, ifnameSize)
	copy(b, name)
	return b
}
