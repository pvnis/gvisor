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
# GHOST on vm-nv-dmd1 (RTX A6000, GA102) -- STEP 2: the 0c/0d probes.
#
# Applies the GHOST hooks to the open 610.43.02 tree, sets the TPC partition to
# half the A6000's 42 TPCs, builds, swaps the stock modules for the patched
# ones with insmod (so a REBOOT FULLY REVERTS), runs the burn, and prints the
# GHOST control statuses.
#
#   GHOST_TPC=21 bash a6000-2-probe.sh     # override the half-partition size
set -euo pipefail
SRC=/home/ubuntu/open-gpu-kernel-modules
TPC=${GHOST_TPC:-21}          # A6000 = 84 SMs = 42 TPCs; half = 21
JOBS=$(nproc)

echo "=== [1/7] apply GHOST hooks at tag $(cd $SRC && git describe --tags) ==="
cd "$SRC"
git checkout -- src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c \
                src/nvidia/src/kernel/gpu/fifo/kernel_channel_group_api.c
git apply /home/ubuntu/gvisor/ghost-experiment/driver-hooks.patch
sed -i "s/GHOST_TPC_COUNT = 12/GHOST_TPC_COUNT = $TPC/" \
    src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c
grep -n "GHOST_TPC_COUNT = " src/nvidia/src/kernel/gpu/fifo/kernel_ctxshare.c
git diff --stat

echo "=== [2/7] build (-j$JOBS, several minutes) ==="
make modules -j"$JOBS" >/tmp/ghost-build.log 2>&1 || {
    echo "!! BUILD FAILED -- last 40 lines:"; tail -40 /tmp/ghost-build.log; exit 1; }
ls -l "$SRC"/kernel-open/nvidia.ko "$SRC"/kernel-open/nvidia-uvm.ko

echo "=== [3/7] release the GPU ==="
sudo systemctl stop nvidia-persistenced 2>/dev/null || true
sudo fuser -k /dev/nvidia* 2>/dev/null || true
sleep 2
sudo lsof /dev/nvidia* 2>/dev/null || echo "(no holders)"

echo "=== [4/7] unload stock modules ==="
sudo rmmod nvidia_drm nvidia_modeset 2>/dev/null || true
sudo rmmod nvidia_uvm 2>/dev/null || true
sudo rmmod nvidia_peermem 2>/dev/null || true
sudo rmmod nvidia 2>/dev/null || true
lsmod | grep -c nvidia || echo "all nvidia modules unloaded"

echo "=== [5/7] insmod the PATCHED modules (reboot reverts) ==="
sudo insmod "$SRC"/kernel-open/nvidia.ko NVreg_OpenRmEnableUnsupportedGpus=1 \
  || sudo insmod "$SRC"/kernel-open/nvidia.ko
sudo insmod "$SRC"/kernel-open/nvidia-uvm.ko
# this package set has no nvidia-modprobe; udev normally recreates the nodes on
# module load, but create them by hand if it did not.
if [ ! -e /dev/nvidia0 ]; then
    NVMAJ=$(awk '$2=="nvidia-frontend"||$2=="nvidia"{print $1}' /proc/devices | head -1)
    UVMMAJ=$(awk '$2=="nvidia-uvm"{print $1}' /proc/devices | head -1)
    echo "recreating device nodes (nvidia major=$NVMAJ uvm major=$UVMMAJ)"
    sudo mknod -m 666 /dev/nvidia0   c "$NVMAJ" 0   2>/dev/null || true
    sudo mknod -m 666 /dev/nvidiactl c "$NVMAJ" 255 2>/dev/null || true
    sudo mknod -m 666 /dev/nvidia-uvm c "$UVMMAJ" 0 2>/dev/null || true
fi
ls -l /dev/nvidia*
lsmod | grep nvidia
nvidia-smi || { echo "!! GPU did not come up on the patched driver"; exit 1; }

echo "=== [6/7] burn on the PATCHED driver (60s) ==="
echo "    STOCK-DRIVER SOLO BASELINE = 773 matmul/s (measured, +/-0.3%)"
echo "    a real 21-of-42-TPC partition should land near 386"
cd /home/ubuntu/gvisor/ghost-experiment
# write to a file: timeout kills buffered pipeline output otherwise
timeout 60 /home/ubuntu/ghostvenv/bin/python burn.py >/tmp/burn-patched.txt 2>/dev/null || true
tail -12 /tmp/burn-patched.txt

echo
echo "=== [7/7] THE RESULT ==="
sudo dmesg | grep GHOST || echo "!! NO GHOST LINES -- hooks did not fire (wrong module loaded?)"
echo "=== STEP 2 DONE ==="
