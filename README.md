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
