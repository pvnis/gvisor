#!/bin/bash
# ghost-reload.sh — reload the rebuilt OPEN modules after editing driver source.
# Assumes the desktop is already off and only nvidia+nvidia_uvm are loaded (the
# state ghost-swap.sh left). Stops k3s + GPU services, swaps the modules, brings
# services back, and kicks the device-plugin. Run: sudo bash ghost-reload.sh
set -u
KO=/home/dmd/open-gpu-kernel-modules/kernel-open
say(){ echo; echo "== $* =="; }

say "stop GPU users"
systemctl stop k3s 2>/dev/null || true
systemctl stop runsc-gpu-scheduler.service nvidia-persistenced 2>/dev/null || true
sleep 2

# systemctl stop k3s leaves the pods' containers running under containerd
# (device-plugin, vgpu-monitor, etc.) still holding /dev/nvidia*. Kill anything
# holding a GPU device node so the module can unload.
scan_holders(){ for f in /proc/[0-9]*/fd/*; do t=$(readlink "$f" 2>/dev/null); case "$t" in
  */nvidia0|*/nvidiactl|*/nvidia-uvm|*/nvidia-caps/*) echo "$f" | cut -d/ -f3;; esac; done 2>/dev/null | sort -u; }
say "kill leftover GPU-holding containers"
h=$(scan_holders); [ -n "$h" ] && { echo "  TERM: $h"; kill $h 2>/dev/null; sleep 3; }
h=$(scan_holders); [ -n "$h" ] && { echo "  KILL: $h"; kill -9 $h 2>/dev/null; sleep 2; }

say "unload current modules"
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia; do
  rmmod $m 2>/dev/null && echo "  rmmod $m ok" || true
done
if lsmod | grep -q "^nvidia "; then
  echo "!!! nvidia still loaded — holders:"; fuser -v /dev/nvidia* 2>&1 | head; exit 1
fi

say "load rebuilt modules ($(modinfo -F version $KO/nvidia.ko))"
insmod $KO/nvidia.ko NVreg_OpenRmEnableUnsupportedGpus=1 || { echo "!!! insmod nvidia failed"; dmesg | tail -15; exit 1; }
insmod $KO/nvidia-uvm.ko || echo "  uvm insmod failed"

say "verify GPU"
if ! nvidia-smi --query-gpu=name,driver_version --format=csv,noheader; then
  echo "!!! nvidia-smi failed on rebuilt driver; dmesg:"; dmesg | tail -20; exit 1
fi

say "restart services + kick device-plugin"
sysctl -w fs.inotify.max_user_instances=1024 >/dev/null 2>&1 || true
systemctl start nvidia-persistenced 2>/dev/null || true
systemctl start k3s 2>/dev/null || true
systemctl start runsc-gpu-scheduler.service 2>/dev/null || true
sleep 8
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
dp=$(k3s kubectl get pods -n kube-system 2>/dev/null | awk '/hami-device-plugin/{print $1}')
[ -n "$dp" ] && k3s kubectl delete pod -n kube-system "$dp" --wait=false 2>/dev/null
echo "reloaded. GHOST log lines will appear in dmesg once a CUDA ctxshare is created."
