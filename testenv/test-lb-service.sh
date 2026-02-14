#!/usr/bin/env bash
# Deploy nginx + LoadBalancer Service (lds.li/tailscale-cni), curl from host (must be on tailnet).
# Tests HTTP and HTTPS (TLS cert from metadata API). Restart pod 3x and scale 0->1 to test register/deregister.
#
# Requires:
#   - Tailscale Service "test-nginx" in admin console with tcp:80 and tcp:443 (see instructions below).
#   - TAILNET_DNS_SUFFIX: your tailnet MagicDNS suffix (e.g. dingo-nase.ts.net) for the init container.
#     Optional if running in testenv (discovered from tailscale status) or if test-nginx already has a hostname.
#
#   export KUBECONFIG=$(pwd)/testenv/kubeconfig
#   export TAILNET_DNS_SUFFIX=your-tailnet.ts.net   # optional in testenv or when service already exists
#   ./testenv/test-lb-service.sh
#
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KUBECONFIG="${KUBECONFIG:-$REPO_ROOT/testenv/kubeconfig}"

export KUBECONFIG

# Discover TAILNET_DNS_SUFFIX if not set (same idea as test-pod-network.sh discovering TAILSCALE_IP from testenv).
if [[ -z "${TAILNET_DNS_SUFFIX:-}" ]]; then
  # From existing test-nginx LoadBalancer hostname (e.g. test-nginx.dingo-nase.ts.net -> dingo-nase.ts.net)
  EXISTING_HOST=$(kubectl get svc test-nginx -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)
  if [[ -n "$EXISTING_HOST" && "$EXISTING_HOST" == *.* ]]; then
    TAILNET_DNS_SUFFIX="${EXISTING_HOST#*.}"
    echo "Using TAILNET_DNS_SUFFIX from existing test-nginx hostname: $TAILNET_DNS_SUFFIX"
  fi
fi
if [[ -z "${TAILNET_DNS_SUFFIX:-}" ]] && [[ -d "$SCRIPT_DIR" ]]; then
  # From vagrant + tailscale status (testenv)
  TAILNET_DNS_SUFFIX=$(cd "$SCRIPT_DIR" && vagrant ssh control-plane -c 'tailscale status --json 2>/dev/null' 2>/dev/null | jq -r '.MagicDNSSuffix // empty' 2>/dev/null || true)
  if [[ -n "$TAILNET_DNS_SUFFIX" ]]; then
    echo "Using TAILNET_DNS_SUFFIX from tailscale status: $TAILNET_DNS_SUFFIX"
  fi
fi
if [[ -z "${TAILNET_DNS_SUFFIX:-}" ]]; then
  echo "TAILNET_DNS_SUFFIX is not set. Required for TLS (cert-fetcher) and for init container."
  echo "Set it to your tailnet MagicDNS suffix (e.g. dingo-nase.ts.net)."
  echo "Example: export TAILNET_DNS_SUFFIX=dingo-nase.ts.net"
  exit 1
fi

echo "=== Tailscale LoadBalancer Service test (nginx HTTP + HTTPS) ==="
echo "Substituting TAILNET_DNS_SUFFIX=$TAILNET_DNS_SUFFIX in manifest..."
MANIFEST=$(sed "s/TAILNET_DNS_SUFFIX/$TAILNET_DNS_SUFFIX/g" "$REPO_ROOT/deploy/test-lb-service.yaml")
echo "Deploying test-nginx Deployment + LoadBalancer Service (loadBalancerClass: lds.li/tailscale-cni)..."
echo "$MANIFEST" | kubectl apply -f -

echo "Waiting for test-nginx pod to be Running (init container fetches cert, then nginx starts)..."
kubectl wait --for=condition=Ready pod -l app=test-nginx --timeout=180s 2>/dev/null || {
  echo "Timed out waiting for test-nginx pod. Check init container (cert-fetcher) and nginx logs:"
  kubectl get pods -l app=test-nginx -o wide
  kubectl logs -l app=test-nginx -c cert-fetcher --tail=30 2>/dev/null || true
  kubectl logs -l app=test-nginx -c nginx --tail=20 2>/dev/null || true
  exit 1
}

echo "Waiting for LoadBalancer hostname (up to 90s)..."
HOSTNAME=""
for i in $(seq 1 90); do
  HOSTNAME=$(kubectl get svc test-nginx -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)
  if [[ -n "$HOSTNAME" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "$HOSTNAME" ]]; then
  echo "SKIP: test-nginx Service did not get a hostname. Define Tailscale Service 'test-nginx' in admin with tcp:80 and tcp:443."
  kubectl get svc test-nginx -o wide
  kubectl -n kube-system logs -l app=tailscale-cni --tail=20 2>/dev/null || true
  exit 0
fi
echo "Hostname: $HOSTNAME"

echo ""
echo "=== Curl HTTP http://${HOSTNAME}/ ==="
if ! curl -sf --connect-timeout 10 "http://${HOSTNAME}/" | grep -q -i nginx; then
  echo "FAIL: curl to http://${HOSTNAME}/ did not return nginx content."
  exit 1
fi
echo "OK: HTTP returned nginx."

echo ""
echo "=== Curl HTTPS https://${HOSTNAME}/ ==="
if ! curl -sf --connect-timeout 10 "https://${HOSTNAME}/" | grep -q -i nginx; then
  echo "FAIL: curl to https://${HOSTNAME}/ did not return nginx content. Is tcp:443 in Tailscale Service?"
  exit 1
fi
echo "OK: HTTPS returned nginx."

echo ""
echo "=== Restarting test-nginx pod 3 times (register/deregister) ==="
for round in 1 2 3; do
  echo "--- Round $round: deleting pod ---"
  kubectl delete pod -l app=test-nginx --wait=false
  echo "Waiting for deployment to have 1 ready replica..."
  kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 deployment/test-nginx --timeout=180s 2>/dev/null || {
    echo "Timed out waiting for pod after delete in round $round"
    exit 1
  }
  sleep 15
  echo "Curl HTTP and HTTPS..."
  if ! curl -sf --connect-timeout 10 "http://${HOSTNAME}/" | grep -q -i nginx; then
    echo "FAIL: HTTP failed after pod restart round $round"
    exit 1
  fi
  if ! curl -sf --connect-timeout 10 "https://${HOSTNAME}/" | grep -q -i nginx; then
    echo "FAIL: HTTPS failed after pod restart round $round"
    exit 1
  fi
  echo "OK: round $round - HTTP and HTTPS reachable."
done

echo ""
echo "=== Scale to 0 then back to 1 (deregister / re-register) ==="
kubectl scale deployment test-nginx --replicas=0
echo "Waiting for pod to terminate..."
for i in $(seq 1 30); do
  if ! kubectl get pods -l app=test-nginx --no-headers 2>/dev/null | grep -q .; then
    break
  fi
  sleep 1
done
sleep 3
echo "Scaling back to 1..."
kubectl scale deployment test-nginx --replicas=1
kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 deployment/test-nginx --timeout=120s 2>/dev/null || exit 1
sleep 15
echo "Curl HTTP and HTTPS after scale-up..."
if ! curl -sf --connect-timeout 15 "http://${HOSTNAME}/" | grep -q -i nginx; then
  echo "FAIL: HTTP failed after scale back to 1"
  exit 1
fi
if ! curl -sf --connect-timeout 15 "https://${HOSTNAME}/" | grep -q -i nginx; then
  echo "FAIL: HTTPS failed after scale back to 1"
  exit 1
fi
echo "OK: Tailscale LoadBalancer (HTTP + HTTPS) test passed."
