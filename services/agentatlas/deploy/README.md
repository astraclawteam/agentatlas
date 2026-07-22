# AgentAtlas deployment

One rule everywhere: development, integration tests, POC, SaaS-like and
private-like validation all run the same production-standard components —
PostgreSQL, OpenSearch (with `analysis-smartcn`), NATS JetStream,
S3-compatible object storage, and the parser sidecars. Unit tests may mock
external clients; integration/e2e tests and every deployed environment must
use this stack. Do not introduce SQLite, in-memory queues, or vector-only
stores as runtime shortcuts.

## Local stack (Docker Compose)

Prerequisite: an **agentnexus checkout beside this repository**.
`services/agentatlas/go.mod` replaces
`github.com/astraclawteam/agentnexus/sdk/go/runtime` with
`../../../agentnexus/sdk/go/runtime`, a path outside this repository, so the Go
service images are built from two contexts: the agentatlas repo root plus a
named `nexus-runtime-sdk` context pointing at that sibling. Compose wires both.
If the checkouts are not siblings, set `ATLAS_NEXUS_RUNTIME_SDK_DIR` (relative
to `deploy/compose/`, or absolute) before building. Without it every Go service
fails at `go mod download`. See the DECISION NOTE in `docker/Dockerfile` for why
this is a workaround rather than the fix.

```sh
cd deploy/compose
cp .env.example .env        # adjust credentials / mirrors
docker compose config -q    # validate
docker compose build        # builds Go services, OpenSearch+smartcn, sidecars
docker compose up -d
```

To check the container build on its own — the gate that CI runs, and the only
check that fails when the image build breaks (`go build`/`go test` do not):

```sh
make -C services/agentatlas docker-build-check
```

### CI prerequisite: the `AGENTNEXUS_READ_TOKEN` repository secret

Because the sibling checkout is a prerequisite for *every* Go command in
`services/agentatlas` — not only the image build — GitHub Actions has to fetch
`astraclawteam/agentnexus` before it can run any of them. It does that with a
repository secret named **`AGENTNEXUS_READ_TOKEN`**, holding a token with read
access to that repository. `.github/workflows/build-test.yml` uses it for the
`actions/checkout` of the sibling, and both the pull-request gate (`pr.yml`) and
the release workflow (`release.yml`) depend on it.

If the secret is absent, the gate stops at a preflight step that names it,
rather than failing later with an opaque `actions/checkout` 404 or a Go
`replacement directory ... does not exist` error. Configure it under
*Settings → Secrets and variables → Actions*. Two consequences worth knowing:

- Pull requests opened from a **fork** never receive repository secrets, so the
  gate cannot pass on fork PRs. No fork can build this repository either, for
  the same underlying reason.
- The token requirement disappears entirely once the AgentNexus runtime SDK is a
  published module — see the DECISION NOTE in `docker/Dockerfile`.

The previous browser-session encryption key is not required on a first
install. During a key rotation, set
`ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_ID` and
`ATLAS_BROWSER_SESSION_PREVIOUS_ENCRYPTION_KEY_FILE_SOURCE`, then include the
rotation override:

```sh
docker compose -f compose.yaml -f compose.rotation.yaml config -q
docker compose -f compose.yaml -f compose.rotation.yaml up -d atlas-agent
```

Remove the override and both previous-key settings after the rotation window.

Verify:

```sh
curl -s http://localhost:9200/_cluster/health          # OpenSearch
curl -s -X POST http://localhost:9200/_analyze \
  -H 'Content-Type: application/json' \
  -d '{"analyzer":"smartcn","text":"研发一部异常工单"}' # Chinese tokenization
curl -s http://localhost:8222/healthz                   # NATS
curl -s http://localhost:9000/minio/health/live        # MinIO
curl -s http://localhost:5003/healthz                   # ASR sidecar
curl -s http://localhost:5004/healthz                   # video sidecar
curl -s http://localhost:5001/health                    # docling-serve
curl -s http://localhost:9091/readyz                    # atlas-worker
curl -s http://localhost:9092/readyz                    # atlas-outcome-projector
curl -s http://localhost:9092/metrics | grep projection_lag  # graph freshness
```

Readiness versus liveness on the two services with no API port: `/healthz`
stays 200 while the process is alive, so the container does not flap, and
`/readyz` answers 503 with a stated reason when the service is not doing its
job. Both are worth asking directly, because both failures are invisible
otherwise — a `healthy` atlas-worker whose AgentNexus org-version
subscription is failing on every attempt still serves `/metrics` perfectly,
and a projector whose graph has gone stale still runs. The projector's
`agentatlas_outcome_projection_lag_events` is the number that says how far
behind the Outcome Graph has fallen; a value that stops falling is a stalled
projection.

`outcome-graph` is a second PostgreSQL instance with the Apache AGE extension
compiled in, separate from the authoritative store and holding only the
projected, rebuildable read model. Its first build compiles AGE from the
pinned source archive, which is slow once and cached after.

Notes:

