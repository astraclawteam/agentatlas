# Open-core / enterprise boundary

## What lives where

Open-core (this repository, MIT):

- Knowledge Space / Org Scope models and org-event sync
- SOP & Method models, Workflow schema + runtime, 16 built-in node types
- Parser Provider interface, parser-gateway, baseline sidecars (docling-serve,
  MinerU-from-PyPI, faster-whisper ASR, ffmpeg+PySceneDetect video)
- Retrieval Plan + OpenSearch-only retrieval (smartcn keyword, kNN vectors,
  RRF hybrid, llmrouter embedding/rerank clients)
- Work Brief ingestion, Memory Timeline, Dream Job foundation
- Evidence Pointer, Answer Trace, AgentNexus client contract + mock
- Production-standard Compose/Helm profiles, console (FlowGram canvas,
  React Flow read-only graphs, AstraClaw design tokens)

Enterprise (private repository):

- Industry SOP packs, knowledge governance packs, dream strategy packs
- Advanced/commercial parser providers (PaddleOCR, commercial OCR, layout
  restoration beyond the baseline)
- Customer migration tools (incl. ragflow-importer), customer templates
- Production private-deployment control, offline packages
- Commercial license issuance and enforcement

## Consumption contract

Enterprise code depends ONLY on published open-core contracts:

- Go modules: `github.com/astraclawteam/agentatlas/sdk/go` and released
  `services/agentatlas` binaries/images — never `internal/*` imports.
- OpenAPI (`services/agentatlas/api/openapi`), Proto (`api/proto`),
  JSON Schemas (`schemas/workflow|parser|atlasdocument`).
- OCI images `agentatlas/<service>:<version>`; Helm chart `agentatlas`.
- Release tags: `services/agentatlas/vX.Y.Z` and `sdk/go/vX.Y.Z`.

Enterprise parser providers implement the Parser Provider schema and register
through the parser-gateway capability registry; they never bypass the
AtlasDocument output contract.

## Hard rules

- `agentatlas` builds, tests, and runs with no enterprise code present.
- Customer names, customer data, credentials, private deployment secrets, and
  commercial roadmaps never enter this repository.
- Raw originals / full OCR / full transcripts stay out of AgentAtlas metadata
  tables in BOTH editions — the data boundary is not a commercial feature.
- Identity, permission, ticket, resource location, evidence read, and audit
  append authority stays in AgentNexus in both editions.

## Third-party licenses (parser sidecars)

Docling (MIT), MinerU (AGPL-3.0 — isolated as a separate sidecar process over
HTTP; enterprise modifications to MinerU itself trigger AGPL obligations),
faster-whisper (MIT), WhisperX (BSD), PySceneDetect (BSD-3), ffmpeg
(LGPL/GPL depending on build), PaddleOCR (Apache-2.0, enterprise optional).
