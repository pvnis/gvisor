load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "gpusched",
    srcs = [
        "gpusched.go",
        "server.go",
        "wire.go",
    ],
    visibility = ["//:sandbox"],
)

go_test(
    name = "gpusched_test",
    size = "small",
    srcs = [
        "gpusched_test.go",
        "server_test.go",
    ],
    library = ":gpusched",
)
