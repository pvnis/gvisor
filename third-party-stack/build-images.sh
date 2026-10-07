#!/bin/bash
# Build phoenix-claw's api, ui and lifecycle images from the working tree and
# import them into k3s. Avoids ghcr.io/midokura entirely -- all three have
# public base images -- so no midokura registry credentials are needed.
set -u
cd /home/ubuntu/phoenix-claw || exit 1
TAG=dev
rc=0
build() { # name dockerfile context image
  echo "=== building $1"
  docker build -t "$4:$TAG" -f "$2" "$3" || { echo "FAILED: $1"; rc=1; return; }
  docker save "$4:$TAG" -o "/tmp/pc-$1.tar" || { echo "SAVE FAILED: $1"; rc=1; return; }
  sudo k3s ctr images import "/tmp/pc-$1.tar" || { echo "IMPORT FAILED: $1"; rc=1; return; }
  rm -f "/tmp/pc-$1.tar"
  echo "=== ok $1"
}
build api       agentic-api/Dockerfile                       agentic-api ghcr.io/midokura/agentic-api
build ui        agentic-ui/Dockerfile                        agentic-ui  ghcr.io/midokura/agentic-ui
build lifecycle charts/agentic-platform/Dockerfile.lifecycle charts/agentic-platform ghcr.io/midokura/agentic-lifecycle
echo "BUILD_RC=$rc"
