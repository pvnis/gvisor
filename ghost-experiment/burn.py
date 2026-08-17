#!/usr/bin/env python3
# Saturating fp16 matmul burn — a doorbell/graph-replay workload that defeats
# every temporal enforcement lever. Prints matmul/s. Creates a CUDA context
# (=> a ctxshare), which fires the GHOST driver hooks; read `sudo dmesg | grep
# GHOST` for the control statuses. Run natively (needs torch+CUDA) or in any
# CUDA container. No gVisor/k8s required for the driver-level probe.
#   python3 burn.py            # full GPU
# Compare the printed rate to a solo baseline to see if 0c/0d confined it.
import torch, time
n = 4096
a = torch.randn(n, n, device="cuda", dtype=torch.float16)
b = torch.randn(n, n, device="cuda", dtype=torch.float16)
for _ in range(50):  # warmup
    c = a @ b
torch.cuda.synchronize()
cnt = 0; t0 = time.time()
while True:
    c = a @ b
    cnt += 1
    if cnt % 500 == 0:
        torch.cuda.synchronize()
        dt = time.time() - t0
        print("rate=%.1f matmul/s" % (500 / dt), flush=True)
        cnt = 0; t0 = time.time()
