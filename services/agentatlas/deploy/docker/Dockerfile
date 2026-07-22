# Shared build for the five Go services; SERVICE selects the entrypoint.
#
# THIS IMAGE NEEDS TWO BUILD CONTEXTS. Both are mandatory.
#
#   1. The DEFAULT context must be the agentatlas REPOSITORY ROOT, because
#      services/agentatlas/go.mod `replace`s ../../sdk/go. That replace is
#      intra-repo, so one context holds both modules.
#
#   2. A NAMED ADDITIONAL context `nexus-runtime-sdk` must point at the
#      agentnexus checkout's sdk/go/runtime directory, because
#      services/agentatlas/go.mod also carries
#
#          replace github.com/astraclawteam/agentnexus/sdk/go/runtime
#               => ../../../agentnexus/sdk/go/runtime
#
#      which resolves OUTSIDE this repository, into a SIBLING checkout. No
#      single build context can ever contain it. Omit the named context and
#      `go mod download` fails for EVERY service with
#      "reading /agentnexus/sdk/go/runtime/go.mod: no such file or
#      directory". That was this file's actual state until the named context
#      was added: `go build ./...` and `go test ./...` passed on developer
#      machines (the sibling directory is really there) while no service
#      image could be built at all. Forgetting the named context now fails
#      immediately on the COPY below, with BuildKit reporting "pull access
#      denied" for docker.io/library/nexus-runtime-sdk — that is an
#      undeclared build context, not a missing image.
#
# A named context is used rather than hoisting the default context up to the
# workspace root: hoisting would drag every unrelated sibling repository and
# its node_modules into the build, and would need a .dockerignore at the
# workspace root — a file belonging to no repository that silently affects
# every other build rooted there.
#
# Direct build:
#
#   docker build -f services/agentatlas/deploy/docker/Dockerfile \
#     --build-context nexus-runtime-sdk=../agentnexus/sdk/go/runtime \
#     --build-arg SERVICE=atlas-api .
#
# deploy/compose/compose.yaml wires both contexts. The anti-regression gate
# that actually builds an image is `make -C services/agentatlas
# docker-build-check` (deploy/docker/build-check.sh), run by the
# `container-build` job in .github/workflows/release.yml.
#
# DECISION NOTE — the real fix, recorded and deliberately NOT implemented
# here. The named context makes the container build work; it does not make
# the dependency EXPRESSIBLE. A cross-repository relative filesystem path is
# not something AgentAtlas can carry in a deployment artifact: Helm has no
# equivalent of a named build context at all, so deploy/helm can only ever
# consume images someone else already built with this workaround. The honest
# fix is to publish github.com/astraclawteam/agentnexus/sdk/go/runtime as a
# real versioned module and delete the replace at
# services/agentatlas/go.mod:115; the `nexus-runtime-sdk` context here and in
# compose.yaml then gets deleted with it, since it exists only to compensate
# for the module being unpublished. That requires a change in the agentnexus
# repository, which is out of scope for this change.
ARG GO_IMAGE=golang:1.26-alpine
FROM ${GO_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0

# The cross-repository replace target. It must land at this ABSOLUTE path:
# the module lives at /src/services/agentatlas, and ../../../agentnexus from
# there is /agentnexus.
COPY --from=nexus-runtime-sdk . /agentnexus/sdk/go/runtime/

WORKDIR /src
COPY sdk/go/ sdk/go/
COPY services/agentatlas/go.mod services/agentatlas/go.sum services/agentatlas/
WORKDIR /src/services/agentatlas
RUN go mod download
WORKDIR /src
COPY services/agentatlas/ services/agentatlas/
WORKDIR /src/services/agentatlas
# SERVICE is declared here, not at the top of the stage: an ARG invalidates the
# cache for every instruction that follows it, so declaring it earlier made all
# five services re-run `go mod download` from scratch instead of sharing one
# cached module layer.
ARG SERVICE
RUN go build -trimpath -o /out/app ./cmd/${SERVICE}

FROM alpine:3.21
RUN adduser -D -u 10001 atlas
USER atlas
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
