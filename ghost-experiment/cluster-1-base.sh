#!/usr/bin/env bash
# vm-nv-dmd1: PHASE 0 + 1 -- revert the GHOST driver, then install the base stack.
#
# Phase 0 reverts to the stock DKMS modules WITHOUT a reboot (this box hosts the
# working session, so a reboot would drop it). Everything was insmod'd, never
# modules_install'd, so the stock modules are untouched in
# /lib/modules/6.8.0-88-generic/updates/dkms/ and modprobe picks them up.
#
# Phase 1 installs docker (needed to build runsc and the two images),
# nvidia-container-toolkit (BEFORE k3s, so k3s auto-creates the `nvidia` runtime
# handler), then k3s configured for Cilium, helm, the cilium CLI and vcluster.
set -euo pipefail
NODE_IP=10.30.30.196

echo "############ PHASE 0: revert the GHOST driver ############"
echo "--- currently loaded ---"; lsmod | grep -E '^(nvidia|nvidia_)' || echo "(none)"
sudo fuser -k /dev/nvidia* 2>/dev/null || true
sleep 1
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia; do
    sudo rmmod "$m" 2>/dev/null || true
done
if lsmod | grep -q '^nvidia '; then
    echo "!! nvidia still loaded, cannot revert"; lsmod | grep nvidia; exit 1
fi
sudo modprobe nvidia
sudo modprobe nvidia-uvm
echo "--- loaded module file (MUST be under updates/dkms, not open-gpu-kernel-modules) ---"
modinfo -F filename nvidia
if modinfo -F filename nvidia | grep -q open-gpu-kernel-modules; then
    echo "!! STILL the GHOST build"; exit 1
fi
GHOST_BEFORE=$(sudo dmesg | grep -c GHOST || true)
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader
echo "GHOST dmesg lines so far: $GHOST_BEFORE (should stop growing from here)"

echo
echo "############ PHASE 1: base stack ############"
echo "=== [1/6] docker ==="
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io
sudo usermod -aG docker ubuntu
sudo systemctl enable --now docker
sudo docker version --format '{{.Server.Version}}'

echo "=== [2/6] nvidia-container-toolkit (before k3s, so k3s finds it) ==="
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
  | sudo gpg --yes --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
  | sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list >/dev/null
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=docker --set-as-default=false || true
sudo systemctl restart docker
nvidia-container-cli --version | head -1

echo "=== [3/6] k3s config (Cilium: no flannel, no kube-proxy) ==="
sudo mkdir -p /etc/rancher/k3s
sudo tee /etc/rancher/k3s/config.yaml >/dev/null <<EOF
# Cilium supplies CNI, network policy and kube-proxy replacement, so k3s must
# not install its own. Single node, so none of sensai's Tailscale/cross-node
# addressing applies -- node-ip is just this box's only interface.
flannel-backend: none
disable-network-policy: true
disable-kube-proxy: true
egress-selector-mode: cluster
node-ip: ${NODE_IP}
write-kubeconfig-mode: "0644"
tls-san:
  - ${NODE_IP}
  - vm-nv-dmd1
EOF
cat /etc/rancher/k3s/config.yaml

echo "=== [4/6] k3s ==="
curl -sfL https://get.k3s.io | sh -
sudo systemctl is-active k3s || true
mkdir -p /home/ubuntu/.kube
sudo cp /etc/rancher/k3s/k3s.yaml /home/ubuntu/.kube/config
sudo chown ubuntu:ubuntu /home/ubuntu/.kube/config
chmod 600 /home/ubuntu/.kube/config
grep -q 'KUBECONFIG=' /home/ubuntu/.bashrc || \
    echo 'export KUBECONFIG=$HOME/.kube/config' >> /home/ubuntu/.bashrc
export KUBECONFIG=/home/ubuntu/.kube/config
k3s --version | head -1

echo "=== [5/6] helm + cilium CLI ==="
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | sudo bash
CILIUM_CLI_VERSION=$(curl -s https://raw.githubusercontent.com/cilium/cilium-cli/main/stable.txt)
curl -sL --fail -o /tmp/cilium.tar.gz \
  "https://github.com/cilium/cilium-cli/releases/download/${CILIUM_CLI_VERSION}/cilium-linux-amd64.tar.gz"
sudo tar xzf /tmp/cilium.tar.gz -C /usr/local/bin
helm version --short; cilium version --client

echo "=== [6/6] vcluster CLI (pinned, as the README does) ==="
curl -sSL -o /tmp/vcluster \
  https://github.com/loft-sh/vcluster/releases/download/v0.36.1/vcluster-linux-amd64
sudo install -m 0755 /tmp/vcluster /usr/local/bin/vcluster && rm -f /tmp/vcluster
vcluster version

echo
echo "############ STATE ############"
echo "node will be NotReady until Cilium is installed -- that is expected here."
kubectl get nodes -o wide 2>&1 | head -5
echo "=== PHASE 0+1 DONE ==="
echo "NOTE: log out and back in (or use 'sudo docker') until the docker group applies."
