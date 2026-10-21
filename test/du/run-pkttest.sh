#!/bin/bash
# Runs pkttest in a gVisor sandbox inside netns "dutest", which holds fh0 (no
# IP) whose veth peer fh0p in the root netns is driven by peer.py.
set -u
MODE=${1:-sandbox}   # sandbox (netstack) or host (hostinet)
RUNSC=${RUNSC:-/tmp/runsc-du}
ip netns add dutest 2>/dev/null
ip link add fh0p type veth peer name fh0 netns dutest 2>/dev/null
ip link set fh0p address 02:00:00:00:00:02 up
ip -n dutest link set fh0 address 02:00:00:00:00:01 up
ip -n dutest link set lo up
ethtool -K fh0p rxvlan off >/dev/null 2>&1
B=/tmp/du-bundle; rm -rf $B; mkdir -p $B; cd $B
$RUNSC spec -- "${PKTTEST:-/tmp/pkttest}" fh0 100
python3 - <<'PY'
import json
c = json.load(open("config.json"))
c["root"] = {"path": "/", "readonly": True}
c["process"]["terminal"] = False
caps = ["CAP_NET_RAW", "CAP_NET_ADMIN", "CAP_NET_BIND_SERVICE", "CAP_KILL", "CAP_AUDIT_WRITE"]
for k in ("bounding", "effective", "permitted"):
    c["process"]["capabilities"][k] = caps
for ns in c["linux"]["namespaces"]:
    if ns["type"] == "network":
        ns["path"] = "/var/run/netns/dutest"
json.dump(c, open("config.json", "w"))
PY
timeout 30 python3 "$(dirname "$0")/peer.py" fh0p 100 &
PEER=$!
sleep 0.5
FLAGS="--root=/tmp/runsc-du-root --net-raw --allow-packet-socket-write"
if [ "$MODE" = host ]; then FLAGS="$FLAGS --network=host"; else FLAGS="$FLAGS --l2-links=fh0"; fi
timeout 40 $RUNSC $FLAGS run --bundle $B dutest-$MODE
echo "runsc exit: $?"
wait $PEER
$RUNSC --root=/tmp/runsc-du-root delete -force dutest-$MODE 2>/dev/null
[ "${KEEP:-}" ] || { ip link del fh0p 2>/dev/null; ip netns del dutest; }
