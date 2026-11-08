# Tailscale CNI

CNI driver for Kubernetes that manages subnet routes for pod CIDR ranges.
Designed around managing a host tailscale daemon, that already exists.

## Tailscale ACLs

```json
// Allow the network traffic
{
   "acls": [
      { "action": "accept", "src": ["tag:tailscale-cni-dev"], "dst": ["10.99.0.0/16:*"] }
   ],
   "tagOwners": {
      "tag:tailscale-cni-dev": ["autogroup:admin"]
   }
},
// Auto approve the CNI adding subnet routes.
"autoApprovers": {
		"routes": {
			"10.99.0.0/16": ["tag:tailscale-cni-dev"],
		},
	},
```

Adjust `10.99.0.0/16` to your `CLUSTER_CIDR`.

## Pod network isolation

The pod network behaves like a private network behind each node:

- **Pod to pod** (any node) keeps the pod's IP, so NetworkPolicy can match on it.
- **Pod to anything else**, including tailnet devices outside the cluster, is
  masqueraded as the node. Write tailnet ACLs for those destinations with the
  node tag as the source, not the pod CIDR.
- **Into pods**, the driver's nftables table drops traffic from outside the
  node unless it's from another pod, another cluster node's Tailscale IP, a
  reply to a connection a pod started, or traffic DNAT'd by a Service/hostPort
  (NodePort and LoadBalancer keep working). A tailnet device that can route to
  the pod CIDR still can't reach pod IPs directly.

Cluster nodes are the tailnet peers currently routing a subnet inside
`CLUSTER_CIDR`. The list refreshes every 30s and on node changes.

Flags:

- `-pod-ingress-allow` / `POD_INGRESS_ALLOW`: comma-separated CIDRs that may
  reach pods directly anyway (e.g. `100.89.18.80` for an admin laptop).
- `-pod-ingress-isolation=false` / `POD_INGRESS_ISOLATION=false`: turn the
  ingress filter off.

Keep the tailnet policy tight as well: only the cluster's tag should be granted
the pod CIDR, and only the cluster's tag should be in `autoApprovers` for it.
Tailscale only accepts a packet from a pod-CIDR source if the sending peer is
approved to route that subnet, so `autoApprovers` decides who can claim to be a pod.

## Tailscale prefs this driver changes

These are saved in tailscaled's own state, so they stay set after the DaemonSet is removed.

- **Advertise routes**: adds this node's pod CIDR.
- **Accept routes** (`RouteAll`): on, so the node learns other nodes' pod CIDRs.
- **Subnet SNAT** (`NoSNAT`): disabled, so pods see the real source IP of
  traffic from other nodes and tailnet clients. NetworkPolicy relies on this.
- **Netfilter mode**: unchanged unless `-tailscale-netfilter-mode` /
  `TAILSCALE_NETFILTER_MODE` is set. `nodivert` stops tailscaled from hooking its
  chains into `FORWARD`/`INPUT`/`POSTROUTING`. Otherwise its `ts-forward` chain
  ACCEPTs everything from `tailscale0`, and if it lands ahead of the policy
  engine's chain, network policy is skipped for that traffic.
