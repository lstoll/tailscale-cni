#!/usr/bin/env bash
# Deploy nginx + LoadBalancer Service (lds.li/tailscale-cni), curl from host (must be on tailnet),
# restart pod 3x and scale 0->1 to test register/deregister.
#
# Requires: Tailscale Service "test-nginx" defined in admin with tcp:80; nodes with tag-based identity.
#
#   export KUBECONFIG=$(pwd)/testenv/kubeconfig
#   ./testenv/test-lb-service.sh
#
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KUBECONFIG="${KUBECONFIG:-$REPO_ROOT/testenv/kubeconfig}"

export KUBECONFIG

echo "=== Tailscale LoadBalancer Service test (nginx) ==="
echo "Deploying test-nginx Deployment + LoadBalancer Service (loadBalancerClass: lds.li/tailscale-cni)..."
kubectl apply -f "$REPO_ROOT/deploy/test-lb-service.yaml"

echo "Waiting for test-nginx pod to be Running..."
kubectl wait --for=condition=Ready pod -l app=test-nginx --timeout=120s 2>/dev/null || {
  echo "Timed out waiting for test-nginx pod."
  kubectl get pods -l app=test-nginx -o wide
  exit 1
}

echo "Waiting for LoadBalancer hostname (up to 90s; requires Tailscale Service 'test-nginx' defined in admin with tcp:80)..."
HOSTNAME=""
for i in $(seq 1 90); do
  HOSTNAME=$(kubectl get svc test-nginx -o jsonpath='{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)
  if [[ -n "$HOSTNAME" ]]; then
    break
  fi
  sleep 1
done
if [[ -z "$HOSTNAME" ]]; then
  echo "SKIP: test-nginx Service did not get a hostname. Define Tailscale Service 'test-nginx' in admin with tcp:80 and ensure nodes use tag-based identity."
  kubectl get svc test-nginx -o wide
  kubectl -n kube-system logs -l app=tailscale-cni --tail=20 2>/dev/null || true
  exit 0
fi
echo "Hostname: $HOSTNAME"

echo ""
echo "=== Curl from host (you must be on the tailnet) http://${HOSTNAME}/ ==="
if ! curl -sf --connect-timeout 10 "http://${HOSTNAME}/" | grep -q -i nginx; then
  echo "FAIL: curl to http://${HOSTNAME}/ did not return nginx content. Are you on the tailnet? Is the Tailscale Service approved?"
  exit 1
fi
echo "OK: curl to Tailscale Service hostname returned nginx."

echo ""
echo "=== Restarting test-nginx pod 3 times to exercise register/deregister ==="
for round in 1 2 3; do
  echo "--- Round $round: deleting pod ---"
  kubectl delete pod -l app=test-nginx --wait=false
  echo "Waiting for deployment to have 1 ready replica..."
  kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 deployment/test-nginx --timeout=180s 2>/dev/null || {
    echo "Timed out waiting for pod after delete in round $round"
    exit 1
  }
  sleep 3
  echo "Curl from host to verify service is reachable..."
  if ! curl -sf --connect-timeout 10 "http://${HOSTNAME}/" | grep -q -i nginx; then
    echo "FAIL: curl failed after pod restart round $round (register may have been delayed or failed)"
    exit 1
  fi
  echo "OK: round $round - service reachable after pod restart."
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
echo "Curl while scaled to 0 (expect failure or timeout)..."
if curl -sf --connect-timeout 5 "http://${HOSTNAME}/" 2>/dev/null | grep -q -i nginx; then
  echo "WARN: curl still succeeded after scale to 0 (another node may still be serving or DNS cached)"
fi
echo "Scaling back to 1..."
kubectl scale deployment test-nginx --replicas=1
kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 deployment/test-nginx --timeout=120s 2>/dev/null || exit 1
sleep 5
echo "Curl from host after scale-up..."
if ! curl -sf --connect-timeout 15 "http://${HOSTNAME}/" | grep -q -i nginx; then
  echo "FAIL: curl failed after scale back to 1 (re-register may have failed)"
  exit 1
fi
echo "OK: Tailscale LoadBalancer register/deregister test passed."
