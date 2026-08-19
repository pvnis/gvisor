#!/usr/bin/env bash
# GHOST on vm-nv-dmd1 (RTX A6000, GA102) -- STEP 1: driver + CUDA + solo baseline.
#
# Installs the *open* 610.43.02 driver (matching the tag already checked out in
# /home/ubuntu/open-gpu-kernel-modules), a torch venv, and measures the solo
# burn rate on the STOCK driver. That rate is the baseline the patched-driver
# run in step 2 is compared against.
#
# Nothing holds the GPU on this box (headless VM, virtio-gpu is the console),
# so there is no display manager / k3s / container to stop first.
set -euo pipefail
echo "=== [1/6] driver packages (610.43.02, open) ==="
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    nvidia-headless-610-open nvidia-utils-610 python3-venv

echo "=== [2/6] versions must match ==="
cat /proc/driver/nvidia/version || true
echo "firmware dirs:"; ls /lib/firmware/nvidia/ || true

echo "=== [3/6] nvidia-smi ==="
nvidia-smi || { echo "!! nvidia-smi failed -- stop here and report"; exit 1; }

echo "=== [4/6] torch venv (~3GB download, few minutes) ==="
if [ ! -d /home/ubuntu/ghostvenv ]; then
    python3 -m venv /home/ubuntu/ghostvenv
fi
/home/ubuntu/ghostvenv/bin/pip install -q --upgrade pip
/home/ubuntu/ghostvenv/bin/pip install -q torch --index-url https://download.pytorch.org/whl/cu128

echo "=== [5/6] device properties (TPC count sanity) ==="
/home/ubuntu/ghostvenv/bin/python - <<'PY'
import torch
p = torch.cuda.get_device_properties(0)
print("name           =", p.name)
print("SMs            =", p.multi_processor_count)
print("TPCs (SMs/2)   =", p.multi_processor_count // 2, "  <-- GHOST_TPC_COUNT should be half of this")
print("capability     = sm_%d%d" % (p.major, p.minor))
print("total VRAM MiB =", p.total_memory // (1024*1024))
PY

echo "=== [6/6] SOLO BASELINE on the STOCK driver (60s) ==="
cd /home/ubuntu/gvisor/ghost-experiment
timeout 60 /home/ubuntu/ghostvenv/bin/python burn.py || true

echo
echo "=== GHOST lines in dmesg (should be NONE -- stock driver) ==="
sudo dmesg | grep GHOST || echo "(none, as expected)"
echo "=== STEP 1 DONE ==="
