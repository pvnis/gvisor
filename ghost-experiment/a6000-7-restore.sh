#!/usr/bin/env bash
# A6000 RE-PROBE, cleanup: put the box back the way the cluster expects it.
#
# Restores the stock DKMS modules and brings the HAMi device plugin back, so the
# node advertises nvidia.com/gpu again and the vCluster GPU path works. A reboot
# does the same thing; this avoids one, since the working session lives on this
# box.
#
# v2: a DaemonSet cannot be scaled, so it is un-paused by REMOVING the
# unsatisfiable nodeSelector a6000-4 added -- not by `scale --replicas=1`,
# which silently errors. Holders are found via /proc rather than lsof, which is
# not installed on this image.
set -uo pipefail
export KUBECONFIG=/home/ubuntu/.kube/config

nvidia_holders() {
    for fd in /proc/[0-9]*/fd/*; do
        tgt=$(readlink "$fd" 2>/dev/null) || continue
        case "$tgt" in
            /dev/nvidia*) echo "${fd%%/fd/*}" | sed 's#/proc/##' ;;
        esac
    done | sort -un
}

echo "=== [1/4] release the GPU ==="
H=$(nvidia_holders || true)
if [ -n "$H" ]; then
    for p in $H; do echo "    killing pid=$p $(tr -d '\0' < /proc/$p/comm 2>/dev/null)"; sudo kill "$p" 2>/dev/null || true; done
    sleep 3
    for p in $(nvidia_holders || true); do sudo kill -9 "$p" 2>/dev/null || true; done
    sleep 2
fi
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia; do
    sudo rmmod "$m" 2>/dev/null || true
done
if lsmod | grep -q '^nvidia '; then
    echo "!! nvidia still loaded; remaining holders:"
    for p in $(nvidia_holders || true); do echo "   pid=$p $(tr -d '\0' < /proc/$p/comm 2>/dev/null)"; done
    exit 1
fi

echo "=== [2/4] load the stock DKMS modules ==="
sudo modprobe nvidia
sudo modprobe nvidia-uvm
# `modinfo -F filename nvidia` cannot verify this -- it reports what modprobe
# would load, not what is resident, so it says "updates/dkms" either way. The
# GHOST-only /proc interface is the reliable fingerprint.
if [ -e /proc/driver/nvidia/gpusched ]; then
    echo "!! still on a GHOST build (gpusched present) -- stock did not take"; exit 1
fi
echo "    gpusched absent -> stock module resident"
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader

echo "=== [3/4] un-pause the device plugin (remove the nodeSelector, do NOT scale) ==="
kubectl -n kube-system patch ds hami-device-plugin --type json \
    -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector/ghost~1probe-paused"}]' 2>&1 | tail -1 || \
kubectl -n kube-system patch ds hami-device-plugin --type merge \
    -p '{"spec":{"template":{"spec":{"nodeSelector":null}}}}' 2>&1 | tail -1 || true
kubectl -n kube-system rollout status ds/hami-device-plugin --timeout=180s 2>&1 | tail -2

echo "=== [4/4] node advertising GPU again? ==="
for i in $(seq 1 30); do
    kubectl get node vm-nv-dmd1 -o jsonpath='{.status.allocatable}' 2>/dev/null | grep -q 'nvidia.com/gpu' && break
    sleep 5
done
kubectl get node vm-nv-dmd1 -o json 2>/dev/null | python3 -c "
import json,sys
a=json.load(sys.stdin)['status']['allocatable']
got=[(k,v) for k,v in a.items() if 'nvidia' in k]
print('  ' + (', '.join(f'{k}={v}' for k,v in got) if got else '(none yet -- give the plugin a moment)'))"
echo "=== RESTORE DONE ==="
