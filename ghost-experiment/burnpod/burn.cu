// cuBLAS SGEMM burn: a doorbell-submission workload, the class that defeats the
// compute gate. Prints "rate=<n> matmul/s" per tenant process.
#include <cublas_v2.h>
#include <cuda_runtime.h>
#include <cstdio>
#include <cstdlib>
#include <chrono>
int main() {
    int n = 4096;
    if (const char* e = getenv("BURN_N")) n = atoi(e);
    size_t sz = (size_t)n * n * sizeof(float);
    float *a, *b, *c;
    if (cudaMalloc(&a, sz) || cudaMalloc(&b, sz) || cudaMalloc(&c, sz)) {
        fprintf(stderr, "alloc failed\n"); return 1;
    }
    cudaMemset(a, 0, sz); cudaMemset(b, 0, sz);
    cublasHandle_t h;
    if (cublasCreate(&h) != CUBLAS_STATUS_SUCCESS) { fprintf(stderr, "cublas init failed\n"); return 1; }
    float alpha = 1.f, beta = 0.f;
    for (int i = 0; i < 20; i++)
        cublasSgemm(h, CUBLAS_OP_N, CUBLAS_OP_N, n, n, n, &alpha, a, n, b, n, &beta, c, n);
    cudaDeviceSynchronize();
    int cnt = 0;
    auto t0 = std::chrono::steady_clock::now();
    for (;;) {
        cublasSgemm(h, CUBLAS_OP_N, CUBLAS_OP_N, n, n, n, &alpha, a, n, b, n, &beta, c, n);
        if (++cnt % 200 == 0) {
            cudaDeviceSynchronize();
            auto t1 = std::chrono::steady_clock::now();
            double dt = std::chrono::duration<double>(t1 - t0).count();
            printf("rate=%.1f matmul/s\n", 200.0 / dt); fflush(stdout);
            cnt = 0; t0 = t1;
        }
    }
}
