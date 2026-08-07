load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "gpusched",
    srcs = [
        "device.go",
        "gpusched.go",
        "server.go",
        "smi.go",
        "usage.go",
        "wire.go",
    ],
    visibility = ["//:sandbox"],
    deps = ["//pkg/log"],
)

go_test(
    name = "gpusched_test",
    size = "small",
    srcs = [
        "device_test.go",
        "gpusched_test.go",
        "server_test.go",
        "smi_test.go",
    ],
    library = ":gpusched",
)
