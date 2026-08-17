#!/bin/bash
# ghost-swap.sh — swap sensai's RTX 5070 from proprietary 610.43.02 to the
# OPEN 610.43.02 modules built in /home/dmd/open-gpu-kernel-modules/kernel-open.
# Reversible: uses insmod (not modules_install), so a reboot reverts to
# proprietary. Stops after verifying nvidia-smi on the open driver; run
# ghost-restore.sh afterwards to bring back k3s/gdm, or ghost-revert.sh to undo.
# Run:  sudo bash ghost-swap.sh
set -u
KO=/home/dmd/open-gpu-kernel-modules/kernel-open
say(){ echo; echo "== $* =="; }

say "Stage 1: quiesce GPU users + display"
systemctl stop k3s 2>/dev/null || true
systemctl stop runsc-gpu-scheduler.service nvidia-persistenced 2>/dev/null || true
systemctl isolate multi-user.target        # stops gdm / graphical session
sleep 3

say "unbind framebuffer console from nvidia_drm"
for c in /sys/class/vtconsole/vtcon*/; do
  if grep -qi "frame buffer" "$c/name" 2>/dev/null; then
    echo "  unbinding $c ($(cat $c/name))"; echo 0 > "$c/bind" 2>/dev/null || true
  fi
done
sleep 1

say "holders now (want nvidia_drm/modeset/uvm used=0)"
lsmod | grep -E "^nvidia" | awk '{print "  "$1, "used="$3}'
fuser -v /dev/nvidia* /dev/dri/* 2>&1 | head || true

say "Stage 2: unload proprietary"
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia; do
  rmmod $m 2>/dev/null && echo "  rmmod $m ok" || echo "  rmmod $m skipped/failed"
done
if lsmod | grep -q "^nvidia "; then
  echo "!!! nvidia still loaded — something holds it. STOP; paste 'lsmod | grep nvidia' and 'sudo fuser -v /dev/nvidia* /dev/dri/*'."
  exit 1
fi
echo "  proprietary fully unloaded."

say "Stage 3: load OPEN modules ($(modinfo -F version $KO/nvidia.ko))"
insmod $KO/nvidia.ko NVreg_OpenRmEnableUnsupportedGpus=1 && echo "  nvidia.ko ok" || { echo "!!! insmod nvidia.ko failed — dmesg tail:"; dmesg | tail -20; exit 1; }
insmod $KO/nvidia-modeset.ko && echo "  nvidia-modeset.ko ok" || echo "  modeset failed"
insmod $KO/nvidia-uvm.ko && echo "  nvidia-uvm.ko ok" || echo "  uvm failed"
insmod $KO/nvidia-drm.ko modeset=1 && echo "  nvidia-drm.ko ok" || echo "  drm failed"
echo "  loaded:"; lsmod | grep -E "^nvidia" | awk '{print "   "$1, $3}'

say "Stage 4: verify GPU on the open driver"
sleep 2
if nvidia-smi --query-gpu=name,driver_version --format=csv,noheader; then
  echo
  echo ">>> OPEN DRIVER LOADED OK. GSP/init from dmesg:"
  dmesg | grep -iE "nvidia|gsp" | tail -8
  echo
  echo ">>> Next: 'sudo bash ghost-restore.sh' to bring back services + desktop,"
  echo ">>>       or 'sudo bash ghost-revert.sh' to go back to proprietary."
else
  echo "!!! nvidia-smi FAILED on the open driver. dmesg tail:"; dmesg | tail -25
  echo "!!! Recover with: sudo bash ghost-revert.sh"
  exit 1
fi
