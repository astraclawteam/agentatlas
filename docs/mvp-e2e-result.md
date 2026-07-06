# AgentAtlas MVP End-to-End Result

This note records the end-to-end MVP verification (Goal 15): proof that AgentAtlas
runs as a cohesive Agent-first enterprise RAG and organizational memory system across
its core subsystems.

## Scenario

Org graph sync → knowledge spaces → employee work-brief timeline → SOP document
parsing → Knowledge Agent workflow draft → immutable workflow publish → Dream Policy
publish → Dream Job masked summaries → OpenSearch indexing → ticket-verified answer →
AgentNexus evidence read → Answer Trace persistence → audit evidence append.

## Production-standard stack

The scenario runs against the same production-standard components used everywhere else
(`deploy/compose/compose.yaml`), with no lightweight substitutes:

- PostgreSQL 17 — metadata and typed SQL access (goose migrations, sqlc queries).
- OpenSearch 3.7 with the `analysis-smartcn` plugin — the only runtime retrieval engine.
- MinIO — S3-compatible object storage for intermediate parse results and sealed summaries.

Test doubles used only where an external authority or heavy model is out of scope for a
reproducible offline run:

- AgentNexus — in-test mock of the proposal client contract (tickets, locate/read
  evidence, audit append). No enterprise access path is bypassed; every read flows
  through the client boundary.
- Parser sidecar — in-test Docling stand-in that echoes the uploaded Markdown; the real
  gateway/provider code path is exercised.
- LLM + embedder — deterministic in-process doubles so the answer and vectors are stable.
- Task bus — in-process bus in place of NATS JetStream for the single-process test.

## Command

```powershell
docker compose -f deploy/compose/compose.yaml up -d postgres opensearch minio minio-init

$env:ATLAS_TEST_POSTGRES_DSN    = "postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable"
$env:ATLAS_TEST_OPENSEARCH      = "http://localhost:9200"
$env:ATLAS_TEST_OBJECT_ENDPOINT = "http://localhost:9000"
go test ./tests/e2e -run TestAgentAtlasMVP -v
```

## Result

`PASS` in ~4.6s.

- Org events synced into 6 knowledge spaces (company, business unit, department,
  project group, two employees). Stale org events do not overwrite newer space state.
- MES exception work-order SOP parsed through the artifact pipeline; only hashes,
  sanitized summaries, structural metadata, and evidence pointers land in PostgreSQL —
  the intermediate parse result stays in object storage.
- Knowledge Agent tool drafted a 5-node SOP workflow; published as immutable version 1.
- Project-group Dream Policy published; Dream Job produced a masked `display` summary
  carrying the risk signal (`风险信号`), with the detailed layer sealed to object storage
  behind an evidence pointer.
- Employee question「我的工作内容是什么？」:
  - ticketless `POST /v1/answer` fails closed → `401 Unauthorized`;
  - ticket-verified call → `你当前的主要工作是 MES 异常工单专项：已完成分拣规则联调与版本复核，遗留风险为接口限流方案未定。`
- Answer Trace persisted with `question_hash`, `answer_hash`, `retrieval_plan_id`,
  `model_route`, `space_ids`, `evidence_pointer_ids`, and `agentnexus_read_grant_ids`
  (three authorized grants across two employees and two dates).
- Audit chain received the `AnswerTraceCreated` event bound to the trace id.

## Boundary notes

- The open-core repository builds, tests, and runs this scenario without importing any
  `agentatlas-enterprise` code.
- No raw original documents, full OCR, full transcripts, or long unmasked chunks are
  stored in AgentAtlas metadata tables.
- AgentNexus remains the authority for identity, permission, ticket verification,
  resource location, evidence read, and audit append.
