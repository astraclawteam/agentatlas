# AgentAtlas deployment

One rule everywhere: development, integration tests, POC, SaaS-like and
private-like validation all run the same production-standard components —
PostgreSQL, OpenSearch (with `analysis-smartcn`), NATS JetStream,
S3-compatible object storage, and the parser sidecars. Unit tests may mock
external clients; integration/e2e tests and every deployed environment must
use this stack. Do not introduce SQLite, in-memory queues, or vector-only
stores as runtime shortcuts.

## Local stack (Docker Compose)

```sh
cd deploy/compose
cp .env.example .env        # adjust credentials / mirrors
docker compose config -q    # validate
docker compose build        # builds Go services, OpenSearch+smartcn, sidecars
docker compose up -d
```

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
```

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
- `LLMROUTER_BASE_URL` accepts any OpenAI-compatible endpoint.
- The AgentNexus mock server ships with the org-sync work (Goal 5+); until
  then `NEXUS_BASE_URL` is a placeholder.

## Helm

```sh
helm lint deploy/helm/agentatlas
helm template atlas deploy/helm/agentatlas > /tmp/agentatlas.yaml
```

The chart deploys the four AgentAtlas services and consumes PostgreSQL,
OpenSearch, NATS, object storage, llmrouter, and AgentNexus as configurable
endpoints (`values.yaml -> config`). Production private-deployment control
(offline packages, license enforcement) lives in the enterprise repository.

`helm lint` runs in CI; local machines without helm rely on
`docker compose config` plus the integration/e2e suites for validation.

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