- `docling-sidecar` uses the official image (available on both `ghcr.io` and
  `quay.io`). `mineru-sidecar` is built from the official MinerU PyPI package
  because no official registry image exists; third-party mirrors are not used.
- MinerU is AGPL-3.0 and runs strictly as a separate sidecar process invoked
  over HTTP (see the third-party license matrix in the technical
  architecture spec).
- ASR/MinerU are CPU-capable but slow without a GPU; set `WHISPER_MODEL=tiny`
  for smoke runs.
- On China networks set `GOPROXY_BUILD=https://goproxy.cn,direct` and a
  domestic `PIP_INDEX_URL` in `.env` before `docker compose build`.
- `LLMROUTER_BASE_URL` accepts any OpenAI-compatible endpoint. It reaches the
  services as `ATLAS_LLMROUTER_BASE_URL`, and it has no default: an endpoint
  that resolves to nothing reads as configured and fails at the first model
  call instead of at boot.
- Model access degrades, it does not abort. With `LLMROUTER_BASE_URL` unset,
  `atlas-agent` and `atlas-api` still start and serve; `/healthz` answers
  `ready:false` with a `reason` naming the missing variable, `POST
  /v1/agent/runs` answers 503 `agent_unavailable`, and `POST /v1/answer`
  answers 503 `generation_unavailable`. Everything that needs no model —
  workflows, dream policies, governed changes, the Console BFF, space/timeline/
  trace queries, work-brief ingestion — is unaffected.
- `LLMROUTER_API_KEY` is separate: with an endpoint but no key, retrieval runs
  keyword-only (no vectors, no rerank) and `atlas-worker` runs its
  deterministic path. This is reported in each service's startup log line.
- The AgentNexus mock server ships with the org-sync work (Goal 5+); until
  then `NEXUS_BASE_URL` is a placeholder.

## Helm

```sh
helm lint deploy/helm/agentatlas
helm template atlas deploy/helm/agentatlas > /tmp/agentatlas.yaml
```

For a browser-session key rotation, set both
`config.browserSessionPreviousEncryptionKeyId` and
`config.browserSessionPreviousEncryptionKey`. Leaving both empty omits the
previous-key environment variables and Secret projection.

The chart deploys the four AgentAtlas services and consumes PostgreSQL,
OpenSearch, NATS, object storage, llmrouter, and AgentNexus as configurable
endpoints (`values.yaml -> config`). Production private-deployment control
(offline packages, license enforcement) lives in the enterprise repository.

`helm lint` runs in CI; local machines without helm rely on
`docker compose config` plus the integration/e2e suites for validation.

The chart consumes already-built images and cannot build them: Helm has no
equivalent of the named build context that the compose stack uses to reach the
sibling agentnexus checkout. Until the AgentNexus runtime SDK is a published
module, images must be produced through compose or `docker build` first — see
the DECISION NOTE in `docker/Dockerfile`.

Boundary and demo docs: `docs/specs/open-core-boundary.md`,
`docs/mvp-demo.md`.

## Real parser sidecars in tests (Goal C1)

`ATLAS_PARSER_SIDECARS=1` switches the parser legs of the test suite from
in-process stand-ins to the REAL sidecars:

```powershell
cd deploy/compose
docker compose up -d docling-sidecar mineru-sidecar asr-sidecar video-sidecar

cd ../..
$env:ATLAS_PARSER_SIDECARS = "1"
go test ./tests/integration -run TestParserSidecars -v   # per-sidecar legs (unreachable sidecars skip loudly)
go test ./tests/e2e -run TestAgentAtlasMVP -v            # MVP parse step now runs through real docling
```

Override endpoints with `ATLAS_TEST_DOCLING_URL` / `ATLAS_TEST_MINERU_URL` /
`ATLAS_TEST_ASR_URL` / `ATLAS_TEST_VIDEO_URL` (defaults match compose ports
5001/5002/5003/5004). First ASR run downloads whisper models — allow minutes.

## Full stack up (Goal Z1, recorded 2026-07-06)

`docker compose up -d` (all images digest/version-pinned; mineru-sidecar builds
from PyPI separately — torch-scale download):

```
NAME                           STATUS
agentatlas-asr-sidecar-1       Up About a minute (healthy)
agentatlas-atlas-agent-1       Up About a minute (healthy)
agentatlas-atlas-api-1         Up 32 seconds (healthy)
agentatlas-atlas-worker-1      Up 32 seconds (healthy)
agentatlas-docling-sidecar-1   Up About a minute (healthy)
agentatlas-minio-1             Up About a minute (healthy)
agentatlas-nats-1              Up 5 hours (healthy)
agentatlas-opensearch-1        Up About a minute (healthy)
agentatlas-parser-gateway-1    Up 57 seconds (healthy)
agentatlas-postgres-1          Up 5 hours (healthy)
agentatlas-video-sidecar-1     Up About a minute (healthy)
```

Build note: on a 4 GB Docker Desktop VM build the images SEQUENTIALLY
(`docker compose build <svc>` one at a time) — a parallel 6-target build can
OOM BuildKit. Helm validation evidence lives in `deploy/helm/HELM-VALIDATION.md`.
