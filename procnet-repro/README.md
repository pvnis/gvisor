# /proc/net/tcp is not network-namespace scoped on gVisor

`./repro.sh gvisor` and `./repro.sh runc`. Measured on vm-nv-dmd1, 2026-09-28.

| | root ns `/proc/net/tcp` | `connect(127.0.0.1:12345)` from root ns |
| --- | --- | --- |
| gVisor | `00000000:3039` — the nested namespace's socket | REFUSED |
| runc | empty — correct | REFUSED |

So on gVisor the file reports a listener the stack in that namespace refuses to
connect to. Found while debugging why an OpenShell agent sandbox's gateway port
looked reachable and was not; see gvisor/UPSTREAM-NOTES.md.
