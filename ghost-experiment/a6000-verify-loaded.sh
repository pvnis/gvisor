#!/usr/bin/env bash
# Which nvidia.ko is ACTUALLY loaded right now?
#
# `modinfo -F filename nvidia` does NOT answer this: it resolves the module NAME
# against /lib/modules and reports the file modprobe WOULD load, regardless of
# what is resident. That is what made a6000-4 v2 declare "stock module loaded"
# immediately after a successful insmod of the GHOST build.
#
# Two checks that do work:
#   srcversion -- /sys/module/nvidia/srcversion is the LOADED module's hash;
#                 compare it against each candidate .ko on disk.
#   gpusched   -- /proc/driver/nvidia/gpusched exists ONLY in the GHOST build,
#                 so its presence is a functional fingerprint.
set -uo pipefail
GHOST=/home/ubuntu/open-gpu-kernel-modules/kernel-open/nvidia.ko
DKMS=/lib/modules/6.8.0-88-generic/updates/dkms/nvidia.ko.zst

echo "=== loaded ==="
if [ ! -d /sys/module/nvidia ]; then echo "  nvidia is NOT loaded at all"; exit 1; fi
LOADED=$(cat /sys/module/nvidia/srcversion 2>/dev/null || echo "?")
echo "  /sys/module/nvidia/srcversion = $LOADED"
echo "  version                       = $(cat /sys/module/nvidia/version 2>/dev/null || echo '?')"

echo "=== candidates on disk ==="
G=$(modinfo -F srcversion "$GHOST" 2>/dev/null || echo "?")
D=$(modinfo -F srcversion "$DKMS"  2>/dev/null || echo "?")
echo "  GHOST build ($GHOST) = $G"
echo "  stock DKMS  ($DKMS) = $D"

echo "=== verdict ==="
if   [ "$LOADED" = "$G" ] && [ "$G" != "?" ]; then echo "  >>> GHOST build is loaded"
elif [ "$LOADED" = "$D" ] && [ "$D" != "?" ]; then echo "  >>> STOCK DKMS build is loaded"
else echo "  >>> inconclusive by srcversion (they may be identical if both built from the same tree)"
fi

echo "=== functional fingerprint (GHOST-only interface) ==="
if [ -e /proc/driver/nvidia/gpusched ]; then
    echo "  /proc/driver/nvidia/gpusched PRESENT -> corrected GHOST driver"
    echo poll | sudo tee /proc/driver/nvidia/gpusched >/dev/null 2>&1 && echo "  poll accepted"
    sudo cat /proc/driver/nvidia/gpusched 2>/dev/null | head -10
else
    echo "  /proc/driver/nvidia/gpusched ABSENT -> NOT the corrected GHOST driver"
fi

echo "=== gpu alive? ==="
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader 2>&1 | head -2
