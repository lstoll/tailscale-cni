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

## Tailscale Services

Services can be created in Kubernetes, that will be configure to serve [Tailscale Services]() via the host tailscaled if the pod exists on the node.


```yaml
apiVersion: v1
kind: Service
metadata:
  name: test-nginx
  annotations:
    # Tailscale Service name
    tailscale-cni.lds.li/service-name: "test-nginx"
spec:
  type: LoadBalancer
  loadBalancerClass: lds.li/tailscale-cni
  selector:
    app: test-nginx
  ports:
    - name: http
      port: 80
      targetPort: http
      protocol: TCP
```

ACL to allow the nodes to serve:

```json
"autoApprovers": {
  "services": {
    "svc:test-nginx": ["tag:tailscale-cni-dev"],
  },
},
```

### Defining the Service in Tailscale Admin Console

For the test LoadBalancer (e.g. `test-nginx`) to work, you must define the **Tailscale Service** in the admin console so the control plane knows which ports to route:

1. Open **Tailscale Admin Console** → **Settings** → **Services** (or **Access controls** → Services).
2. **Add a service** (or edit existing):
   - **Service name**: `test-nginx` (must match the annotation `tailscale-cni.lds.li/service-name` or the default derived from the K8s Service name).
   - **Protocol and port**: add **tcp:80** (HTTP) and **tcp:443** (HTTPS) so both are advertised.
3. Save. Nodes that advertise `svc:test-nginx` will then accept traffic on both ports.

For HTTP-only you only need tcp:80. For the TLS test (metadata cert + nginx HTTPS), add tcp:443 as well.
