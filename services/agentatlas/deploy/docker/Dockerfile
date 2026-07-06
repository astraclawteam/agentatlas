# Shared build for the four Go services; SERVICE selects the entrypoint.
ARG GO_IMAGE=golang:1.26-alpine
FROM ${GO_IMAGE} AS build
ARG SERVICE
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/app ./cmd/${SERVICE}

FROM alpine:3.21
RUN adduser -D -u 10001 atlas
USER atlas
COPY --from=build /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
