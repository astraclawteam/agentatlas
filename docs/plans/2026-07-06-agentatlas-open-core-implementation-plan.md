# AgentAtlas Open-Core Implementation Plan

> This public plan is sanitized for the open-core repository. It avoids local workspace paths, private repository names, customer details, credentials, private endpoints, and enterprise-only implementation details.

**Goal:** Build AgentAtlas as an open-core, agent-first enterprise RAG and organizational memory runtime with stable public contracts, workflow foundations, parser interfaces, retrieval, dream jobs, answer trace, and a Claw-runtime-style console.

**Architecture:** AgentAtlas uses Google ADK Go v2 for Knowledge Agent workflows, Go services for runtime APIs and workers, PostgreSQL for metadata, OpenSearch for runtime retrieval, NATS JetStream for durable events, S3-compatible object storage for artifacts, and parser sidecars for document and multimodal processing. AgentNexus remains the authority for identity, permissions, tickets, resource location, evidence reads, and audit append.

**Tech Stack:** Go, Google ADK Go v2, llmrouter through `adk-llmrouter-model`, chi, oapi-codegen, ConnectRPC, PostgreSQL, pgx, sqlc, goose, OpenSearch, NATS JetStream, S3-compatible storage, Docling sidecar, MinerU sidecar, WhisperX/faster-whisper, ffmpeg, PySceneDetect, FlowGram.AI, React Flow/xyflow, React, TypeScript, Vite, zap, OpenTelemetry, Prometheus, and offline RAG evaluation tooling.

---

## Open-Core Scope

The open-core repository owns:

- Knowledge Space model.
- Org Scope model.
- SOP and Method Outline models.
- Memory Timeline and Dream Job foundations.
- Evidence Pointer, Retrieval Plan, and Answer Trace foundations.
- AgentNexus protocol SDK.
- Workflow schema and runtime foundation.
- Parser Provider interface.
- AtlasDocument schema.
- Parser Gateway interface and basic provider clients.
- OpenSearch retrieval foundation.
- Production-standard Compose and Helm profiles.
- Open-core console and shared UI primitives.

The open-core repository must not contain:

- Customer-specific documents, SOPs, templates, migrations, credentials, or endpoints.
- Commercial parser implementation details.
- Enterprise license enforcement.
- Production private-deployment automation.
- Private roadmap, customer names, private endpoints, or secrets.

## Public Contracts

Enterprise and third-party extensions must depend on published contracts, not private implementation details:

- Go module: `github.com/astraclawteam/agentatlas/services/agentatlas`.
- Public Go SDKs: `sdk/go/*`.
- OpenAPI contracts: `services/agentatlas/api/openapi`.
- Proto contracts: `services/agentatlas/api/proto/agentatlas/*/v1`.
- Workflow schema: `services/agentatlas/schemas/workflow`.
- Parser Provider schema: `services/agentatlas/schemas/parser`.
- AtlasDocument schema: `services/agentatlas/schemas/atlasdocument`.
- OCI images: `agentatlas/<service>:<semver-or-sha>`.
- Helm chart: `agentatlas`.

Packages under `services/agentatlas/internal/*` are private implementation details.

## Locked Architecture Decisions

- Project name: `AgentAtlas`; internal identifiers use lowercase `agentatlas`.
- Service entrypoints: `atlas-api`, `atlas-agent`, `atlas-worker`, and `parser-gateway`.
- Agent framework: Google ADK Go v2.
- Model access: llmrouter only through `adk-llmrouter-model` and capability routing.
- Runtime retrieval: OpenSearch only.
- Metadata storage: PostgreSQL.
- Task/event backbone: NATS JetStream.
- Object storage: S3-compatible API.
- Parser gateway providers: Docling and MinerU enter the MVP.
- Audio parsing: WhisperX/faster-whisper.
- Video parsing: ffmpeg plus PySceneDetect.
- Editable workflow canvas: FlowGram.AI.
- Read-only graph and trace visualization: React Flow/xyflow.
- Access boundary: AgentNexus verifies tickets, locates evidence, reads evidence, and appends audit evidence.
- Data boundary: raw original documents, full OCR, full transcripts, and long unmasked chunks do not enter AgentAtlas metadata tables.

## Goal Sequence

### Goal 0: Lock Architecture Baseline

- Confirm all technology decisions, data boundaries, repository boundaries, and production-standard dependency rules.
- Publish this sanitized open-core plan.

Verification:

```powershell
$patterns = @('private '+'endpoint','customer '+'name','workspace '+'path','credential')
Select-String -Path 'docs/plans/2026-07-06-agentatlas-open-core-implementation-plan.md' -Pattern $patterns
```

Expected: no matches.

### Goal 1: Create Go Workspace And Service Skeleton

- Create `services/agentatlas`.
- Initialize Go module `github.com/astraclawteam/agentatlas/services/agentatlas`.
- Add service entrypoints for `atlas-api`, `atlas-agent`, `atlas-worker`, and `parser-gateway`.
- Add production-standard config fields, health status, logging, unit tests, and build/test commands.

