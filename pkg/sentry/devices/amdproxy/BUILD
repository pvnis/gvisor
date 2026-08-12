load("//tools:defs.bzl", "go_library")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "amdproxy",
    srcs = [
        "amdproxy.go",
        "drm.go",
        "drm_mmap.go",
        "drm_unsafe.go",
        "kfd.go",
        "kfd_handlers.go",
        "kfd_mmap.go",
        "kfd_unsafe.go",
        "memquota.go",
        "seccomp_filters.go",
        "sharedvm.go",
    ],
    visibility = [
        "//pkg/sentry:internal",
    ],
    deps = [
        "//pkg/abi/amdgpu",
        "//pkg/abi/linux",
        "//pkg/context",
        "//pkg/devutil",
        "//pkg/errors/linuxerr",
        "//pkg/fdnotifier",
        "//pkg/hostarch",
        "//pkg/log",
        "//pkg/marshal",
        "//pkg/marshal/primitive",
        "//pkg/seccomp",
        "//pkg/sentry/devices/amdproxy/amdconf",
        "//pkg/sentry/arch",
        "//pkg/sentry/fsutil",
        "//pkg/sentry/kernel",
        "//pkg/sentry/kernel/auth",
        "//pkg/sentry/memmap",
        "//pkg/sentry/vfs",
        "//pkg/sync",
        "//pkg/usermem",
        "//pkg/waiter",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
