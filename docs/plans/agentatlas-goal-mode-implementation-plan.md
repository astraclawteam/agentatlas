# AgentAtlas Implementation Plan (Public)

AgentAtlas is an open-core, agent-first enterprise RAG and organizational memory system. It organizes enterprise knowledge as org-driven knowledge spaces, SOP/method libraries, work-brief memory timelines, periodic "dream" summaries, and evidence pointers — all retrievable by agents with full answer traceability.

AgentAtlas never becomes the authority for identity or permissions and never stores raw enterprise documents. A companion gateway (AgentNexus) owns identity, org graph, access tickets, resource location, evidence reads, and the audit chain. AgentAtlas stores summaries, indexes, structured SOP steps, timeline nodes, evidence pointers, and answer traces only.

## Architecture Baseline

- Backend: Go. Module `github.com/astraclawteam/agentatlas/services/agentatlas`.
- Agent runtime: Google ADK Go v1.x (module `google.golang.org/adk`).
- Model access: `adk-llmrouter-model` adapter targeting the OpenAI-compatible Chat Completions contract. Any OpenAI-compatible endpoint works; a capability-routing gateway (llmrouter) is the recommended deployment.
- HTTP API: chi + oapi-codegen (OpenAPI-first). Internal RPC: ConnectRPC.
- Metadata store: PostgreSQL (sqlc + pgx, goose migrations).
- Retrieval: OpenSearch only, across all environments — keyword, phrase, structured filters, vector, and hybrid retrieval. Chinese tokenization via the `analysis-smartcn` plugin. SaaS tenant isolation uses per-enterprise indexes (`atlas-{enterprise_id}-{kind}`) plus an `enterprise_id` field as defense in depth.
- Events and jobs: NATS JetStream. Business task state: a minimal task runner over PostgreSQL (`internal/tasks`), no Temporal in the MVP.
- Object storage: any S3-compatible API (cloud S3, MinIO, or enterprise object storage).
- Parsing: Parser Provider interface + sidecars. Docling (default), MinerU (complex PDF/long documents), ASR sidecar (WhisperX / faster-whisper), video sidecar (ffmpeg + PySceneDetect). ASR may alternatively be served by a cloud model gateway; both back the same Parser Provider interface.
- Editable workflow canvas: FlowGram.AI wrapped as `AtlasWorkflowCanvas`. Read-only graphs (knowledge map, evidence chain, answer trace): React Flow / xyflow.
- Frontend: React + TypeScript + Vite, using the AstraClaw runtime design language via the `packages/claw-runtime-ui` primitives package.
- Observability: zap, OpenTelemetry, Prometheus.
- Delivery: Docker Compose and Helm, both running the same production-standard components.

Third-party parser licensing: Docling (MIT), MinerU (AGPL-3.0, isolated as a separate sidecar process invoked over HTTP), WhisperX (BSD), faster-whisper (MIT), PySceneDetect (BSD-3), ffmpeg (LGPL/GPL depending on build). The AgentAtlas codebase itself is MIT.

## Production-Standard Development Rule

Development, integration testing, POC, SaaS, and private deployments all use the same core dependencies: PostgreSQL, OpenSearch, NATS JetStream, S3-compatible object storage, parser sidecars, and the AgentNexus client contract. Unit tests may mock external clients; integration and end-to-end tests must run the real stack. No SQLite, in-memory queues, or vector-only stores as runtime shortcuts.

## Repository Layout

```text
agentatlas/
├─ services/agentatlas/
│  ├─ cmd/                 # atlas-api, atlas-agent, atlas-worker, parser-gateway
│  ├─ internal/            # agent, app, artifacts, config, dream, llmroutermodel,
│  │                       # nexusclient, parsergateway, retrieval, spaces, storage,
│  │                       # tasks, trace, workflow, observability, auditrefs
│  ├─ api/                 # openapi/, proto/
│  ├─ db/                  # migrations/, queries/, generated/
│  ├─ schemas/             # workflow/, parser/, atlasdocument/
│  ├─ sidecars/            # asr-sidecar/, video-sidecar/
│  ├─ deploy/              # compose/, helm/
│  └─ tests/               # integration/, e2e/, fixtures/
├─ sdk/go/                 # standalone module: workflow, parser, atlasdocument, nexus
├─ packages/
│  ├─ claw-runtime-ui/     # UI primitives and design tokens
│  └─ agentatlas-console/  # knowledge map, dream timeline, workflow canvas, trace graph
└─ docs/
```