Verification:

```powershell
go test ./...
go build ./cmd/atlas-api
go build ./cmd/atlas-agent
go build ./cmd/atlas-worker
go build ./cmd/parser-gateway
```

### Goal 2: Define Public Contracts And Schemas

- Define Runtime OpenAPI.
- Define Agent OpenAPI.
- Define AgentNexus client OpenAPI.
- Define workflow, parser, trace, Workflow schema, Parser Provider schema, and AtlasDocument schema.
- Add public SDK structs.

Verification:

```powershell
go test ./...
```

### Goal 3: Build Production-Standard Deployment Stack

- Add Compose and Helm assets with PostgreSQL, OpenSearch, NATS JetStream, S3-compatible storage, Parser Gateway, Docling, MinerU, ASR, and video sidecars.
- Do not add alternate runtime dependencies for integration or end-to-end paths.

Verification:

```powershell
docker compose -f services/agentatlas/deploy/compose/compose.yaml config
```

### Goal 4: Implement Persistence Foundation

- Add PostgreSQL migrations and sqlc queries for knowledge spaces, workflows, artifacts, evidence pointers, dream policies, retrieval plans, and answer traces.
- Store hashes, sanitized summaries, pointers, and grant IDs rather than raw original content.

Verification:

```powershell
go test ./tests/integration -run TestPostgresCore
```

### Goal 5: Implement AgentNexus Client And Org Space Sync

- Implement ticket verification, evidence location, evidence read, audit append, and org event subscription boundaries.
- Sync org events into employee, project group, department, business unit, and company knowledge spaces.

Verification:

```powershell
go test ./internal/nexusclient ./internal/spaces
```

### Goal 6: Implement ADK Go v2 And llmrouter Adapter

- Implement `adk-llmrouter-model`.
- Add Knowledge Agent tools for workflow draft, retrieval plan draft, and trace explanation.

Verification:

```powershell
go test ./internal/llmroutermodel ./internal/agent
```

### Goal 7: Implement Workflow Runtime

- Implement workflow draft, publish, run, schema validation, node registry, and run events.

Verification:

```powershell
go test ./internal/workflow
```

### Goal 8: Build Console And Workflow Canvas

- Build Claw-runtime-style UI primitives.
- Implement AgentAtlas dashboard, knowledge map, dream timeline, FlowGram workflow canvas, React Flow trace graph, and floating Agent launcher.

Verification:

```powershell
npm test --workspace packages/agentatlas-console
```

### Goal 9: Implement Parser Gateway And Artifact Pipeline

- Implement Parser Provider registry.
- Implement Docling, MinerU, ASR, and video provider clients.
- Create AtlasDocument, summaries, and evidence pointers.
- Keep full intermediate parsing results in object storage, not metadata tables.

Verification:

```powershell
go test ./internal/parsergateway ./internal/artifacts
```

### Goal 10: Implement OpenSearch Indexing And Retrieval

- Implement OpenSearch mapping, index jobs, hybrid retrieval, structured filters, and llmrouter rerank.

Verification:

```powershell
go test ./internal/retrieval
```

### Goal 11: Implement Dream Policy And Dream Job Runtime

- Implement Dream Policy versions, schedule, project group and department summaries, layered summary storage, and evidence pointers.

Verification:

```powershell
go test ./internal/dream
```

### Goal 12: Implement Runtime Answer API And Answer Trace

- Implement answer path for “What is my work?” with AgentNexus ticket verification, retrieval, evidence read, answer generation, trace persistence, and audit append.

Verification:

```powershell
go test ./internal/trace ./internal/app
```

### Goal 13: Implement Observability And Evaluation

- Add zap logs, OpenTelemetry traces, Prometheus metrics, audit append guarantees, and offline parser/retrieval evaluation harness.

Verification:

```powershell
go test ./internal/observability ./internal/auditrefs
```

### Goal 14: Package Open-Core Runtime

- Document Compose, Helm, open-core boundary, and MVP demo.
- Keep production private-deployment automation outside open-core.

Verification:

```powershell
git status --short
```

### Goal 15: Run End-To-End MVP Scenario

- Prove org space sync, SOP workflow generation, Dream Policy run, OpenSearch retrieval, AgentNexus evidence read, answer generation, Answer Trace, and audit append.

Verification:

```powershell
go test ./tests/e2e -run TestAgentAtlasMVP
```

## Implementation Risks

- ADK Go v2 and llmrouter adapter assumptions must be validated early and isolated behind `internal/llmroutermodel`.
- Integration tests must use production-standard dependencies so mapping, indexing, parser, queue, and object storage problems appear early.
- Public-adjacent schemas must stabilize early: Workflow, Parser Provider, AtlasDocument, Retrieval Plan, Answer Trace, OpenAPI, and Proto contracts.
- Repository boundary checks must stay active so enterprise-only material does not enter open-core.
- UI reuse should remain a small shared package; desktop-only runtime stores and native bridges must not enter the console.
