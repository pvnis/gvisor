#!/bin/bash
# Runs a command in a gVisor sandbox in netns "dutestlo" (only lo, up), e.g.
# a syscall test binary under --network=host. Usage: run-in-netns.sh FLAGS -- CMD...
set -u
RUNSC=${RUNSC:-/tmp/runsc-du}
FLAGS=(); while [ "$1" != -- ]; do FLAGS+=("$1"); shift; done; shift
ip netns add dutestlo 2>/dev/null; ip -n dutestlo link set lo up
B=/tmp/du-bundle-lo; rm -rf $B; mkdir -p $B; cd $B
$RUNSC spec -- "$@"
python3 - <<'PY'
import json
c = json.load(open("config.json"))
c["root"] = {"path": "/", "readonly": True}
c["process"]["terminal"] = False
caps = ["CAP_NET_RAW", "CAP_NET_ADMIN", "CAP_IPC_LOCK", "CAP_SYS_ADMIN", "CAP_KILL", "CAP_AUDIT_WRITE", "CAP_NET_BIND_SERVICE", "CAP_DAC_OVERRIDE", "CAP_SYS_PTRACE"]
for k in ("bounding", "effective", "permitted"):
    c["process"]["capabilities"][k] = caps
for ns in c["linux"]["namespaces"]:
    if ns["type"] == "network":
        ns["path"] = "/var/run/netns/dutestlo"
c["mounts"].append({"destination": "/tmp", "type": "tmpfs", "source": "tmpfs"})
json.dump(c, open("config.json", "w"))
PY
timeout ${TMO:-600} $RUNSC --root=/tmp/runsc-du-root "${FLAGS[@]}" run --bundle $B dutestlo
rc=$?
$RUNSC --root=/tmp/runsc-du-root delete -force dutestlo 2>/dev/null
ip netns del dutestlo
exit $rc
