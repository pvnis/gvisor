#!/usr/bin/env bash
# ADVERSARIAL 2: attacks launched from inside a vCluster tenant, holding only a
# tenant API token -- the threat model in SECURITY-FINDINGS.md.
#
# Scoring follows test-isolation.sh's rule, which this project learned the hard
# way: a probe that FAILED TO RUN is INCONCLUSIVE, never a PASS. A deny test
# that passes because the pod crashed proves nothing, and this repo has already
# produced two false "blocked" results that way.
set -uo pipefail
export KUBECONFIG=/home/ubuntu/.kube/config
T=${TENANT:-tenant-nv-a}
VC="vcluster connect $T --namespace $T --"
pass=0; fail=0; inc=0
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass+1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail+1)); }
huh() { printf '  \033[33mINCONCLUSIVE\033[0m %s\n' "$1"; inc=$((inc+1)); }

# run a pod inside the tenant, return its logs; empty output => did not run
tenant_run() {  # name yaml
    $VC kubectl delete pod "$1" --ignore-not-found --wait=true >/dev/null 2>&1
    printf '%s' "$2" | $VC kubectl apply -f - >/dev/null 2>&1 || { echo ""; return; }
    local ph
    for i in $(seq 1 48); do
        ph=$($VC kubectl get pod "$1" --no-headers 2>/dev/null | awk '{print $3}')
        case "$ph" in Succeeded|Completed|Failed|Error) break;; esac
        sleep 5
    done
    $VC kubectl logs "$1" 2>/dev/null
}
host_pod_for() { kubectl get pods -n "$T" --no-headers 2>/dev/null | awk -v n="$1" '$1 ~ "^"n"-x-" {print $1}' | head -1; }

echo "############ A. memory quota ############"
Q=$(tenant_run quota-probe '
apiVersion: v1
kind: Pod
metadata:
  name: quota-probe
  annotations:
    dev.gvisor.flag.nvproxy-gpu-memory-limit: "42949672960"   # 40 GiB: escalation
spec:
  restartPolicy: Never
  schedulerName: hami-scheduler
  containers:
  - name: c
    image: nvidia/cuda:12.9.1-base-ubuntu22.04
    env: [{name: CUDA_DISABLE_CONTROL, value: "true"}]
    command: ["bash","-lc","nvidia-smi --query-gpu=memory.total --format=csv,noheader; uname -r; ls /proc/driver/nvidia/ 2>/dev/null | tr \x27\n\x27 \x27 \x27; echo; echo GPUSCHED=$(test -e /proc/driver/nvidia/gpusched && echo VISIBLE || echo hidden)"]
    resources: {limits: {nvidia.com/gpu: "1", nvidia.com/gpumem: "1024"}}
')
if [ -z "$Q" ]; then huh "quota probe produced no output (pod never ran)"; else
  echo "$Q" | sed 's/^/      /'
  echo "$Q" | grep -q '1024 MiB' && ok "quota is the ADMITTED 1024 MiB, not the 40 GiB self-annotation" \
                                 || bad "quota is NOT 1024 MiB -- escalation may have worked"
  echo "$Q" | grep -q '46068'    && bad "sandbox can see the whole 46068 MiB device" \
                                 || ok "whole-device size not visible"
  echo "$Q" | grep -q 'gvisor'   && ok "sandboxed (4.19.0-gvisor)" \
                                 || bad "NOT sandboxed -- host kernel visible"
  # the control surface the GHOST driver adds
  echo "$Q" | grep -q 'GPUSCHED=hidden' && ok "/proc/driver/nvidia/gpusched NOT visible in sandbox" \
                                        || bad "gpusched VISIBLE in sandbox -- a tenant may be able to detach peers"
  HP=$(host_pod_for quota-probe)
  if [ -n "$HP" ]; then
      A=$(kubectl get pod -n "$T" "$HP" -o jsonpath='{.metadata.annotations.dev\.gvisor\.flag\.nvproxy-gpu-memory-limit}' 2>/dev/null)
      echo "      host annotation = ${A:-<none>}"
      [ "$A" = "1073741824" ] && ok "webhook rewrote the annotation to 1 GiB on the host" \
                              || bad "host annotation is ${A:-<none>}, expected 1073741824"
  else huh "host pod not found; could not check the rewritten annotation"; fi
fi

echo "############ B. sandbox escape via runtimeClassName ############"
E=$(tenant_run escape-probe '
apiVersion: v1
kind: Pod
metadata: {name: escape-probe}
spec:
  restartPolicy: Never
  runtimeClassName: nvidia        # ask for the runc+nvidia handler = host kernel
  schedulerName: hami-scheduler
  containers:
  - name: c
    image: nvidia/cuda:12.9.1-base-ubuntu22.04
    command: ["bash","-lc","uname -r; nvidia-smi --query-gpu=memory.total --format=csv,noheader"]
    resources: {limits: {nvidia.com/gpu: "1", nvidia.com/gpumem: "512"}}
')
if [ -z "$E" ]; then huh "escape probe produced no output (may have been rejected -- check manually)"; else
  echo "$E" | sed 's/^/      /'
  echo "$E" | grep -q 'gvisor' && ok "syncer forced gvisor despite the pod asking for runtimeClassName: nvidia" \
                               || bad "ESCAPE: pod got a non-gVisor runtime; host kernel $(echo "$E" | head -1)"
  HP=$(host_pod_for escape-probe)
  [ -n "$HP" ] && echo "      host runtimeClassName = $(kubectl get pod -n "$T" "$HP" -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null)"
fi

echo "############ C. peer visibility ############"
echo "  (start a long-lived GPU pod in $T, then check a SECOND tenant cannot see it)"
V=$(tenant_run peek-probe '
apiVersion: v1
kind: Pod
metadata: {name: peek-probe}
spec:
  restartPolicy: Never
  schedulerName: hami-scheduler
  containers:
  - name: c
    image: nvidia/cuda:12.9.1-base-ubuntu22.04
    command: ["bash","-lc","nvidia-smi; echo ---; nvidia-smi --query-compute-apps=pid,used_memory --format=csv"]
    resources: {limits: {nvidia.com/gpu: "1", nvidia.com/gpumem: "512"}}
')
if [ -z "$V" ]; then huh "peer-visibility probe produced no output"; else
  echo "$V" | tail -6 | sed 's/^/      /'
  # any pid listed that is not this pod's own is a leak
  if echo "$V" | grep -qE '^[0-9]+,'; then
      huh "compute-apps listed pids -- inspect above; only THIS pod's own work should appear"
  else
      ok "no peer processes visible in nvidia-smi"
  fi
fi

echo
printf 'passed %d, failed %d, inconclusive %d\n' "$pass" "$fail" "$inc"
echo "NOTE: inconclusive is not a pass. Re-run those before drawing any conclusion."
[ "$fail" -eq 0 ]
