#!/bin/bash
# Runs DPDK testpmd (af_packet PMD on fh0, txonly) in a gVisor sandbox inside
# netns "dutest", whose veth peer fh0p is in the root netns, and reports how
# many frames reached fh0p. MODE: hugetlbfs | in-memory | no-huge.
# FWD is testpmd's forwarding mode (txonly, or macswap to echo the SEND frames
# that the host injects on fh0p back to it).
set -u
MODE=${1:-hugetlbfs}
FWD=${FWD:-txonly}
SEND=${SEND:-0}
BATCH=${BATCH:-0}  # sleep 1 ms after every BATCH injected frames (0: no pacing)
RUNSC=${RUNSC:-/tmp/runsc-du}
TESTPMD=${TESTPMD:-$HOME/dpdk-src/dpdk-stable-24.11.3/build/app/dpdk-testpmd}
ip netns add dutest 2>/dev/null
ip link add fh0p type veth peer name fh0 netns dutest 2>/dev/null
ip link set fh0p address 02:00:00:00:00:02 up
ip -n dutest link set fh0 address 02:00:00:00:00:01 up
ip -n dutest link set lo up
EAL="-l 0-1 -m 256 --file-prefix=du --vdev=net_af_packet0,iface=fh0"
case $MODE in
  in-memory) EAL="$EAL --in-memory" ;;
  no-huge) EAL="$EAL --no-huge" ;;
esac
B=/tmp/du-bundle-pmd; rm -rf $B; mkdir -p $B; cd $B
$RUNSC spec -- /bin/sh -c "grep -i huge /proc/meminfo; (sleep 4; grep HugePages_Free /proc/meminfo | sed s/^/during-run-/) & timeout -s INT 6 $TESTPMD $EAL -- --forward-mode=$FWD --auto-start --stats-period=2 --total-num-mbufs=4096 </dev/null; echo testpmd-exit=\$?; grep -i hugepages_free /proc/meminfo"
python3 - "$MODE" <<'PY'
import json, sys
c = json.load(open("config.json"))
c["root"] = {"path": "/", "readonly": True}
c["process"]["terminal"] = False
caps = ["CAP_NET_RAW", "CAP_NET_ADMIN", "CAP_IPC_LOCK", "CAP_SYS_ADMIN", "CAP_KILL", "CAP_AUDIT_WRITE", "CAP_NET_BIND_SERVICE"]
for k in ("bounding", "effective", "permitted"):
    c["process"]["capabilities"][k] = caps
for ns in c["linux"]["namespaces"]:
    if ns["type"] == "network":
        ns["path"] = "/var/run/netns/dutest"
c["mounts"].append({"destination": "/var/run/dpdk", "type": "tmpfs", "source": "tmpfs"})
if sys.argv[1] == "hugetlbfs":
    c["mounts"].append({"destination": "/dev/hugepages", "type": "hugetlbfs", "source": "nodev", "options": ["pagesize=2M"]})
json.dump(c, open("config.json", "w"))
PY
before=$(cat /sys/class/net/fh0p/statistics/rx_packets)
if [ "$SEND" -gt 0 ]; then
  (sleep 3; python3 -c "
import socket, sys
s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
s.bind(('fh0p', 0))
f = bytes.fromhex('020000000001020000000002aefe1000') + bytes(48)
import time
for i in range($SEND):
    s.send(f)
    if $BATCH and i % $BATCH == $BATCH - 1: time.sleep(0.001)
") &
fi
timeout 60 $RUNSC --root=/tmp/runsc-du-root --net-raw --allow-packet-socket-write --l2-links=fh0 --hugetlb-pages=256 run --bundle $B dutest-pmd 2>&1 | grep -vE "^\s*$" > /tmp/testpmd.out 2>&1; tr -d "\033" < /tmp/testpmd.out | grep -aE "EAL: (Selected|Detected CPU)|mbuf pool|[RT]X-packets|testpmd-exit|HugePages_Free|during-run|Error|error|failed" | sort -u
after=$(cat /sys/class/net/fh0p/statistics/rx_packets)
echo "fh0p received $((after - before)) frames from the sandbox"
$RUNSC --root=/tmp/runsc-du-root delete -force dutest-pmd 2>/dev/null
[ "${KEEP:-}" ] || { ip link del fh0p 2>/dev/null; ip netns del dutest; }
