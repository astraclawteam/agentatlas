# AgentAtlas MVP walkthrough

Prereqs: Docker (Compose), Go 1.26+, pnpm 9+. All components are the
production-standard stack — no lightweight substitutes.

## 1. Start the stack

```sh
cd services/agentatlas/deploy/compose
cp .env.example .env
docker compose up -d postgres opensearch nats minio minio-init
```

## 2. Run the end-to-end MVP scenario

```sh
cd services/agentatlas
ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable \
ATLAS_TEST_OPENSEARCH=http://localhost:9200 \
ATLAS_TEST_OBJECT_ENDPOINT=http://localhost:9000 \
go test ./tests/e2e -run TestAgentAtlasMVP -v
```

The scenario covers: org-event space sync → work-brief ingestion (ticket
gated) → SOP document parsing → workflow draft + immutable publish → dream
policy + scheduled dream run (layered summaries, sealed detail in object
storage) → hybrid retrieval → AgentNexus locate/read with grants → answer →
Answer Trace with hashes/pointers/grants → mandatory audit append.

## 3. Explore the console

```sh
pnpm install
pnpm --dir packages/agentatlas-console dev   # http://localhost:5173
```

Four work surfaces: organization knowledge map, dream timeline, editable
workflow canvas (FlowGram), and the read-only answer-trace graph. The
floating Atlas Agent button talks to `POST /v1/agent/runs`.

## 4. Run atlas-api against the stack

```sh
cd services/agentatlas
ATLAS_NEXUS_BASE_URL=http://localhost:8100 \  # your AgentNexus (or mock)
ATLAS_LLMROUTER_BASE_URL=https://your-openai-compatible/v1 \
ATLAS_LLMROUTER_API_KEY=sk-... \
go run ./cmd/atlas-api
curl -s localhost:8080/healthz
```

Every runtime call carries `X-Nexus-Ticket`; without a valid ticket the API
fails closed (401/403).
