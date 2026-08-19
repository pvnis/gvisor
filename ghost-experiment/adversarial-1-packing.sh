#!/usr/bin/env bash
# ADVERSARIAL 1: does the RUNLIST enforcer survive the attack that killed the
# compute gate?
#
# SECURITY-FINDINGS.md V1/V2: a weight-25 tenant that forked 5 identical burners
# took 312 matmul/s while two weight-100 peers fell to 62 each, total conserved
# -- theft, not gap-filling. Root cause: the gate keyed enforcement on a
# per-sandbox fault signal, so N processes bought N shares.
#
# The runlist enforcer is a different mechanism (per-TSG timeslice committed by
# RESTART_RUNLIST), so the attack may or may not carry over. It is the single
# most important thing to know about it before anyone relies on it.
#
# THE ATTACK: A is weighted 1/3 of B. A then forks N processes. If A's TOTAL
# rises with N, packing works and the enforcer divides per-CHANNEL-GROUP rather
# than per-tenant -- the same class of hole, in a new mechanism.
#
# Native processes, no k8s: the mechanism is what is under test, and each
# burn.py is its own pid/TSG exactly as each packed process was in V1.
#
#   bash adversarial-1-packing.sh          # N = 1 then 4
#   PACK=6 bash adversarial-1-packing.sh
set -uo pipefail
PY=/home/ubuntu/ghostvenv/bin/python
BURN=/home/ubuntu/gvisor/ghost-experiment/burn.py
GS=/proc/driver/nvidia/gpusched
PACK=${PACK:-4}
SETTLE=${SETTLE:-10}
WINDOW=${WINDOW:-30}
TS_A=${TS_A:-1000}     # attacker: the LOW weight
TS_B=${TS_B:-3000}     # victim:   the HIGH weight

[ -e "$GS" ] || { echo "!! $GS missing -- GHOST driver not loaded"; exit 1; }

lines() { grep -ac 'rate=' "$1" 2>/dev/null || echo 0; }
since() { tail -n +$(( ${2:-0} + 1 )) "$1" 2>/dev/null | grep -a -oE 'rate=[0-9.]+' | cut -d= -f2 \
          | awk '{s+=$1;n++} END{ if(n>=2) printf "%.1f", s/n; else printf "0" }'; }
sum_since() { local t=0 f m; for spec in "$@"; do f=${spec%%:*}; m=${spec##*:}
                t=$(awk -v a="$t" -v b="$(since "$f" "$m")" 'BEGIN{print a+b}'); done; echo "$t"; }

APIDS=(); ALOGS=()
cleanup(){ for p in "${APIDS[@]}" ${BPID:-}; do kill "$p" 2>/dev/null; done; }
trap cleanup EXIT

start_attacker_procs() {   # n
    for i in $(seq 1 "$1"); do
        local log=/tmp/packA$i.txt
        $PY "$BURN" > "$log" 2>/dev/null & APIDS+=($!) ; ALOGS+=("$log")
    done
}
weight_all() {             # re-apply weights to every attacker pid + victim
    echo poll | sudo tee $GS >/dev/null
    for p in "${APIDS[@]}"; do echo "ts $p $TS_A" | sudo tee $GS >/dev/null; done
    echo "ts $BPID $TS_B" | sudo tee $GS >/dev/null
}
measure_round() {          # label
    sleep "$SETTLE"
    local marks=() m
    for f in "${ALOGS[@]}"; do m=$(lines "$f"); marks+=("$f:$m"); done
    local mb; mb=$(lines /tmp/packB.txt)
    sleep "$WINDOW"
    local a b; a=$(sum_since "${marks[@]}"); b=$(since /tmp/packB.txt "$mb")
    printf '  %-28s A_total=%-8s (%d proc)  B=%-8s' "$1" "$a" "${#APIDS[@]}" "$b"
    awk -v a="$a" -v b="$b" 'BEGIN{ if(b>0) printf "  agg=%.1f  A:B=%.2f:1\n", a+b, a/b; else printf "\n" }'
    LAST_A=$a; LAST_B=$b
}

echo "=== victim B starts (weight $TS_B) ==="
$PY "$BURN" > /tmp/packB.txt 2>/dev/null & BPID=$!
sleep 20

echo "=== round 1: A honest, 1 process (weight $TS_A) ==="
start_attacker_procs 1
sleep 20
weight_all; sleep 3; weight_all
measure_round "honest 1 proc"
H_A=$LAST_A; H_B=$LAST_B

echo "=== round 2: A PACKS $PACK processes, same weight each ==="
start_attacker_procs $(( PACK - 1 ))
sleep 20
weight_all; sleep 3; weight_all
measure_round "packed $PACK proc"
P_A=$LAST_A; P_B=$LAST_B

echo
echo "=== VERDICT ==="
awk -v ha="$H_A" -v hb="$H_B" -v pa="$P_A" -v pb="$P_B" -v n="$PACK" 'BEGIN{
  printf "  A honest total = %.1f   -> A packed total = %.1f", ha, pa;
  if (ha>0) printf "   (x%.2f)", pa/ha; printf "\n";
  printf "  B victim       = %.1f   -> B under attack = %.1f", hb, pb;
  if (hb>0) printf "   (x%.2f)", pb/hb; printf "\n";
  if (ha<=0 || hb<=0 || pa<=0) { print "  INCONCLUSIVE -- a round produced no rate lines"; exit }
  gain = pa/ha; loss = pb/hb;
  if (gain > 1.25 && loss < 0.85)
      printf "  >>> PACKING WORKS: A gained %.0f%% and B lost %.0f%%. The runlist\n      enforcer divides per channel-group, not per tenant -- SAME CLASS OF HOLE AS V1.\n", (gain-1)*100, (1-loss)*100;
  else if (gain > 1.25)
      printf "  >>> A gained %.0f%% but B did not lose much -- looks like gap-filling\n      (work conservation), not theft. Check the aggregate.\n", (gain-1)*100;
  else
      printf "  >>> PACKING DEFEATED: A total flat (x%.2f) across %d processes.\n      The enforcer binds the TENANT, not the process.\n", gain, n;
}'
echo
echo "=== the new attack surface the GHOST driver itself adds ==="
echo "  /proc/driver/nvidia/gpusched is the scheduler control. It is root-only"
echo "  (NV_IS_SUSER) -- but a container running as root is still root in its"
echo "  userns unless the sandbox hides the file. If a TENANT can write it, one"
echo "  tenant can detach another. Checked from inside a sandbox by"
echo "  adversarial-2-tenant.sh; from the host it is trivially writable:"
ls -l $GS
echo "poll" | sudo tee $GS >/dev/null && echo "  host can drive it (expected)"
sudo cat $GS | head -8
echo "=== dmesg ==="
sudo dmesg | grep -a -iE 'GHOST sched' | tail -8
echo "=== DONE ==="
