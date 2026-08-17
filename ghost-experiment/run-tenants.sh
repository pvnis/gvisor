#!/bin/bash
# run-tenants.sh <tag> <ntenants> <seconds>
# Launches N burn.py tenants at the same instant, lets them run <seconds>, kills
# them, and reports each one's median rate after the ramp. Results land in $OUT.
#   PY=... BURN=... OUT=... bash run-tenants.sh disj27 2 75
# Use run-staggered.sh instead when the driver assigns per-tenant partitions in
# arrival order and you need to know which tenant got which.
set -u
TAG="$1"; N="$2"; SECS="${3:-70}"
PY="${PY:-python3}"
BURN="${BURN:-$(dirname "$0")/burn.py}"
OUT="${OUT:-$HOME/ghost-a100}"
mkdir -p "$OUT"

pids=()
for i in $(seq 1 "$N"); do
  $PY "$BURN" > "$OUT/${TAG}-t${i}.txt" 2>&1 &
  pids+=($!)
done
sleep "$SECS"
for p in "${pids[@]}"; do kill -9 "$p" 2>/dev/null; done
wait 2>/dev/null
sleep 2

echo "== $TAG ($N tenants, ${SECS}s) =="
for i in $(seq 1 "$N"); do
  f="$OUT/${TAG}-t${i}.txt"
  # drop the first 3 samples (ramp), report count/median
  vals=$(grep -o 'rate=[0-9.]*' "$f" | cut -d= -f2 | tail -n +4)
  n=$(echo "$vals" | grep -c .)
  med=$(echo "$vals" | sort -n | awk '{a[NR]=$1} END{if(NR)print (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2}')
  echo "  tenant$i: samples=$n median=${med:-NA} matmul/s"
done
