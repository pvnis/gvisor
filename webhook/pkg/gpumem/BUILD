load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "gpumem",
    srcs = ["gpumem.go"],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/log",
        "@io_k8s_api//core/v1:go_default_library",
    ],
)

go_test(
    name = "gpumem_test",
    size = "small",
    srcs = ["gpumem_test.go"],
    library = ":gpumem",
    deps = [
        "@io_k8s_api//core/v1:go_default_library",
        "@io_k8s_apimachinery//pkg/api/resource:go_default_library",
    ],
)
