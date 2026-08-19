#!/usr/bin/env bash
# A6000 CLEAN RE-PROBE, step 2: the TEMPORAL axis. Runtime only -- no re-insmod.
#
# Two native burn.py tenants, weighted 3:1 by SET_TIMESLICE+RESTART_RUNLIST
# through /proc/driver/nvidia/gpusched. Three questions, in order:
#   1. does `ts` actually move the division?           (GB205 gave exactly 3.00:1)
#   2. is it work-conserving -- does A expand when B idles?   (design-decisive)
#   3. does `detach` evict B outright, and `attach` restore it?
#
# Judge by matmul/s, never by NvStatus.
#
# v2 fixes the measurement, which invalidated the first run entirely:
#   - v1 truncated each burn log with `: > file` between phases. python still
#     held the fd at its old offset, so the truncated file grew a NUL hole,
#     grep called it "binary file matches" and returned NOTHING. Every tenant-B
#     number came back n/a and the 3:1 ratio -- the actual result -- was lost.
#     Windows are now delimited by LINE OFFSETS; nothing is ever truncated, and
#     every grep is -a.
#   - a solo baseline is measured ON THIS DRIVER first. The 773 figure came from
#     the stock driver; comparing against it assumes the GHOST hooks are free,
#     which is exactly the sort of thing that should be measured, not assumed.
#   - transition lines after each change are discarded before averaging.
set -uo pipefail
PY=/home/ubuntu/ghostvenv/bin/python
BURN=/home/ubuntu/gvisor/ghost-experiment/burn.py
GS=/proc/driver/nvidia/gpusched
SETTLE=${SETTLE:-8}       # seconds discarded after any change
WINDOW=${WINDOW:-30}      # seconds averaged

[ -e "$GS" ] || { echo "!! $GS missing -- corrected driver not loaded"; exit 1; }

lines()  { grep -ac 'rate=' "$1" 2>/dev/null || echo 0; }
# mean of rate= lines appearing AFTER line offset $2
since()  {
    tail -n +$(( ${2:-0} + 1 )) "$1" 2>/dev/null | grep -a -oE 'rate=[0-9.]+' | cut -d= -f2 \
      | awk '{s+=$1; n++} END{ if(n>=2) printf "%.1f", s/n; else printf "n/a" }'
}
# average both tenants over a fresh window, printing A B aggregate ratio
measure() {
    local label="$1" ma mb ra rb
    sleep "$SETTLE"
    ma=$(lines /tmp/burnA.txt); mb=$(lines /tmp/burnB.txt)
    sleep "$WINDOW"
    ra=$(since /tmp/burnA.txt "$ma"); rb=$(since /tmp/burnB.txt "$mb")
    printf '    %-22s A=%-8s B=%-8s' "$label" "$ra" "$rb"
    awk -v a="$ra" -v b="$rb" 'BEGIN{
        if (a+0>0 && b+0>0) printf "  agg=%.1f  A:B=%.2f:1\n", a+b, a/b;
        else if (a+0>0)     printf "  agg=%.1f  (B idle/evicted)\n", a+0;
        else                printf "\n"; }'
}

echo "=== [0] solo baseline ON THIS DRIVER (stock-driver figure was 773) ==="
$PY "$BURN" > /tmp/burnA.txt 2>/dev/null & PA=$!
trap 'kill $PA ${PB:-} 2>/dev/null' EXIT
sleep 25
M=$(lines /tmp/burnA.txt); sleep 20
SOLO=$(since /tmp/burnA.txt "$M")
echo "    solo A = $SOLO matmul/s"
[ "$SOLO" = "n/a" ] && { echo "!! no rate lines -- burn is not producing output"; head -3 /tmp/burnA.txt; exit 1; }

echo "=== [1] second tenant, both free-running ==="
$PY "$BURN" > /tmp/burnB.txt 2>/dev/null & PB=$!
echo "    A=$PA  B=$PB"
sleep 20
[ "$(lines /tmp/burnB.txt)" = "0" ] && { echo "!! tenant B produced no rate lines"; head -3 /tmp/burnB.txt; exit 1; }
measure "unweighted"

echo "=== [2] apply 3:1 -- ts A 3000us / B 1000us ==="
echo poll | sudo tee $GS >/dev/null
echo "ts $PA 3000" | sudo tee $GS >/dev/null
echo "ts $PB 1000" | sudo tee $GS >/dev/null
sleep 3
# re-issue: the first write can land before the compute TSG exists
echo "ts $PA 3000" | sudo tee $GS >/dev/null
echo "ts $PB 1000" | sudo tee $GS >/dev/null
measure "weighted 3:1"

echo "=== [3] idle-yield: STOP B -- does A expand toward solo $SOLO? ==="
kill -STOP $PB
measure "B stopped"
kill -CONT $PB
measure "B resumed"

echo "=== [4] evict: detach B, then attach it back ==="
echo "detach $PB" | sudo tee $GS >/dev/null
measure "B detached"
echo "attach $PB" | sudo tee $GS >/dev/null
measure "B reattached"

echo
echo "=== reference ==="
echo "    solo on this driver = $SOLO      solo on stock driver = 773"
echo "    a 3:1 timeslice that binds should show A:B ~= 3 with agg ~= solo"
echo "    work-conserving => 'B stopped' A climbs to ~= solo, not stuck at its weighted share"
echo "=== dmesg ==="
sudo dmesg | grep -aiE 'GHOST' | tail -15
echo "=== proc state ==="
echo poll | sudo tee $GS >/dev/null; sudo cat $GS | head -12
echo "=== TEMPORAL DONE ==="
