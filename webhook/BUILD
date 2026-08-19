load("//images:defs.bzl", "docker_image")
load("//tools:defs.bzl", "go_binary", "pkg_tar")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

docker_image(
    name = "image",
    data = ":files",
    statements = ['ENTRYPOINT ["/webhook"]'],
    tags = [
        "local",
        "manual",
        "no-sandbox",
    ],
)

# files is the full file system of the webhook container. It is simply:
# /
# └─ webhook
pkg_tar(
    name = "files",
    srcs = [":webhook"],
    extension = "tgz",
    # go_binary puts its output in a "<name>_" directory, so the binary is at
    # webhook/webhook_/webhook and that whole prefix has to come off for the
    # image's ENTRYPOINT of /webhook to resolve.
    strip_prefix = "/webhook/webhook_",
)

go_binary(
    name = "webhook",
    srcs = ["main.go"],
    features = ["fully_static_link"],
    pure = "on",
    deps = ["//webhook/pkg/cli"],
)
