#!/bin/bash
# run-staggered.sh <tag> <ntenants> <seconds>
# Same as run-tenants.sh but starts each tenant 8s apart, so the driver's slice
# index — and therefore which TPC range/width a tenant is granted — is
# deterministic: tenant1 = slice 0, tenant2 = slice 1, ... Rates are the median
# of the last 20 samples, i.e. taken once every tenant is running.
#   PY=... BURN=... OUT=... bash run-staggered.sh w40_14 2 90
set -u
TAG="$1"; N="$2"; SECS="${3:-80}"
PY="${PY:-python3}"
BURN="${BURN:-$(dirname "$0")/burn.py}"
OUT="${OUT:-$HOME/ghost-a100}"
mkdir -p "$OUT"

pids=()
for i in $(seq 1 "$N"); do
  $PY "$BURN" > "$OUT/${TAG}-t${i}.txt" 2>&1 &
  pids+=($!)
  [ "$i" -lt "$N" ] && sleep 8
done
sleep "$SECS"
for p in "${pids[@]}"; do kill -9 "$p" 2>/dev/null; done
wait 2>/dev/null
sleep 2

echo "== $TAG ($N tenants staggered, ${SECS}s) =="
for i in $(seq 1 "$N"); do
  f="$OUT/${TAG}-t${i}.txt"
  vals=$(grep -o 'rate=[0-9.]*' "$f" | cut -d= -f2 | tail -20)
  n=$(echo "$vals" | grep -c .)
  med=$(echo "$vals" | sort -n | awk '{a[NR]=$1} END{if(NR)print (NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2}')
  echo "  tenant$i: samples=$n median(last20)=${med:-NA} matmul/s"
done
