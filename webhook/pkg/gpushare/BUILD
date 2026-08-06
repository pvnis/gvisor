load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "gpushare",
    srcs = [
        "gpushare.go",
        "hami.go",
    ],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/log",
        "@io_k8s_api//core/v1:go_default_library",
    ],
)

go_test(
    name = "gpushare_test",
    size = "small",
    srcs = ["gpushare_test.go"],
    library = ":gpushare",
    deps = [
        "@io_k8s_api//core/v1:go_default_library",
        "@io_k8s_apimachinery//pkg/api/resource:go_default_library",
    ],
)
