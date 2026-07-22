#!/bin/sh
# Anti-regression gate: actually BUILD service images.
#
# `go build ./...` and `go test ./...` pass on any machine that has the
# agentnexus sibling checkout, so they cannot detect a container build that is
# broken by a dependency the build context does not contain. That is precisely
# how the Dockerfile stayed unbuildable for every service without a single
# failing test. This script closes that gap by producing a real image and
# confirming the binary landed in it.
#
# Usage:
#   deploy/docker/build-check.sh [service ...]
#
# With no arguments it builds every service in cmd/. Environment:
#   ATLAS_NEXUS_RUNTIME_SDK_DIR  path to agentnexus/sdk/go/runtime
#                                (default: the sibling checkout)
#   GOPROXY_BUILD                passed through as the GOPROXY build arg
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)   # <repo>/services/agentatlas/deploy/docker
module_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)    # <repo>/services/agentatlas
repo_root=$(CDPATH= cd -- "$module_dir/../.." && pwd)     # <repo>

nexus_sdk_dir=${ATLAS_NEXUS_RUNTIME_SDK_DIR:-"$repo_root/../agentnexus/sdk/go/runtime"}

if [ ! -f "$nexus_sdk_dir/go.mod" ]; then
  cat >&2 <<EOF
build-check: AgentNexus runtime SDK not found at
  $nexus_sdk_dir

services/agentatlas/go.mod replaces
github.com/astraclawteam/agentnexus/sdk/go/runtime with a relative path into a
SIBLING agentnexus checkout, so building any AgentAtlas image requires that
checkout to be present. Clone agentnexus next to agentatlas, or point
ATLAS_NEXUS_RUNTIME_SDK_DIR at sdk/go/runtime inside an existing checkout.

This prerequisite exists only because the SDK is not a published module — see
the DECISION NOTE in deploy/docker/Dockerfile.
EOF
  exit 1
fi
nexus_sdk_dir=$(CDPATH= cd -- "$nexus_sdk_dir" && pwd)

if [ "$#" -gt 0 ]; then
  services=$*
else
  services=$(cd "$module_dir/cmd" && ls -d -- */ | tr -d '/')
fi

status=0
for service in $services; do
  tag="agentatlas/$service:build-check"
  echo "build-check: building $service"
  if ! docker build \
      -f "$script_dir/Dockerfile" \
      --build-context "nexus-runtime-sdk=$nexus_sdk_dir" \
      --build-arg "SERVICE=$service" \
      ${GOPROXY_BUILD:+--build-arg "GOPROXY=$GOPROXY_BUILD"} \
      -t "$tag" \
      "$repo_root"; then
    echo "build-check: FAILED to build $service" >&2
    status=1
    continue
  fi
  # The image must actually carry the service binary; a Dockerfile that builds
  # but drops the artifact is still broken. The MSYS_* settings stop Git Bash on
  # Windows from rewriting the in-container paths into host paths.
  if ! MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*' \
      docker run --rm --entrypoint /bin/sh "$tag" -c 'test -x /usr/local/bin/app'; then
    echo "build-check: $service image has no executable /usr/local/bin/app" >&2
    status=1
  fi
done

if [ "$status" -eq 0 ]; then
  echo "build-check: OK"
fi
exit "$status"
