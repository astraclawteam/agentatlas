# Shared build for the four Go services; SERVICE selects the entrypoint.
# Build context is the REPOSITORY ROOT (agentatlas/): the service module
# `replace`s ../../sdk/go, so both modules must be inside the context.
ARG GO_IMAGE=golang:1.26-alpine
FROM ${GO_IMAGE} AS build
ARG SERVICE
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY sdk/go/ sdk/go/
COPY services/agentatlas/go.mod services/agentatlas/go.sum services/agentatlas/
WORKDIR /src/services/agentatlas
RUN go mod download
WORKDIR /src
COPY services/agentatlas/ services/agentatlas/
WORKDIR /src/services/agentatlas
RUN go build -trimpath -o /out/app ./cmd/${SERVICE}

FROM alpine:3.21
RUN adduser -D -u 10001 atlas
USER atlas
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
