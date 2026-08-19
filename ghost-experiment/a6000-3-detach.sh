#!/usr/bin/env bash
#
# !! SUPERSEDED CALL SITE -- READ BEFORE TRUSTING ANY NUMBER FROM THIS SCRIPT !!
# The committed driver-hooks.patch originates 0c/0d from inside
# kctxshareapiConstruct_IMPL, where GSP cannot resolve the ctxshare handle yet,
# so it returns 0x57 = NV_ERR_OBJECT_NOT_FOUND -- NOT NV_ERR_NOT_SUPPORTED
# (0x56). A 0x57 from here says nothing about the die. Likewise 0b without the
# NVA06F_CTRL_CMD_RESTART_RUNLIST companion is never committed by GSP, so it is
# accepted and inert regardless of hardware. See NVIDIA-COMPUTE-ISOLATION.md
# CORRECTION 3 and CORRECTION 5. Move the hook to the deferred GPFIFO_SCHEDULE
# call site first; the 2026-08-17 A6000 run using this script is VOID.
#
# GHOST on vm-nv-dmd1 (RTX A6000, GA102) -- STEP 3: the 0b temporal probe.
#
# Flips bGhostDetach to NV_TRUE, so RM force-disables each TSG right after the
# client enables it, and asks whether a driver-level detach STICKS against a
# doorbell workload. Run this ONLY after step 2, and read its result separately:
# a detach that works stalls the burn, so it cannot share a run with a
# throughput measurement.
#
# GHOST_TPC defaults to 42 here -- the A6000's FULL TPC count -- so that if the
# 0c spatial hook did turn out to work in step 2, it imposes a no-op whole-GPU
# partition and cannot confound the throughput reading for 0b.
set -euo pipefail
SRC=/home/ubuntu/open-gpu-kernel-modules
TPC=${GHOST_TPC:-42}
JOBS=$(nproc)

echo "=== [1/6] re-apply hooks with detach ENABLED ==="
cd "$SRC"
git checkout -- src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c \
                src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c
git apply /home/ubuntu/gvisor/ghost-experiment/driver-hooks.patch
sed -i "s/GHOST_TPC_COUNT = 12/GHOST_TPC_COUNT = $TPC/" \
    src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c
sed -i "s/const NvBool bGhostDetach = NV_FALSE;/const NvBool bGhostDetach = NV_TRUE;/" \
    src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c
grep -n "bGhostDetach = NV_" src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c

echo "=== [2/6] build ==="
make modules -j"$JOBS" >/tmp/ghost-build-0b.log 2>&1 || {
    echo "!! BUILD FAILED:"; tail -40 /tmp/ghost-build-0b.log; exit 1; }

echo "=== [3/6] release the GPU ==="
sudo systemctl stop nvidia-persistenced 2>/dev/null || true
sudo fuser -k /dev/nvidia* 2>/dev/null || true
sleep 2

echo "=== [4/6] reload ==="
# nvidia_modeset <- nvidia_drm hold a reference to nvidia; without unloading them
# first, `rmmod nvidia` fails and insmod then reports "File exists" while the OLD
# module is still resident -- i.e. it silently measures the previous build.
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia; do
    sudo rmmod "$m" 2>/dev/null || true
done
if lsmod | grep -q '^nvidia '; then
    echo "!! nvidia is STILL loaded -- refusing to continue (would measure the old module)"
    lsmod | grep -E '^(nvidia|nvidia_)'
    echo "   holders:"; sudo lsof /dev/nvidia* 2>/dev/null || echo "   (none -- an unlisted module still references it)"
    exit 1
fi
echo "all nvidia modules unloaded; inserting the 0b build"
sudo insmod "$SRC"/kernel-open/nvidia.ko NVreg_OpenRmEnableUnsupportedGpus=1 \
  || sudo insmod "$SRC"/kernel-open/nvidia.ko
sudo insmod "$SRC"/kernel-open/nvidia-uvm.ko
if [ ! -e /dev/nvidia0 ]; then
    NVMAJ=$(awk '$2=="nvidia-frontend"||$2=="nvidia"{print $1}' /proc/devices | head -1)
    UVMMAJ=$(awk '$2=="nvidia-uvm"{print $1}' /proc/devices | head -1)
    sudo mknod -m 666 /dev/nvidia0   c "$NVMAJ" 0   2>/dev/null || true
    sudo mknod -m 666 /dev/nvidiactl c "$NVMAJ" 255 2>/dev/null || true
    sudo mknod -m 666 /dev/nvidia-uvm c "$UVMMAJ" 0 2>/dev/null || true
fi
nvidia-smi --query-gpu=name,driver_version --format=csv

echo "=== [5/6] burn with the TSG force-detached (90s) ==="
echo "    STALLS / no 'rate=' lines -> detach STICKS, temporal viable"
echo "    ~773 matmul/s (the measured solo baseline) -> ineffective (the 5070 result)"
cd /home/ubuntu/gvisor/ghost-experiment
timeout 90 /home/ubuntu/ghostvenv/bin/python burn.py >/tmp/burn-0b.txt 2>/dev/null || true
echo "--- burn output (empty = stalled) ---"; tail -12 /tmp/burn-0b.txt

echo
echo "=== [6/6] THE RESULT ==="
N0B=$(sudo dmesg | grep -c "GHOST 0b" || true)
echo "GHOST 0b lines seen: $N0B"
if [ "$N0B" = "0" ]; then
    echo "!! ZERO 0b lines -- the detach hook never fired, so the loaded module is"
    echo "   NOT the 0b build. Any rate below is meaningless. Do not interpret it."
fi
sudo dmesg | grep "GHOST 0b" | tail -20
echo "=== STEP 3 DONE ==="
