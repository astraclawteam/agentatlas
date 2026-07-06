# Codegen Decision (locked 2026-07-06)

Goal B7 of the completion plan requires eliminating the "spec exists but
generates nothing" state. Decision:

## OpenAPI — oapi-codegen ADOPTED (types)

- `github.com/oapi-codegen/oapi-codegen/v2` is pinned as a `tool` directive in
  `go.mod`; `make generate` regenerates the committed types:
  - `api/openapi/atlas-runtime.yaml` → `api/openapi/gen/runtime` (package `runtimeapi`)
  - `api/openapi/atlas-agent.yaml` → `api/openapi/gen/agent` (package `agentapi`)
- The generated packages are committed and are part of the public contract
  surface (enterprise code may consume them alongside `sdk/go`).
- Two packages, not one: both specs declare an `Error` component; a single
  package would collide.
- Drift guards (`internal/app/contract_drift_test.go`):
  - **Route coverage, both directions** — every route served by `NewRouter` /
    `NewAgentRouter` must appear in its spec and every spec path must be
    served. This is what caught (and forced the fix of) two real drifts:
    `/v1/artifacts/jobs` was declared on the agent spec but served by
    atlas-api, and `/v1/workflows/{id}/runs` was served but undeclared.
  - The test suite compiles against the generated packages, so regenerating
    without committing breaks the build.
- Handlers keep their hand-written DTOs for now; adopting the generated server
  interfaces is deliberate follow-up work once the contract stabilizes —
  request/response shape parity is enforced through the route-coverage guard
  and the committed generated types.

## Proto / ConnectRPC — DEFERRED (locked out with a drift guard)

- AgentAtlas is a modular monolith today; all external consumption goes
  through the OpenAPI contracts and `sdk/go`. There is no cross-service RPC
  boundary that would justify buf/protoc + ConnectRPC codegen yet.
- The proto files under `api/proto/agentatlas/*/v1` stay hand-authored
  versioned documents describing the future service split.
- Drift guard: `TestProtoMirrorsSDK` fails when `workflow.proto` loses naming
  parity with `sdk/go/workflow` (messages `Workflow`/`Node`/`Edge`, fields
  `workflow_id`/`risk_level`/`requires_confirmation`, `WorkflowService`).
- Revisit trigger: the first real service split (atlas-agent or parser-gateway
  moving out of process with typed RPC) adopts buf + ConnectRPC, and the
  hand-authored protos become the source for generated stubs at that point.
