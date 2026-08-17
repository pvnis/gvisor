#!/bin/bash
# ghost-revert.sh — undo the swap: unload the OPEN modules and restore the
# proprietary 610.43.02 driver (still installed in /lib/modules), then bring
# back services + desktop. Run:  sudo bash ghost-revert.sh
# (A plain reboot also reverts, since the swap used insmod, not modules_install.)
set -u
say(){ echo; echo "== $* =="; }

say "stop GPU users + display"
systemctl stop k3s 2>/dev/null || true
systemctl stop runsc-gpu-scheduler.service nvidia-persistenced 2>/dev/null || true
systemctl isolate multi-user.target
sleep 2
for c in /sys/class/vtconsole/vtcon*/; do
  grep -qi "frame buffer" "$c/name" 2>/dev/null && echo 0 > "$c/bind" 2>/dev/null || true
done
sleep 1

say "unload open modules"
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia_peermem nvidia; do
  rmmod $m 2>/dev/null && echo "  rmmod $m ok" || echo "  rmmod $m skipped"
done

say "reload proprietary from /lib/modules"
modprobe nvidia && echo "  modprobe nvidia ok" || { echo "!!! modprobe failed; dmesg:"; dmesg|tail -15; }
modprobe nvidia_modeset 2>/dev/null || true
modprobe nvidia_uvm 2>/dev/null || true
modprobe nvidia_drm modeset=1 2>/dev/null || true

say "restart services + desktop"
systemctl start nvidia-persistenced 2>/dev/null || true
systemctl start k3s 2>/dev/null || true
systemctl start runsc-gpu-scheduler.service 2>/dev/null || true
systemctl isolate graphical.target 2>/dev/null || true

say "state"
nvidia-smi --query-gpu=name,driver_version --format=csv,noheader 2>/dev/null || echo "nvidia-smi failed — may need reboot"
