# A third-party inference + agent stack on the slicing path

The values files and NetworkPolicies used to put two unmodified third-party
repositories onto this branch's slicing path on `vm-nv-dmd1` (RTX A6000, GA102),
2026-09-28. That machine has since been destroyed; this is what is needed to do
it again. `CLAUDE.md`'s "A real third-party stack on the slicing path" has the
measurements and the findings.

- **Phoenix Serving** — `github.com/midokura/phoenix-serving`, KServe
  `LLMInferenceService` + Gateway API + AgentGateway + a GAIE endpoint picker,
  serving Qwen3.5-4B.
- **MidoClaw** — `github.com/midokura/phoenix-claw`, an agent platform whose
  per-user OpenShell sandboxes run LLM-authored code.

Neither repository was modified.

## Files

| file | what it is |
| --- | --- |
| `values-tenant-gvisor.yaml` | Phoenix Serving's model chart: `vllm.runtimeClassName: gvisor` plus `nvidia.com/gpumem` and `nvidia.com/gpucores` on the container, and a smaller `epp.resources` |
| `values-claw-tenant.yaml` | MidoClaw: locally built images, `authBypass`, and `server.defaultRuntimeClassName: gvisor` so every agent sandbox is a gVisor sandbox |
| `allow-inference-platform.yaml` | lets the serving control plane reach into the tenant (ext_proc + vLLM) |
| `allow-traefik.yaml` | lets the ingress controller reach the tenant's UI and API |
| `allow-platform-to-sandboxes.yaml` | opens the agent gateway port to the platform namespace — see the caveat below |
| `build-images.sh` | builds MidoClaw's api/ui/lifecycle from the working tree and imports them into k3s, so no private registry is needed |
| `tenant-nv-a-quota-before.json` | the tenant's ResourceQuota before it was raised to fit a real model |
| `cilium-values-before.yaml` | the Cilium helm values before `socketLB.hostNamespaceOnly=true` |
| `evidence-cilium-*.txt` | `cilium-dbg monitor` output behind the socket-LB finding, including the runc/gVisor comparison |

## Order to bring it up

1. **Cilium must have `socketLB.hostNamespaceOnly=true`** before anything else,
   or no gVisor pod can reach any ClusterIP and the whole stack fails in ways
   that look like DNS. Set it, then **`kubectl rollout restart ds/cilium`** — a
   helm upgrade that only changes the ConfigMap rolls nothing and
   `rollout status` still prints success. Confirm with
   `cilium-dbg status --verbose | grep "Socket LB Coverage"`; it must say
   `Hostns-only`, not `Full`.
2. `kubernetes-sigs/agent-sandbox` CRDs + controller, installed **separately**.
   The agentic-platform chart's `agentSandbox.install: true` is inert — the key
   exists only in `values.yaml`, with no template behind it.
3. Phoenix Serving: `GATEWAY_API_ENABLED=false ./scripts/setup.sh` if another
   component already owns the Gateway API CRDs (k3s's traefik does).
4. The model chart into the tenant namespace with `values-tenant-gvisor.yaml`.
5. `build-images.sh`, then MidoClaw with `values-claw-tenant.yaml`.
6. The three NetworkPolicies.

Sizing note: the quota has to fit the model. Qwen3.5-4B at `gpumem: 20480` and
`gpucores: 60` left room on a 46 GiB A6000 for a second tenant.

## Caveats worth reading before reusing these

- **`allow-platform-to-sandboxes.yaml` does not actually fix anything**, and is
  kept only so the next person does not rediscover it. OpenShell runs a
  sandbox's workload in a *nested* network namespace and publishes no workload
  port to the pod IP, so MidoClaw's `gateway_readiness`, which dials a pod IP on
  18789, cannot work in `combined` topology — on gVisor *or* runc. Its
  podSelector (`openshell.ai/managed-by=openshell`) also matches nothing: the
  pods carry only `agents.x-k8s.io/sandbox-name-hash`, so the openshell chart's
  own `<ns>-sandbox-ssh` policy is equally inert. See CLAUDE.md, "Settled: an
  OpenShell sandbox exposes nothing at its pod IP".
- **The namespace label matters twice.** The quota webhook's namespace selector
  decides whether a pod gets its slice at all, and it also stamps
  `runtimeClassName: gvisor` on *every* pod in that namespace — including the
  trusted control plane. That was acceptable here; decide it deliberately.
- **`kubectl port-forward` will not reach any of these pods**, because they are
  gVisor sandboxes. Use a Service or the Ingress.
