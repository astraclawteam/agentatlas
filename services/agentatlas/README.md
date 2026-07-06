# agentatlas core service

Open-core runtime of AgentAtlas: an agent-first enterprise RAG and
organizational memory system. Four binaries share this module:

| Binary | Role |
|---|---|
| `atlas-api` | Stable runtime API: answer, ingestion, spaces, traces |
| `atlas-agent` | Agent-first control plane: Knowledge Agent runs, workflow drafts, confirmations |
| `atlas-worker` | Async execution: dream jobs, index jobs, artifact processing, workflow runs |
| `parser-gateway` | Parser sidecar scheduling (Docling, MinerU, ASR, video) → AtlasDocument |

## Build and test

```sh
make build   # go build ./...
make test    # go test ./...
```

Configuration is YAML + `ATLAS_*` env overlay (see `internal/config`). All
environments use the same production-standard dependencies: PostgreSQL,
OpenSearch, NATS JetStream, S3-compatible object storage, and parser
sidecars. See `../../docs/plans/agentatlas-goal-mode-implementation-plan.md`
for the roadmap and data boundary rules.
