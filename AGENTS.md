# AgentAtlas AI Coding Rules

This file is the mandatory entry point for AI coding agents working in this repository.
Read it before editing files, running generators, or proposing implementation changes.

## Repository Role

`agentatlas` is the open-core repository for AgentAtlas.
It must remain buildable, testable, and runnable without `agentatlas-enterprise`.

Open-core contains:

- Knowledge Space, Org Scope, SOP, Method Outline, Memory Timeline, Dream Job, Evidence Pointer, Retrieval Plan, and Answer Trace foundations.
- Public SDKs, OpenAPI contracts, Proto contracts, Workflow schema, Parser Provider schema, and AtlasDocument schema.
- Parser Gateway interfaces and basic parser provider integrations.
- OpenSearch retrieval foundation.
- Production-standard Docker Compose and Helm profiles.
- The open-core AgentAtlas console and reusable Claw-runtime-style UI primitives.

Open-core must not contain:

- Customer-specific documents, SOPs, templates, migrations, credentials, or endpoints.
- Commercial parser implementation details.
- Enterprise license enforcement.
- Production private-deployment automation.
- Private roadmap, customer names, private endpoints, or secrets.

## Hard Boundaries

- Do not import code from `agentatlas-enterprise`.
- Do not require `agentatlas-enterprise` to build, test, or run this repo.
- Do not place public SDKs inside `internal`.
- Do not expose secret values in logs, test output, fixtures, or documentation.
- Do not bypass AgentNexus for identity, permission checks, evidence location, evidence reads, or audit append.
- Do not store raw original documents, full OCR, full transcripts, full frame descriptions, or long unmasked chunks in metadata tables.
- Do not add customer-specific behavior to open-core fixtures or tests.
- Do not introduce alternate runtime dependencies for integration or end-to-end paths. Use PostgreSQL, OpenSearch, NATS JetStream, S3-compatible object storage, Parser Gateway, Docling, MinerU, llmrouter, and AgentNexus client contracts.

## Public Contract Locations

Enterprise extensions may depend only on published open-core contracts:

- Go module: `github.com/astraclawteam/agentatlas/services/agentatlas`
- Public Go SDKs: `sdk/go/*`
- OpenAPI: `services/agentatlas/api/openapi`
- Proto: `services/agentatlas/api/proto/agentatlas/*/v1`
- Workflow schema: `services/agentatlas/schemas/workflow`
- Parser Provider schema: `services/agentatlas/schemas/parser`
- AtlasDocument schema: `services/agentatlas/schemas/atlasdocument`
- OCI images: `agentatlas/<service>:<semver-or-sha>`
- Helm chart: `agentatlas`

Anything under `services/agentatlas/internal/*` is private implementation detail.

## Required Working Method

Before editing:

1. Read the active plan in `docs/plans`.
2. Identify the current Goal or task.
3. Check existing patterns before adding packages, dependencies, or abstractions.
4. Keep changes scoped to the current Goal.

During implementation:

- Prefer small, testable changes.
- Write or update tests for behavior changes.
- Keep public API and SDK changes backward compatible unless an explicit migration plan exists.
- Use structured parsers and typed APIs instead of ad hoc string manipulation.
- Add comments only when they explain non-obvious business or security constraints.
- Keep generated files reproducible and document the generation command.

Before finishing:

- Run the verification command for the current Goal.
- Report any command that could not be run.
- Check that no enterprise-only material was added to open-core.
- Check that no secrets, local paths, private endpoints, or customer details were added.

## AI Safety Rules

AI agents must not:

- Invent new architecture when the plan already specifies one.
- Move enterprise-only features into open-core to make a demo easier.
- Replace Google ADK Go v2, llmrouter, PostgreSQL, OpenSearch, NATS JetStream, S3-compatible object storage, Parser Gateway, Docling, MinerU, FlowGram.AI, or AgentNexus access boundaries without explicit human approval.
- Change repository boundaries without human approval.
- Use destructive git commands such as `git reset --hard` or `git checkout --` unless explicitly requested.
- Hide failing tests or remove tests to make a task pass.
- Commit unrelated formatting churn.

If a requirement conflicts with this file, stop and ask for human direction.

