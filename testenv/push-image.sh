#!/usr/bin/env bash
# Build the tailscale-cni image and load it onto each Vagrant node so the
# DaemonSet can run without a registry. Run from the repo root.
#
#   ./testenv/push-image.sh
#
set -e

command -v zstd >/dev/null || { echo "zstd is required (e.g. brew install zstd)"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-tailscale-cni:latest}"

cd "$REPO_ROOT"

echo "Building $IMAGE_NAME..."
docker build -t "$IMAGE_NAME" .

echo "Saving image and compressing with zstd..."
TMP_TAR=$(mktemp -t tailscale-cni-image.XXXXXX.tar.zst)
rm -f "$TMP_TAR"  # mktemp created it; zstd won't overwrite existing when reading stdin
trap "rm -f $TMP_TAR" EXIT
docker save "$IMAGE_NAME" | zstd -o "$TMP_TAR"

for vm in control-plane node; do
  echo "Loading image on $vm..."
  # K3s uses embedded containerd; k8s.io is the namespace for cluster images
  (cd "$SCRIPT_DIR" && zstd -d -c "$TMP_TAR" | vagrant ssh "$vm" -c "sudo k3s ctr -n k8s.io images import -")
done

echo "Done. Image $IMAGE_NAME is available on both nodes. Apply the DaemonSet:"
echo "  kubectl apply -f $REPO_ROOT/deploy/tailscale-cni-daemonset.yaml"