## Public Contracts

- OpenAPI: `services/agentatlas/api/openapi` (runtime answer API, agent control plane, AgentNexus client proposal).
- Proto: `services/agentatlas/api/proto/agentatlas/*/v1`.
- JSON Schemas: workflow, parser provider, AtlasDocument under `services/agentatlas/schemas`.
- Go SDK: `sdk/go` as a standalone module; downstream extensions must depend on SDK/API/schema contracts and must not import `internal` packages.
- All runtime, agent, and admin endpoints authenticate via gateway-issued tickets (`X-Nexus-Ticket`); local development uses tickets from the bundled mock gateway.

## Goal Sequence

| Goal | Scope |
|---|---|
| 0 | Lock architecture baseline and publish this plan |
| 1 | Go workspace and four service skeletons (atlas-api, atlas-agent, atlas-worker, parser-gateway) |
| 2 | Public contracts: OpenAPI, Proto, JSON Schemas, standalone Go SDK |
| 3 | Production-standard Compose and Helm stack, sidecar images, OpenSearch smartcn image |
| 4 | PostgreSQL migrations and typed queries for all core entities (incl. work briefs and timeline nodes) |
| 5 | AgentNexus client boundary and org-event-driven knowledge space sync |
| 6 | ADK Go + OpenAI-compatible model adapter, Knowledge Agent tools |
| 7 | Workflow runtime: draft → immutable published version → run, 16 built-in node types |
| 8 | Console: knowledge map, dream timeline, editable workflow canvas, read-only trace graph |
| 9 | Parser gateway, artifact pipeline, long-image tiling, minimal task runner |
| 10 | OpenSearch indexing, hybrid retrieval, rerank, retrieval plans |
| 11 | Dream policies and scheduled dream jobs with layered summaries |
| 12 | Runtime answer API with ticket verification, evidence reads, answer traces, audit append |
| 13 | Observability, audit append guarantees, offline parser/retrieval evaluation |
| 14 | Open-core packaging and extension boundary documentation |
| 15 | End-to-end MVP scenario |

## MVP Definition of Done

1. The gateway holds an enterprise org graph and issues a case ticket for an employee.
2. AgentAtlas syncs org events and creates employee, project group, and department knowledge spaces.
3. The desktop agent pushes work brief summaries and evidence pointers through `POST /v1/work-briefs`, producing timeline nodes.
4. An admin turns an uploaded SOP document into an editable workflow draft, adjusts it on the canvas, and publishes an immutable version.
5. An admin publishes a dream policy; a scheduled dream job produces layered summaries (display summary, retrieval summary, sealed detail pointer).
6. An employee asks "我的工作内容是什么?"; AgentAtlas verifies the ticket, retrieves from OpenSearch, reads authorized evidence through the gateway, generates an answer, and records a full answer trace.
7. Audit evidence is appended to the gateway audit chain; the console shows the knowledge map, dream timeline, workflow canvas, and answer trace.
8. The whole scenario runs on the production-standard Compose stack.

## Data Boundaries

Allowed in AgentAtlas: short summaries, sanitized snippets, outlines, tags, keyword/vector index payloads, timeline nodes, evidence pointers, structured SOP steps, method outlines, answer traces, dream summaries.

Never stored in AgentAtlas: raw originals, attachments, full OCR output, full transcripts, frame-by-frame video descriptions, long unmasked chunks, financial/MES/contract/mail raw details. Full parse intermediates go to object storage under gateway control; AgentAtlas keeps pointers.
