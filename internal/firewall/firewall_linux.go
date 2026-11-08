//go:build linux

package firewall

import (
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

const (
	tableName      = "tailscale-cni"
	natChainName   = "masq"
	fwdChainName   = "pod-ingress"
	nodeSetName    = "cluster_nodes"
	ifnameSize     = 16   // IFNAMSIZ on Linux
	ctStatusDstNAT = 0x20 // IPS_DST_NAT
)

// apply rebuilds the whole table from s in a single atomic transaction, so
// there's never a moment with no NAT or no filtering.
func apply(s *state) error {
	conn, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables conn: %w", err)
	}

	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName}
	// Add (no-op if it exists) then delete, so the delete can't fail on first
	// run, then rebuild. All in one batch.
	conn.AddTable(table)
	conn.DelTable(table)
	conn.AddTable(table)

	addNAT(conn, table, s)
	if s.IngressIsolation {
		if err := addIngress(conn, table, s); err != nil {
			return err
		}
	}

	if err := conn.Flush(); err != nil {
		return fmt.Errorf("nftables apply: %w", err)
	}
	return nil
}

// addNAT: pod -> pod keeps its source IP; anything else leaving the node
// (other than back onto the bridge) is masqueraded as the node.
func addNAT(conn *nftables.Conn, table *nftables.Table, s *state) {
	chain := conn.AddChain(&nftables.Chain{
		Name:     natChainName,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityRef(99), // before NATSource (100)
	})

	// ip saddr podCIDR ip daddr clusterCIDR accept
	conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: concat(
		matchPrefix(saddrOffset, s.PodCIDR),
		matchPrefix(daddrOffset, s.ClusterCIDR),
		verdict(expr.VerdictAccept),
	)})
	// ip saddr podCIDR oifname != bridge masquerade
	conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: concat(
		matchPrefix(saddrOffset, s.PodCIDR),
		matchIfname(expr.MetaKeyOIFNAME, expr.CmpOpNeq, s.Bridge),
		[]expr.Any{&expr.Masq{}},
	)})
}

// addIngress filters traffic routed into the pod bridge from another
// interface. Locally generated traffic (host -> pod) uses OUTPUT, not
// FORWARD, so it isn't affected. A drop in this chain is final regardless of
// what other tables (kube-router, kube-proxy, tailscaled) accept.
func addIngress(conn *nftables.Conn, table *nftables.Table, s *state) error {
	policy := nftables.ChainPolicyAccept
	chain := conn.AddChain(&nftables.Chain{
		Name:     fwdChainName,
		Table:    table,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookForward,
		Priority: nftables.ChainPriorityFilter,
		Policy:   &policy,
	})

	nodes := &nftables.Set{Table: table, Name: nodeSetName, KeyType: nftables.TypeIPAddr}
	elems := make([]nftables.SetElement, 0, len(s.ClusterNodeIPs))
	for _, ip := range s.ClusterNodeIPs {
		if ip.Is4() {
			elems = append(elems, nftables.SetElement{Key: ip.AsSlice()})
		}
	}
	if err := conn.AddSet(nodes, elems); err != nil {
		return fmt.Errorf("add set %s: %w", nodeSetName, err)
	}

	add := func(e ...[]expr.Any) {
		conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: concat(e...)})
	}
	ret := verdict(expr.VerdictReturn)
	accept := verdict(expr.VerdictAccept)
	fromTailscale := matchIfname(expr.MetaKeyIIFNAME, expr.CmpOpEq, s.TailscaleIface)

	// Only traffic going into the bridge from somewhere else.
	add(matchIfname(expr.MetaKeyOIFNAME, expr.CmpOpNeq, s.Bridge), ret)
	add(matchIfname(expr.MetaKeyIIFNAME, expr.CmpOpEq, s.Bridge), ret)
	// Replies to connections pods started.
	add(matchCtBits(expr.CtKeySTATE, expr.CtStateBitESTABLISHED|expr.CtStateBitRELATED), accept)
	// Services and hostPorts (kube-proxy / portmap DNAT): exposure was asked for.
	add(matchCtBits(expr.CtKeySTATUS, ctStatusDstNAT), accept)
	// Other pods, over Tailscale.
	add(fromTailscale, matchPrefix(saddrOffset, s.ClusterCIDR), accept)
	// Other cluster nodes (apiserver -> webhooks, masqueraded NodePort traffic,
	// hostNetwork pods), over Tailscale.
	add(fromTailscale, lookupSaddr(nodes), accept)
	// Explicit allowlist, any interface.
	for _, p := range s.AllowedSources {
		add(matchPrefix(saddrOffset, p), accept)
	}
	add([]expr.Any{&expr.Counter{}}, verdict(expr.VerdictDrop))
	return nil
}

// Teardown removes the tailscale-cni nftables table.
func Teardown() error {
	conn, err := nftables.New()
	if err != nil {
		return err
	}
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: tableName}
	conn.AddTable(table)
	conn.DelTable(table)
	return conn.Flush()
}

const (
	saddrOffset = 12
	daddrOffset = 16
)

func concat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func verdict(k expr.VerdictKind) []expr.Any {
	return []expr.Any{&expr.Verdict{Kind: k}}
}

// matchPrefix matches the IPv4 header address at offset (saddr/daddr) against p.
func matchPrefix(offset uint32, p netip.Prefix) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: netmask4(p.Bits()), Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: p.Masked().Addr().AsSlice()},
	}
}

func lookupSaddr(set *nftables.Set) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: saddrOffset, Len: 4},
		&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID},
	}
}

func matchIfname(key expr.MetaKey, op expr.CmpOp, name string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: op, Register: 1, Data: padIfname(name)},
	}
}

// matchCtBits matches when any of bits are set in the conntrack key (state or status).
func matchCtBits(key expr.CtKey, bits uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Key: key, Register: 1},
		&expr.Bitwise{
			SourceRegister: 1, DestRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(bits),
			Xor:  binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(0)},
	}
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
