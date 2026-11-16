load("//tools:defs.bzl", "go_library", "go_test")

package(default_applicable_licenses = ["//:license"])

licenses(["notice"])

go_library(
    name = "amdsysfs",
    srcs = ["snapshot.go"],
    visibility = ["//visibility:public"],
)

go_test(
    name = "amdsysfs_test",
    size = "small",
    srcs = ["snapshot_test.go"],
    library = ":amdsysfs",
)
