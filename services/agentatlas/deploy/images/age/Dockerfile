# syntax=docker/dockerfile:1
#
# Reproducible Apache AGE Outcome Graph image (GA Task 1A).
#
# Builds a PostgreSQL 17 image with the Apache AGE 1.6.0 graph extension
# compiled in, from the upstream-verified Apache AGE 1.6.0 source archive
# for the PG17 release branch (tag PG17/v1.6.0-rc0, commit
# 54905a09bf8462f22a87c3adfd2ab5752e5c1e71). This image is the Outcome
# Graph's independent database service — it is a separately deployed
# PostgreSQL instance and is never the authoritative WorkCase PostgreSQL
# store (see docs/superpowers/plans/2026-07-13-agentatlas-commercial-ga-tdd.md
# Task 1A and agentatlas-enterprise contracts/ga-manifest.yaml
# outcome_graph section).
#
# Every ARG default below is pinned to the exact value recorded in
# build.lock.yaml; do not change one without updating the other.
#
# Reproducibility caveat: the base image and the AGE source archive are
# pinned by digest (content-addressed), but the apt-get toolchain installs
# below are NOT version-pinned (matching Apache AGE's own reference
# Dockerfile), so bit-for-bit reproducibility of the compiled age.so layer
# is not yet guaranteed. See build.lock.yaml `reproducibility_notes` for
# exactly what is/isn't pinned and what Task 22 would need (apt snapshot pin
# + recorded package versions + SOURCE_DATE_EPOCH).
#
# Build (from services/agentatlas/deploy/images/age):
#   docker buildx build --sbom=true --provenance=true \
#     --platform linux/amd64 \
#     -t agentatlas/age-postgres17:1.6.0 \
#     -f Dockerfile .
#
# The base image is pinned by digest (independent of the authoritative
# WorkCase PostgreSQL image pin in contracts/ga-manifest.yaml, even though
# both currently resolve to the same upstream postgres:17 index — see
# notes.md "no dual role" discussion: this Dockerfile's OUTPUT is a
# distinct, AGE-bearing image with its own digest, which is what actually
# runs as the graph service).
ARG BASE_IMAGE=postgres:17@sha256:0af65001d05296a2ead57ac4a6412433d8913d1bb5d0c88435a7d1e1ee5cb04b
ARG AGE_VERSION=1.6.0
ARG AGE_SOURCE_URL=https://github.com/apache/age/releases/download/PG17/v1.6.0-rc0/apache-age-1.6.0-src.tar.gz
ARG AGE_SOURCE_SHA256=57c7187a37942f1b6208e1f68caafcb7374f8bedaabad162c8760adb44ae9ffe

# ---- build stage: compile the AGE extension against the pinned PG17 dev headers ----
FROM ${BASE_IMAGE} AS build
ARG AGE_VERSION
ARG AGE_SOURCE_URL
ARG AGE_SOURCE_SHA256

RUN apt-get update \
    && apt-get install -y --no-install-recommends --no-install-suggests \
       ca-certificates \
       curl \
       bison \
       build-essential \
       flex \
       postgresql-server-dev-17 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /age-src

# Fetch the pinned, upstream-verified AGE source archive and refuse to
# proceed if its content digest does not match exactly — this is the sole
# network fetch in the build and is checksum-gated, never trust-on-first-use.
RUN curl -fsSL -o age.tar.gz "${AGE_SOURCE_URL}" \
    && echo "${AGE_SOURCE_SHA256}  age.tar.gz" | sha256sum -c - \
    && tar -xzf age.tar.gz --strip-components=1 \
    && rm age.tar.gz

# PGXS build: produces age.so plus the generated age--1.6.0.sql install
# script and age.control, installed under the PG17 server tree.
RUN make && make install

# ---- final stage: same pinned base, extension artifacts only (no toolchain) ----
FROM ${BASE_IMAGE}
ARG AGE_VERSION

RUN apt-get update \
    && apt-get install -y --no-install-recommends --no-install-suggests \
       locales \
    && rm -rf /var/lib/apt/lists/* \
    && echo "en_US.UTF-8 UTF-8" > /etc/locale.gen \
    && locale-gen \
    && update-locale LANG=en_US.UTF-8

ENV LANG=en_US.UTF-8
ENV LC_COLLATE=en_US.UTF-8
ENV LC_CTYPE=en_US.UTF-8

COPY --from=build /usr/lib/postgresql/17/lib/age.so /usr/lib/postgresql/17/lib/
COPY --from=build /usr/share/postgresql/17/extension/age--${AGE_VERSION}.sql /usr/share/postgresql/17/extension/
COPY --from=build /usr/share/postgresql/17/extension/age.control /usr/share/postgresql/17/extension/

# Bootstrap script for a fresh graph database: creates the extension and
# sets the ag_catalog search path expected by Cypher queries. This database
# is the projected, rebuildable Outcome Graph read model only; it never
# receives a direct write that did not originate from the authoritative
# PostgreSQL outbox projector (see internal/outcomegraph, Task 0I).
RUN mkdir -p /docker-entrypoint-initdb.d \
    && { \
       echo "-- Apache AGE graph extension bootstrap (GA Task 1A)."; \
       echo "-- Independent Outcome Graph read model; never the authoritative"; \
       echo "-- WorkCase PostgreSQL store."; \
       echo "CREATE EXTENSION IF NOT EXISTS age;"; \
       echo "LOAD 'age';"; \
       echo "SET search_path = ag_catalog, \"\$user\", public;"; \
       } > /docker-entrypoint-initdb.d/00-create-extension-age.sql

LABEL org.opencontainers.image.title="agentatlas-age-postgres17" \
      org.opencontainers.image.description="PostgreSQL 17 + Apache AGE 1.6.0 graph extension for the AgentAtlas Outcome Graph. Independent of the authoritative WorkCase PostgreSQL image." \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.source="https://github.com/apache/age" \
      org.opencontainers.image.version="${AGE_VERSION}" \
      io.agentatlas.outcome-graph.provider="apache-age" \
      io.agentatlas.outcome-graph.age-release="${AGE_VERSION}" \
      io.agentatlas.outcome-graph.postgres-major="17"

CMD ["postgres", "-c", "shared_preload_libraries=age"]
