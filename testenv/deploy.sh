#!/usr/bin/env bash
# Full deploy: ensure VMs up, patch pod CIDRs, pre-load pause image, build & push
# image, apply DaemonSet, verify. CNI plugins are installed by Ansible (playbook.yml). Run from repo root.
#
#   ./testenv/deploy.sh
#
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KUBECONFIG="${REPO_ROOT}/testenv/kubeconfig"
export KUBECONFIG

cd "$REPO_ROOT"

VAGRANT_VMS=(control-plane node)

echo ""
echo "=== Building and pushing tailscale-cni image to both Vagrant nodes: ${VAGRANT_VMS[*]} ==="
"$SCRIPT_DIR/push-image.sh"

echo ""
echo "=== Deleting existing CNI pods ==="
kubectl -n kube-system delete pods -l app=tailscale-cni

echo ""
echo "=== Applying Tailscale CNI DaemonSet ==="
kubectl apply -f deploy/tailscale-cni-daemonset.yaml

echo ""
echo "=== Waiting for DaemonSet pods ==="
kubectl -n kube-system rollout status daemonset/tailscale-cni --timeout=120s

echo ""
echo "=== Pod status ==="
kubectl -n kube-system get pods -l app=tailscale-cni -o wide

echo ""
echo "=== Recent logs (both pods) ==="
kubectl -n kube-system logs -l app=tailscale-cni --tail=30 2>/dev/null || echo "  (kubectl logs failed; use: vagrant ssh control-plane -c \"sudo k3s crictl logs \\\$(sudo k3s crictl ps -q --name tailscale-cni | head -1)\")"

echo ""
echo "Done. Approve subnet routes in the Tailscale admin console if auto approve not enabled. Then run (from testenv): ./test-pod-network.sh"
