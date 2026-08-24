FROM nvidia/cuda:12.9.1-devel-ubuntu22.04 AS build
COPY burn.cu /burn.cu
RUN nvcc -O2 -arch=sm_86 -o /burn /burn.cu -lcublas

FROM nvidia/cuda:12.9.1-runtime-ubuntu22.04
COPY --from=build /burn /burn
# stand the in-container HAMi limiter down: enforcement must be the Sentry's
ENV CUDA_DISABLE_CONTROL=true
# BURN_PROCS>1 is the packing attack: N processes inside ONE sandbox
CMD ["sh","-c","for i in $(seq 1 ${BURN_PROCS:-1}); do /burn & done; wait"]
