#!/bin/bash
# gVisor: /proc/net/tcp is not network-namespace scoped.
#
# Binds a listener inside a nested network namespace, then reads /proc/net/tcp
# and attempts a connect from the ROOT namespace of the same sandbox. On gVisor
# the file lists the nested namespace's socket while the connect is refused --
# the file and the stack disagree. Under runc the file is correctly empty.
#
# Usage:  ./repro.sh gvisor|runc   (needs kubectl and a "gvisor" RuntimeClass)
set -u
RT="${1:-gvisor}"
POD="procnet-repro-$RT"
OVR=""
[ "$RT" = "gvisor" ] && OVR='--overrides={"spec":{"runtimeClassName":"gvisor"}}'

kubectl delete pod "$POD" --ignore-not-found --wait=true >/dev/null 2>&1
# shellcheck disable=SC2086
kubectl run "$POD" --image=alpine:3.21 --restart=Never $OVR --command -- sh -c '
apk add --no-cache util-linux python3 >/dev/null 2>&1
echo "root netns: $(readlink /proc/self/ns/net)"
unshare -Urn sh -c "
  ip link set lo up 2>/dev/null
  python3 -c \"
import socket,time
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind((chr(48)+chr(46)+chr(48)+chr(46)+chr(48)+chr(46)+chr(48),12345)); s.listen(5)
print(\\\"    nested: bound 0.0.0.0:12345\\\",flush=True)
time.sleep(14)
\" &
  sleep 3
  echo \"  nested netns: \$(readlink /proc/self/ns/net)\"
  echo \"  nested 0A:\"; grep \" 0A \" /proc/net/tcp | awk \"{print \\\"    \\\" \\\$2}\"
  sleep 11
" &
sleep 7
echo "=== from the ROOT netns ==="
echo "  root 0A:"; grep " 0A " /proc/net/tcp | awk "{print \"    \" \$2}"
echo -n "  connect 127.0.0.1:12345 -> "; (nc -z -w 3 127.0.0.1 12345 && echo OPEN) || echo REFUSED
wait' >/dev/null 2>&1

for _ in $(seq 1 24); do
  sleep 5
  p=$(kubectl get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  case "$p" in Succeeded|Failed) break;; esac
done
echo "runtime=$RT phase=$p   (0x3039 = port 12345)"
kubectl logs "$POD" 2>&1 | head -14
kubectl delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1
