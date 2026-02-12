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

# echo "=== Vagrant status ==="
# (cd "$SCRIPT_DIR" && vagrant status)

# echo ""
# echo "=== Ensuring VMs are up ==="
# (cd "$SCRIPT_DIR" && vagrant up)

# echo ""
# echo "=== Patching pod CIDRs on all nodes (K3s with no flannel may not set these) ==="
# export KUBECONFIG
# # Discover cluster nodes and assign each a /24 from 10.99.0.0/16 so both nodes advertise
# NODES=($(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'))
# if [[ ${#NODES[@]} -eq 0 ]]; then
#   echo "  No nodes found. Is the cluster up? (vagrant up, then retry)"
#   exit 1
# fi
# echo "  Found ${#NODES[@]} node(s): ${NODES[*]}"
# for i in "${!NODES[@]}"; do
#   # 10.99.0.0/24, 10.99.1.0/24, ...
#   CIDR="10.99.${i}.0/24"
#   echo "  Patching ${NODES[$i]} -> podCIDR $CIDR"
#   kubectl patch node "${NODES[$i]}" --type=merge -p "{\"spec\":{\"podCIDR\":\"$CIDR\"}}"
# done

# Both Vagrant VMs (must match Vagrantfile: control-plane, node)
VAGRANT_VMS=(control-plane node)

# echo ""
# echo "=== Pre-loading pause image on both Vagrant nodes: ${VAGRANT_VMS[*]} ==="
# PAUSE_IMAGE="${PAUSE_IMAGE:-rancher/mirrored-pause:3.6}"
# if ! docker image inspect "$PAUSE_IMAGE" &>/dev/null; then
#   docker pull --platform linux/arm64 "$PAUSE_IMAGE"
# fi
# PAUSE_TAR=$(mktemp -t pause-image.XXXXXX.tar)
# trap "rm -f $PAUSE_TAR" EXIT
# docker save "$PAUSE_IMAGE" -o "$PAUSE_TAR"
# for vm in "${VAGRANT_VMS[@]}"; do
#   echo "  $vm..."
#   (cd "$SCRIPT_DIR" && vagrant ssh "$vm" -c "sudo k3s ctr -n k8s.io images import -" < "$PAUSE_TAR")
# done
# rm -f "$PAUSE_TAR"

echo ""
echo "=== Building and pushing tailscale-cni image to both Vagrant nodes: ${VAGRANT_VMS[*]} ==="
"$SCRIPT_DIR/push-image.sh"

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
