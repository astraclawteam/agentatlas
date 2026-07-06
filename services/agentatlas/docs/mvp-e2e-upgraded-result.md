# AgentAtlas Upgraded MVP End-to-End Result (Goal Z2)

Recorded 2026-07-06. `TestAgentAtlasMVPRealStack` PASS in ~32s against the
DEPLOYED stack — the completion plan's 12 UPGRADES in force simultaneously.

## Topology under test

11 compose services, all healthy (see `deploy/README.md` for the recorded
`docker compose ps`): postgres, opensearch(smartcn), nats, minio(+init),
docling/asr/video sidecars, and the four REAL service binaries — atlas-api
(:8080), atlas-agent (:8081), atlas-worker (:9091 metrics), parser-gateway
(:8090). All images digest/version-pinned; mineru-sidecar (PyPI/torch-scale
build) is the one deferred member. The test hosts the mock AgentNexus over
real HTTP on :8100 (the containers' NEXUS_BASE_URL), implementing the
`agentnexus-client.yaml` proposal contract including the org-events SSE
stream — swapping in the real AgentNexus stays a composition-root change.

## What the run proved (per UPGRADE)

- **Org sync via the deployed worker** — atlas-worker's new AgentNexus SSE
  subscription (`ATLAS_ORG_SYNC_ENTERPRISES`) created all 6 knowledge spaces
  from fixture org events.
- **UPGRADE 1 (real LLM)** — `POST /v1/answer` returned a genuinely
  synthesized multi-point summary of the ingested work briefs through real
  llmrouter (not a canned string).
- **UPGRADE 2 (agent executes)** — `POST /v1/agent/runs` on the deployed
  atlas-agent drove the real ADK loop; ≥1 `draft_workflow` tool call recorded.
- **UPGRADE 3 (real parser)** — the committed PDF fixture uploaded via
  `POST /v1/artifacts/jobs` was parsed by the REAL docling sidecar through the
  worker's async pipeline (job → succeeded).
- **UPGRADE 4 (real embeddings + rerank)** — retrieval on the answer path ran
  with real bge-m3; `TestRetrievalRealModel` separately proves paraphrase
  vector recall AND live `/v1/rerank` reorder (llmrouter capability shipped
  2026-07-06; the test flipped SKIP→PASS).
- **UPGRADE 5/6 (four binaries, full API)** — every step above went through
  the deployed binaries over their served routes; ticket auth fail-closed
  everywhere.
- **UPGRADE 7 (workflows execute)** — create → publish → `POST
  /v1/workflows/{id}/runs` → the worker claimed and ran the workflow to
  `succeeded` (trace.append persisted a real answer trace bound to the run).
- **UPGRADE 8 (observability)** — the deployed `/metrics` endpoints
  (atlas-api + atlas-worker :9091) expose live `agentatlas_` series.
- **UPGRADE 9 (stack + Helm)** — full pinned compose stack all-healthy;
  `helm lint` 0 failures + `helm template` renders 7 manifests
  (`deploy/helm/HELM-VALIDATION.md`).
- **UPGRADE 10 (AgentNexus contract)** — verify/locate/read/audit flowed over
  REAL HTTP against the contract server; 2 audit events appended.
- **UPGRADE 11/12** — oapi-codegen types committed + bidirectional route
  drift guard green; the eval harness fixtures/runner ship under
  `tools/evaluation` (execution against a scoring backend remains
  environment-dependent).

## Residuals (explicit, not silent)

- mineru-sidecar image not built in this run (multi-GB torch build); the
  gated `TestParserSidecars/mineru_pdf` leg skips loudly until it exists.
- Dream jobs execute via the in-process e2e + worker handler; the SCHEDULED
  trigger (scheduler tick loop / admin trigger route) is not yet wired into a
  binary — dream policies create+publish through the served route, runs
  dispatch when a tick source lands.
- ASR diarization runs in single-speaker fallback unless pyannote models +
  HF token are provisioned (`WHISPER_DIARIZE=1`).

## Re-run

```powershell
cd services/agentatlas/deploy/compose
$env:LLMROUTER_BASE_URL="..."; $env:LLMROUTER_API_KEY="..."
$env:ATLAS_ORG_SYNC_ENTERPRISES="ent_realstack"
docker compose up -d postgres opensearch nats minio minio-init docling-sidecar asr-sidecar video-sidecar atlas-api atlas-agent atlas-worker parser-gateway

cd ../..
$env:ATLAS_E2E_REALSTACK="1"
$env:ATLAS_TEST_POSTGRES_DSN="postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable"
go test ./tests/e2e -run TestAgentAtlasMVPRealStack -v
```
