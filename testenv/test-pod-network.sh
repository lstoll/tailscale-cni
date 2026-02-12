#!/usr/bin/env bash
# Deploy test pods (2 replicas, spread across nodes) and verify:
# - Pod-to-pod connectivity (ping both ways)
# - Internet connectivity (ping 8.8.8.8 and google.com from a pod)
# Use with testenv kubeconfig.
#
#   export KUBECONFIG=$(pwd)/testenv/kubeconfig
#   ./testenv/test-pod-network.sh
#
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KUBECONFIG="${KUBECONFIG:-$REPO_ROOT/testenv/kubeconfig}"

export KUBECONFIG

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
