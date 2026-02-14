//go:build linux

// Package masq configures nftables to masquerade traffic from the pod bridge
// that leaves the node via the host's default route (e.g. to the internet).
// Standard CNI behavior: we do not masq traffic that stays on the bridge (cni0)
// or goes out Tailscale (pod-to-pod across nodes).
// Optionally adds a nat prerouting chain to redirect metadata service traffic
// (169.254.169.253:80) from the pod CIDR to a local port.
package masq

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

const (
	tableName  = "tailscale-cni"
	chainName  = "masq"
	ifnameSize = 16 // IFNAMSIZ on Linux

	metadataChainName   = "metadata-redirect"
	metadataIP          = "169.254.169.253"
	metadataPortMatch   = 80
)

// Setup reconciles the tailscale-cni nftables table to the desired state: a
// single NAT chain that masquerades traffic from podCIDR leaving via any
// interface other than the bridge (bridgeName) or Tailscale (tailscaleInterface).
// Traffic to the internet via the host's default route gets SNAT'd; pod-to-pod
// and pod-to-tailscale do not. Ingress to pods is not filtered here; use
// Tailscale ACLs to control who can reach your cluster's pod CIDRs.
//
// If metadataRedirectPort is > 0, a nat prerouting chain is added that DNATs
// traffic from podCIDR to 169.254.169.253:80 to 127.0.0.1:metadataRedirectPort.
//
// Reconcile semantics: we always delete the table (if it exists) then recreate
// it from scratch. That guarantees no stale chains, rules, or sets remain from
// previous runs or from removed features.
func Setup(podCIDR, bridgeName, tailscaleInterface string, metadataRedirectPort int) error {
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

	if metadataRedirectPort > 0 {
		if err := addMetadataRedirectChain(conn, table, podCIDR, metadataRedirectPort); err != nil {
			return err
		}
	}

	return conn.Flush()
}

// addMetadataRedirectChain adds a nat prerouting chain that DNATs traffic from
// podCIDR to metadataIP:metadataPortMatch to 127.0.0.1:metadataRedirectPort.
func addMetadataRedirectChain(conn *nftables.Conn, table *nftables.Table, podCIDR string, metadataRedirectPort int) error {
	prefix, err := netip.ParsePrefix(podCIDR)
	if err != nil {
		return fmt.Errorf("metadata redirect pod CIDR: %w", err)
	}
	if !prefix.Addr().Is4() {
		return fmt.Errorf("metadata redirect requires IPv4 pod CIDR")
	}

	metaChain := &nftables.Chain{
		Name:     metadataChainName,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityNATDest,
	}
	conn.AddChain(metaChain)

	// Match: ip daddr 169.254.169.253
	metaIP := net.ParseIP(metadataIP)
	if metaIP == nil || metaIP.To4() == nil {
		return fmt.Errorf("invalid metadata IP %s", metadataIP)
	}
	// Match: tcp dport 80
	port80 := []byte{0, 80}
	// Match: ip saddr in podCIDR (same as masq: load saddr, mask, cmp)
	bits := prefix.Bits()
	if bits < 0 || bits > 32 {
		return fmt.Errorf("invalid prefix bits: %d", bits)
	}
	mask := netmask4(bits)
	network := prefix.Masked().Addr().AsSlice()
	// New destination: 127.0.0.1, port metadataRedirectPort (big-endian 2 bytes)
	loopback := net.IPv4(127, 0, 0, 1).To4()
	portBuf := make([]byte, 2)
	portBuf[0] = byte(metadataRedirectPort >> 8)
	portBuf[1] = byte(metadataRedirectPort)

	exprs := []expr.Any{
		// ip daddr -> reg 1, cmp eq 169.254.169.253
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       16,
			Len:          4,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     metaIP.To4(),
		},
		// tcp dport -> reg 1, cmp eq 80
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseTransportHeader,
			Offset:       2,
			Len:          2,
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     port80,
		},
		// ip saddr in podCIDR
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12,
			Len:          4,
		},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           mask,
			Xor:            []byte{0, 0, 0, 0},
		},
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     network,
		},
		// Load new IP and port into reg 2 and reg 3 for DNAT
		&expr.Immediate{
			Register: 2,
			Data:     loopback,
		},
		&expr.Immediate{
			Register: 3,
			Data:     portBuf,
		},
		&expr.NAT{
			Type:        expr.NATTypeDestNAT,
			Family:      unix.NFPROTO_IPV4,
			RegAddrMin:  2,
			RegProtoMin: 3,
		},
	}

	conn.AddRule(&nftables.Rule{
		Table: table,
		Chain: metaChain,
		Exprs: exprs,
	})
	return nil
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
