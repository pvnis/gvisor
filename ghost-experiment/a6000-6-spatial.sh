#!/usr/bin/env bash
# A6000 CLEAN RE-PROBE, step 3: the SPATIAL axis.
#
# SET_TPC_PARTITION_TABLE is now issued from the DEFERRED GpFifoSchedule site.
# The width comes from a registry knob read at module load, so each width needs
# its own insmod -- hence this drives a6000-4 once per width and runs one burn.
#
#   bash a6000-6-spatial.sh            # sweep 21 32 42
#   TPCS="13 21 42" bash a6000-6-spatial.sh
#
# What to look for, per NVIDIA-COMPUTE-ISOLATION.md CORRECTION 5:
#   - `granted N TPCs ... SETTBL(N)=0x0` on the PRIMARY compute ctxshare
#   - throughput scaling roughly linearly with N against the 773 solo baseline
#     (42/42 should sit at ~773, 21/42 near ~386)
# Secondary torch ctxshares may still report 0x56/0x57 -- those are not the
# compute context; ignore them. The compute ctxshare plus the burn rate is the
# whole signal. A status of 0x0 with unmoved throughput means accepted-not-
# enforced, which is a different (and weaker) answer than enforced.
set -uo pipefail
PY=/home/ubuntu/ghostvenv/bin/python
BURN=/home/ubuntu/gvisor/ghost-experiment/burn.py
SWAP=/home/ubuntu/gvisor/ghost-experiment/a6000-4-corrected-swap.sh
TPCS=${TPCS:-"21 32 42"}
RESULTS=/tmp/a6000-spatial-results.txt
: > $RESULTS

# Solo reference measured on the GHOST driver with NO TPC table imposed. The
# 773 figure came from the stock driver, before k3s existed on this box, so it
# is not comparable -- measured here at 614.4, a gap not yet attributed
# (hooks vs host CPU contention). Override with SOLO=<n> to skip this pass.
if [ -z "${SOLO:-}" ]; then
    echo "################ solo reference (no TPC table) ################"
    GHOST_TPC=0 bash "$SWAP" > /tmp/swap-solo.log 2>&1 || {
        echo "!! swap failed for the solo pass"; tail -15 /tmp/swap-solo.log; exit 1; }
    timeout 70 $PY "$BURN" > /tmp/burn-solo.txt 2>/dev/null || true
    SOLO=$(tail -n 12 /tmp/burn-solo.txt | grep -a -oE 'rate=[0-9.]+' | cut -d= -f2 \
           | awk '{s+=$1;n++} END{if(n>=2) printf "%.1f", s/n; else printf "0"}')
    echo "solo (42/42 TPCs, table untouched) = $SOLO matmul/s"
    [ "$SOLO" = "0" ] && { echo "!! solo pass produced no rate lines"; exit 1; }
fi

for N in $TPCS; do
    echo "################ $N of 42 TPCs ################"
    GHOST_TPC=$N bash "$SWAP" > /tmp/swap-$N.log 2>&1 || {
        echo "!! swap failed for N=$N"; tail -15 /tmp/swap-$N.log; continue; }
    sudo dmesg -C >/dev/null 2>&1 || true
    timeout 70 $PY "$BURN" > /tmp/burn-$N.txt 2>/dev/null || true
    R=$(tail -n 12 /tmp/burn-$N.txt | grep -oE 'rate=[0-9.]+' | cut -d= -f2 \
        | awk '{s+=$1;n++} END{if(n) printf "%.1f", s/n; else printf "n/a"}')
    echo "--- dmesg, deferred-site grant ---"
    sudo dmesg | grep -a -iE 'granted|SETTBL|SET_TPC' | tail -6
    EXPECT=$(awk -v n="$N" -v s="$SOLO" 'BEGIN{printf "%.0f", s*n/42}')
    echo "TPC=$N  rate=$R  (linear expectation ~$EXPECT of solo $SOLO)" | tee -a $RESULTS
done

echo
echo "################ SUMMARY ################"
echo "solo on THIS driver, whole GPU: $SOLO matmul/s"
echo "(the 773 figure came from the STOCK driver and is NOT the right yardstick;"
echo " the measured gap is unattributed -- hooks vs host CPU load from k3s.)"
cat $RESULTS
echo "=== SPATIAL DONE ==="
