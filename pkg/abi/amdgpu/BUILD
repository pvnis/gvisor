load("//tools:defs.bzl", "go_library")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "amdgpu",
    srcs = [
        "kfd.go",
    ],
    hostlayout = True,
    marshal = True,
    visibility = ["//pkg/sentry:internal"],
    deps = ["//pkg/abi/linux"],
)
