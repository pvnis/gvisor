#!/usr/bin/env bash
# Bring the k3s + vCluster + HAMi stack back up ON THE GHOST DRIVER.
#
# Deliberately NOT a6000-7-restore.sh. That reverts to the stock DKMS module,
# which would discard the runlist enforcer -- and the runlist is the only
# mechanism measured to bind a doorbell workload. The GHOST build is open
# 610.43.02 plus hooks, same driver ABI, so nvproxy's memory quota works on it
# unchanged. Running the cluster on it is what lets memory quota and compute
# division be adversarially tested together.
#
# Prereq: a6000-4-corrected-swap.sh has been run (GHOST driver resident).
set -uo pipefail
export KUBECONFIG=/home/ubuntu/.kube/config
fail=0

echo "=== [1/5] which driver is resident? ==="
if [ ! -e /proc/driver/nvidia/gpusched ]; then
    echo "!! GHOST driver NOT loaded (no gpusched). Run a6000-4-corrected-swap.sh first."
    echo "   (a6000-verify-loaded.sh will tell you exactly what is resident.)"
    exit 1
fi
echo "  gpusched present -> GHOST driver"
cat /sys/module/nvidia/srcversion 2>/dev/null
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader

echo "=== [2/5] un-pause the HAMi device plugin ==="
# A DaemonSet cannot be scaled; a6000-4 paused it with an unsatisfiable
# nodeSelector, so it is un-paused by removing that key.
kubectl -n kube-system patch ds hami-device-plugin --type json \
    -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector/ghost~1probe-paused"}]' 2>&1 | tail -1 || \
kubectl -n kube-system patch ds hami-device-plugin --type merge \
    -p '{"spec":{"template":{"spec":{"nodeSelector":null}}}}' 2>&1 | tail -1 || true
kubectl -n kube-system rollout status ds/hami-device-plugin --timeout=180s 2>&1 | tail -2

echo "=== [3/5] node advertising GPU again? ==="
for i in $(seq 1 36); do
    kubectl get node vm-nv-dmd1 -o jsonpath='{.status.allocatable}' 2>/dev/null | grep -q 'nvidia.com/gpu' && break
    sleep 5
done
ALLOC=$(kubectl get node vm-nv-dmd1 -o jsonpath='{.status.allocatable.nvidia\.com/gpu}' 2>/dev/null)
echo "  nvidia.com/gpu = ${ALLOC:-<none>}"
[ -z "$ALLOC" ] && { echo "!! device plugin did not re-register"; fail=1; }

echo "=== [4/5] cluster health ==="
kubectl get nodes 2>&1 | tail -2
kubectl get pods -A --no-headers 2>/dev/null | awk '{print $4}' | sort | uniq -c
echo "  tenants:"
kubectl get pods -A --no-headers 2>/dev/null | grep -E 'tenant-(nv-a|nv-b|c)-0' | awk '{printf "    %-14s %s %s\n", $1, $3, $4}'

echo "=== [5/5] end-to-end smoke: a quota'd GPU pod under gVisor ==="
kubectl create ns gpu-test --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1
kubectl -n gpu-test delete pod resume-smoke --ignore-not-found --wait=true >/dev/null 2>&1
cat <<'EOF' | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: resume-smoke, namespace: gpu-test}
spec:
  restartPolicy: Never
  runtimeClassName: gvisor
  schedulerName: hami-scheduler
  containers:
  - name: cuda
    image: nvidia/cuda:12.9.1-base-ubuntu22.04
    command: ["bash","-lc","nvidia-smi --query-gpu=memory.total --format=csv,noheader; uname -r"]
    resources: {limits: {nvidia.com/gpu: "1", nvidia.com/gpumem: "2048"}}
EOF
for i in $(seq 1 40); do
    P=$(kubectl get pod -n gpu-test resume-smoke --no-headers 2>/dev/null | awk '{print $3}')
    case "$P" in Succeeded|Completed|Failed|Error) break;; esac; sleep 5
done
OUT=$(kubectl logs -n gpu-test resume-smoke 2>/dev/null)
echo "  phase=$P"
echo "$OUT" | sed 's/^/    /'
echo "$OUT" | grep -q '2048 MiB'      || { echo "  !! quota NOT applied (expected 2048 MiB)"; fail=1; }
echo "$OUT" | grep -q 'gvisor'        || { echo "  !! NOT sandboxed (expected 4.19.0-gvisor)"; fail=1; }

echo
if [ "$fail" = "0" ]; then
    echo "=== STACK UP on the GHOST driver: quota enforced AND runlist available ==="
else
    echo "=== STACK NOT HEALTHY -- see the !! lines above ==="
fi
exit $fail
