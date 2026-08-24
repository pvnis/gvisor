#!/usr/bin/env bash
# A6000 CLEAN RE-PROBE, step 1: swap in the CORRECTED GHOST driver.
#
# This is the driver that fixes what made the 2026-08-17 A6000 run void:
#   - 0c/0d re-probed from the DEFERRED GpFifoSchedule site, not the ctxshare
#     constructor (where GSP returns 0x57 = OBJECT_NOT_FOUND for a handle whose
#     construction has not finished, which says nothing about the die)
#   - SET_TIMESLICE / detach now paired with RESTART_RUNLIST, without which GSP
#     never commits the change
#   - /proc/driver/nvidia/gpusched runtime control (poll / ts / detach / attach)
#
# Source: pvnis/open-gpu-kernel-modules branch ghost-experiment, base 610.43.02
# (matches the installed firmware exactly), GHOST_TOTAL_TPC set to 42 for GA102.
#
#   bash a6000-4-corrected-swap.sh                # temporal run (no TPC table)
#   GHOST_TPC=21 bash a6000-4-corrected-swap.sh   # spatial run, 21 of 42 TPCs
#
# !! DISRUPTIVE. Unloads the GPU driver. The node stops advertising
# !! nvidia.com/gpu until a6000-7-restore.sh runs. Tenant control planes and
# !! non-GPU pods are unaffected. Uses insmod, never modules_install, so a
# !! reboot also restores everything.
#
# v2 fixes two silent failures in v1 that left the driver pinned at refcount 11:
#   - `kubectl scale ds --replicas=0` DOES NOT WORK on a DaemonSet. It errors,
#     `|| true` hides it, and the device plugin keeps /dev/nvidia* open. A
#     DaemonSet is stood down with an unsatisfiable nodeSelector, or deleted.
#   - lsof/fuser are not installed on this image, so `lsof || echo "(none)"`
#     printed a reassuring "(none)" that meant nothing and `fuser -k` killed
#     nothing. Holders are now found by walking /proc/*/fd, which always works.
set -euo pipefail
SRC=/home/ubuntu/open-gpu-kernel-modules
TPC=${GHOST_TPC:-0}            # 0 => leave the TPC table alone (temporal-only)
DISJOINT=${GHOST_DISJOINT:-1}
export KUBECONFIG=/home/ubuntu/.kube/config

# List pids holding any /dev/nvidia* fd, without lsof.
nvidia_holders() {
    for fd in /proc/[0-9]*/fd/*; do
        tgt=$(readlink "$fd" 2>/dev/null) || continue
        case "$tgt" in
            /dev/nvidia*) echo "${fd%%/fd/*}" | sed 's#/proc/##' ;;
        esac
    done | sort -un
}

echo "=== [1/6] stand the cluster's GPU consumers down ==="
# nodeSelector that matches nothing -> kubelet tears the DaemonSet pod down.
kubectl -n kube-system patch ds hami-device-plugin --type merge \
    -p '{"spec":{"template":{"spec":{"nodeSelector":{"ghost/probe-paused":"true"}}}}}' 2>&1 | tail -1 || true
kubectl -n gpu-test delete pod --all --wait=false 2>/dev/null || true
sudo systemctl stop nvidia-persistenced 2>/dev/null || true
echo "    waiting for the device plugin pod to go away"
for i in $(seq 1 30); do
    n=$(kubectl get pods -n kube-system --no-headers 2>/dev/null | grep -c 'hami-device-plugin' || true)
    [ "$n" = "0" ] && break
    sleep 2
done
kubectl get pods -n kube-system 2>/dev/null | grep hami || echo "    (no hami pods left)"

echo "=== [2/6] find and clear /dev/nvidia* holders (via /proc, not lsof) ==="
H=$(nvidia_holders || true)
if [ -n "$H" ]; then
    echo "    holders:"
    for p in $H; do echo "      pid=$p  $(tr -d '\0' < /proc/$p/comm 2>/dev/null)"; done
    for p in $H; do sudo kill "$p" 2>/dev/null || true; done
    sleep 3
    H2=$(nvidia_holders || true)
    [ -n "$H2" ] && { echo "    still held, forcing:"; for p in $H2; do sudo kill -9 "$p" 2>/dev/null || true; done; sleep 2; }
else
    echo "    none"
fi

echo "=== [3/6] unload (drm/modeset FIRST, or rmmod nvidia silently fails) ==="
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia; do
    sudo rmmod "$m" 2>/dev/null || true
done
if lsmod | grep -q '^nvidia '; then
    echo "!! nvidia STILL loaded -- refusing to continue, this would measure the OLD module"
    lsmod | grep '^nvidia'
    echo "   refcount above is the number of holders still open."
    echo "   remaining holders:"
    for p in $(nvidia_holders || true); do echo "     pid=$p $(tr -d '\0' < /proc/$p/comm 2>/dev/null)"; done
    echo "   NOTE: a wedged gVisor sandbox shows up as 'exe', not runsc-sandbox."
    exit 1
fi

echo "=== [4/6] insmod the corrected build ==="
if [ "$TPC" != "0" ]; then
    REG="GhostTpcCount=${TPC};GhostDisjoint=${DISJOINT}"
    echo "    spatial: NVreg_RegistryDwords=\"$REG\""
    sudo insmod "$SRC"/kernel-open/nvidia.ko NVreg_RegistryDwords="$REG"
else
    echo "    temporal-only: no TPC table imposed"
    sudo insmod "$SRC"/kernel-open/nvidia.ko
fi
sudo insmod "$SRC"/kernel-open/nvidia-uvm.ko
if [ ! -e /dev/nvidia0 ]; then
    NVMAJ=$(awk '$2=="nvidia-frontend"||$2=="nvidia"{print $1}' /proc/devices | head -1)
    UVMMAJ=$(awk '$2=="nvidia-uvm"{print $1}' /proc/devices | head -1)
    sudo mknod -m 666 /dev/nvidia0    c "$NVMAJ"  0   2>/dev/null || true
    sudo mknod -m 666 /dev/nvidiactl  c "$NVMAJ"  255 2>/dev/null || true
    sudo mknod -m 666 /dev/nvidia-uvm c "$UVMMAJ" 0   2>/dev/null || true
fi

echo "=== [5/6] confirm it is OUR module, not the stock DKMS one ==="
# NOT `modinfo -F filename nvidia`: that resolves the module NAME against
# /lib/modules and reports what modprobe WOULD load, not what is resident. It
# says "updates/dkms/..." even immediately after a successful insmod of this
# build. Compare the LOADED module's srcversion instead.
LOADED=$(cat /sys/module/nvidia/srcversion 2>/dev/null || echo "?")
OURS=$(modinfo -F srcversion "$SRC"/kernel-open/nvidia.ko 2>/dev/null || echo "?")
echo "    loaded srcversion = $LOADED"
echo "    ours   srcversion = $OURS"
if [ "$LOADED" != "$OURS" ] || [ "$OURS" = "?" ]; then
    echo "!! loaded module does not match the build we just inserted"
    exit 1
fi
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader

echo "=== [6/6] runtime control interface ==="
ls -l /proc/driver/nvidia/gpusched 2>&1 || {
    echo "!! no gpusched proc entry -- wrong build"; exit 1; }
echo poll | sudo tee /proc/driver/nvidia/gpusched >/dev/null && echo "poll accepted"
sudo cat /proc/driver/nvidia/gpusched | head -10
echo "=== SWAP DONE ==="
