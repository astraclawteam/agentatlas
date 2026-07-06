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
