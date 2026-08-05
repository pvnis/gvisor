load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "amdconf",
    srcs = ["cumask.go"],
    visibility = ["//visibility:public"],
)

go_test(
    name = "amdconf_test",
    size = "small",
    srcs = ["cumask_test.go"],
    library = ":amdconf",
)
