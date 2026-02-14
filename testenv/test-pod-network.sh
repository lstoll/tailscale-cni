#!/usr/bin/env bash
# Deploy test pods (2 replicas, spread across nodes) and verify:
# - Pod-to-pod connectivity (ping both ways)
# - Internet connectivity (ping 8.8.8.8 and google.com from a pod)
# Use with testenv kubeconfig. For LoadBalancer/Tailscale Service test see test-lb-service.sh.
#
#   export KUBECONFIG=$(pwd)/testenv/kubeconfig
#   ./testenv/test-pod-network.sh
#
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KUBECONFIG="${KUBECONFIG:-$REPO_ROOT/testenv/kubeconfig}"

export KUBECONFIG

echo "=== Deleting existing test-pods ==="
kubectl delete deployment test-pods || true

echo "=== Waiting for test-pods to be deleted ==="
for i in $(seq 1 120); do
  if ! kubectl get pods -l app=test-pods --no-headers 2>/dev/null | grep -q .; then
    break
  fi
  sleep 1
done
if kubectl get pods -l app=test-pods --no-headers 2>/dev/null | grep -q .; then
  echo "Timed out waiting for test-pods to be deleted. Current state:"
  kubectl get pods -l app=test-pods -o wide
  exit 1
fi

echo "=== Deploying test-pods (2 replicas, spread across nodes) ==="
kubectl apply -f "$REPO_ROOT/deploy/test-pods.yaml"

echo ""
echo "=== Waiting for 2 pods to be Running ==="
kubectl wait --for=jsonpath='{.status.readyReplicas}'=2 deployment/test-pods --timeout=120s 2>/dev/null || {
  echo "Timed out or not all pods ready. Current state:"
  kubectl get pods -l app=test-pods -o wide
  exit 1
}

echo ""
echo "=== Pods ==="
kubectl get pods -l app=test-pods -o wide

PODS=($(kubectl get pods -l app=test-pods -o jsonpath='{.items[*].metadata.name}'))
if [[ ${#PODS[@]} -lt 2 ]]; then
  echo "Need 2 pods, got ${#PODS[@]}"
  exit 1
fi

POD_A="${PODS[0]}"
POD_B="${PODS[1]}"
IP_B=$(kubectl get pod "$POD_B" -o jsonpath='{.status.podIP}')

if [[ -z "$IP_B" ]]; then
  echo "Could not get pod IP for $POD_B"
  exit 1
fi

echo ""
echo "=== Pinging $POD_B ($IP_B) from $POD_A ==="
if kubectl exec "$POD_A" -- ping -c 3 -W 2 "$IP_B"; then
  echo ""
  echo "OK: pod-to-pod connectivity works."
else
  echo ""
  echo "FAIL: ping from $POD_A to $IP_B failed. Check routes and Tailscale on both nodes."
  exit 1
fi

echo ""
echo "=== Reverse: ping $POD_A from $POD_B ==="
IP_A=$(kubectl get pod "$POD_A" -o jsonpath='{.status.podIP}')
if kubectl exec "$POD_B" -- ping -c 3 -W 2 "$IP_A"; then
  echo ""
  echo "OK: bidirectional pod-to-pod connectivity works."
else
  echo ""
  echo "FAIL: reverse ping failed."
  exit 1
fi

echo ""
echo "=== Internet: ping 8.8.8.8 from $POD_A ==="
if kubectl exec "$POD_A" -- ping -c 3 -W 3 8.8.8.8; then
  echo ""
  echo "OK: outbound internet (8.8.8.8) works."
else
  echo ""
  echo "FAIL: ping 8.8.8.8 failed. Check NAT/masq and default route from pods."
  exit 1
fi

echo ""
echo "=== Internet: ping google.com from $POD_A ==="
if kubectl exec "$POD_A" -- ping -c 3 -W 3 google.com; then
  echo ""
  echo "OK: internet + DNS (google.com) works."
else
  echo ""
  echo "FAIL: ping google.com failed. Check cluster DNS and/or outbound connectivity."
  exit 1
fi

echo ""
echo "=== Metadata service (token + identity) ==="
# Get the other node's Tailscale IP so we can look up its identity from a pod.
# In testenv we have control-plane and node; get Tailscale IP of the node that POD_B is on (the "other" node from POD_A's perspective).
NODE_B=$(kubectl get pod "$POD_B" -o jsonpath='{.spec.nodeName}')
TAILSCALE_IP=""
if [[ -d "$SCRIPT_DIR" ]]; then
  for n in "$NODE_B" control-plane node; do
    TAILSCALE_IP=$(cd "$SCRIPT_DIR" && vagrant ssh "$n" -c "tailscale ip -4 2>/dev/null" 2>/dev/null | tr -d '\r\n' || true)
    [[ -n "$TAILSCALE_IP" ]] && break
  done
fi
if [[ -z "$TAILSCALE_IP" ]]; then
  echo "Skipping metadata identity test (could not get other node Tailscale IP; run from testenv with vagrant)."
else
  echo "Other node ($NODE_B) Tailscale IP: $TAILSCALE_IP"
  # Run a one-off pod with curl on the same node as POD_A to hit the metadata service
  NODE_A=$(kubectl get pod "$POD_A" -o jsonpath='{.spec.nodeName}')
  METADATA_OUT=$(kubectl run metadata-test --rm -i --restart=Never --image=curlimages/curl --overrides="{\"spec\":{\"nodeName\":\"$NODE_A\"}}" -- curl -sS -w "\n%{http_code}" -X PUT -H "X-Tailscale-Metadata-Token-TTL-Seconds: 60" "http://169.254.169.253/metadata/api/token" 2>/dev/null || true)
  TOKEN=$(echo "$METADATA_OUT" | head -n -1)
  HTTP_CODE=$(echo "$METADATA_OUT" | tail -1)
  if [[ "$HTTP_CODE" != "200" || -z "$TOKEN" ]]; then
    echo "FAIL: metadata token request returned HTTP $HTTP_CODE or empty token"
    exit 1
  fi
  echo "OK: metadata token obtained."
  IDENTITY_OUT=$(kubectl run metadata-identity-test --rm -i --restart=Never --image=curlimages/curl --overrides="{\"spec\":{\"nodeName\":\"$NODE_A\"}}" -- curl -sS -w "\n%{http_code}" -H "X-Tailscale-Metadata-Token: $TOKEN" "http://169.254.169.253/metadata/identity?ip=$TAILSCALE_IP" 2>/dev/null || true)
  IDENTITY_HTTP=$(echo "$IDENTITY_OUT" | tail -1)
  IDENTITY_BODY=$(echo "$IDENTITY_OUT" | head -n -1)
  if [[ "$IDENTITY_HTTP" != "200" ]]; then
    echo "FAIL: metadata identity request returned HTTP $IDENTITY_HTTP (body: $IDENTITY_BODY)"
    exit 1
  fi
  if ! echo "$IDENTITY_BODY" | grep -q '"node"'; then
    echo "FAIL: metadata identity response missing node (body: $IDENTITY_BODY)"
    exit 1
  fi
  echo "OK: metadata identity lookup for $TAILSCALE_IP returned node/userProfile."
  echo "    $IDENTITY_BODY"
fi
