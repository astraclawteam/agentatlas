# AgentAtlas Public Contract Freeze Report

## Scope

- Worktree: `F:/xiaozhiclaw/.worktrees/agentatlas-contracts`
- Production contract: `services/agentatlas/api/openapi/atlas-agent.yaml`
- Generator config/output: `services/agentatlas/api/openapi/oapi-codegen-agent.yaml` and `services/agentatlas/api/openapi/gen/agent/types.gen.go`
- Contract test: `services/agentatlas/internal/app/contract_drift_test.go`
- Added schema components only: `WorkflowRef`, `DreamPolicy`, `DreamSummary`, `DreamRun`, `ChangeDraft`, and `PublishDecision`
- No routes, handlers, persistence, or runtime services were added.

## RED evidence

### Interrupted token test, unchanged

The inherited uncommitted token-presence test was run before any production edit.

Command:

```powershell
cd F:/xiaozhiclaw/.worktrees/agentatlas-contracts/services/agentatlas
go test ./internal/app -run Contract -count=1
```

Output:

```text
--- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas (0.00s)
    contract_drift_test.go:31: contract missing WorkflowRef
FAIL
FAIL    github.com/astraclawteam/agentatlas/services/agentatlas/internal/app    0.133s
FAIL
```

This was the expected RED: the first required public component was absent.

### Strengthened exact-shape test

While production remained unchanged, the inherited raw-token check was replaced with YAML parsing and assertions covering exact component property sets, required and optional fields, types, enums, array item types, references, positive/non-negative integer constraints, RFC3339 `date-time` formats, and open object-map semantics.

Command:

```powershell
cd F:/xiaozhiclaw/.worktrees/agentatlas-contracts/services/agentatlas
gofmt -w internal/app/contract_drift_test.go
go test ./internal/app -run AgentContractIncludesDreamAndGovernanceSchemas -count=1
```

Output:

```text
--- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas (0.00s)
    --- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas/WorkflowRef (0.00s)
        contract_drift_test.go:33: contract missing schema WorkflowRef
    --- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas/DreamPolicy (0.00s)
        contract_drift_test.go:40: contract missing schema DreamPolicy
    --- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas/DreamSummary (0.00s)
        contract_drift_test.go:64: contract missing schema DreamSummary
    --- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas/DreamRun (0.00s)
        contract_drift_test.go:81: contract missing schema DreamRun
    --- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas/ChangeDraft (0.00s)
        contract_drift_test.go:110: contract missing schema ChangeDraft
    --- FAIL: TestAgentContractIncludesDreamAndGovernanceSchemas/PublishDecision (0.00s)
        contract_drift_test.go:130: contract missing schema PublishDecision
FAIL
FAIL    github.com/astraclawteam/agentatlas/services/agentatlas/internal/app    0.133s
FAIL
```

All six component subtests failed for the intended missing-schema reason.

### Generated public models RED

After the schemas were added, compile-time references demonstrated that the existing generator pruned route-free public components.

Command:

```powershell
cd F:/xiaozhiclaw/.worktrees/agentatlas-contracts/services/agentatlas
go test ./internal/app -run Contract -count=1
```

Output:

```text
internal\app\contract_drift_test.go:345:15: undefined: agentapi.WorkflowRef
internal\app\contract_drift_test.go:346:15: undefined: agentapi.DreamPolicy
internal\app\contract_drift_test.go:347:15: undefined: agentapi.DreamRun
internal\app\contract_drift_test.go:348:15: undefined: agentapi.DreamSummary
internal\app\contract_drift_test.go:349:15: undefined: agentapi.ChangeDraft
internal\app\contract_drift_test.go:350:15: undefined: agentapi.PublishDecision
FAIL    github.com/astraclawteam/agentatlas/services/agentatlas/internal/app [build failed]
FAIL
```

`output-options.skip-prune: true` was therefore added to the agent model generator so public components remain available even though Task 1 intentionally adds no routes.

### Existing generated symbol compatibility RED

Self-review of the first unpruned output found enum literal collisions had renamed existing exported constants. Compile guards for the pre-existing symbols reproduced that compatibility regression:

```text
internal\app\contract_drift_test.go:351:15: undefined: agentapi.High
internal\app\contract_drift_test.go:354:15: undefined: agentapi.Completed
internal\app\contract_drift_test.go:358:15: undefined: agentapi.PointerOnly
internal\app\contract_drift_test.go:360:15: undefined: agentapi.AgentAnswers
FAIL    github.com/astraclawteam/agentatlas/services/agentatlas/internal/app [build failed]
FAIL
```

The root cause was duplicate enum value names across old and newly unpruned components. Unique `x-enum-varnames` were assigned only to the new enums; regeneration then preserved every old exported constant while emitting the new models.

## GREEN evidence

### Focused contract suite

Command:

```powershell
cd F:/xiaozhiclaw/.worktrees/agentatlas-contracts/services/agentatlas
go test ./internal/app -run Contract -count=1
```

Output:

```text
ok      github.com/astraclawteam/agentatlas/services/agentatlas/internal/app    0.125s
```

This covers exact OpenAPI shapes, route drift, generated model presence, and preservation of existing generated enum constants.

### Generator reproducibility

The host does not provide a `make` executable. The literal command failed as follows:

```text
make : The term 'make' is not recognized as the name of a cmdlet, function, script file, or operable program.
CategoryInfo : ObjectNotFound: (make:String) [], CommandNotFoundException
```

The two exact commands from the Makefile's `generate` recipe were run twice instead:

```powershell
go tool oapi-codegen -config api/openapi/oapi-codegen-runtime.yaml api/openapi/atlas-runtime.yaml
go tool oapi-codegen -config api/openapi/oapi-codegen-agent.yaml api/openapi/atlas-agent.yaml
```

Hashes after the first and second final recipe runs:

```text
api/openapi/gen/runtime/types.gen.go
  first:  BE36580DA396045190162D0943863C2674D2F5F01593F945C064A31B0A12B173
  second: BE36580DA396045190162D0943863C2674D2F5F01593F945C064A31B0A12B173
  stable:  true

api/openapi/gen/agent/types.gen.go
  first:  1FC5DE02A4A8F0505B66346EEC4CF98C7111F2A0854FAB5855B5E9A85123BD23
  second: 1FC5DE02A4A8F0505B66346EEC4CF98C7111F2A0854FAB5855B5E9A85123BD23
  stable:  true
```

The runtime generated file has no Git diff; only the required agent generated file changed.

### Full Go suite

Command:

```powershell
cd F:/xiaozhiclaw/.worktrees/agentatlas-contracts/services/agentatlas
go test ./...
```

Output summary:

```text
All tested command, internal, schema, integration, and E2E packages passed.
Exit code: 0
```

Passing packages include `internal/app`, `internal/dream`, `internal/workflow`, `schemas`, `tests/integration`, and `tests/e2e`.

## Self-review

- The OpenAPI production diff is confined to `components.schemas`; existing operations and component names are preserved.
- `WorkflowRef.version`, policy/run workflow versions, attempts, and revisions have explicit integer floors.
- Dream input sources use the eight required singular lower-snake-case enum values.
- Dream runs expose immutable identity/window/version lineage, object-map input and visibility snapshots, model metadata, coverage/missing inputs, summaries, rerun lineage, and idempotency.
- Dream summaries expose display, retrieval, sealed-pointer, structured signal, coverage/missing-input, and evidence-pointer layers.
- `ChangeDraft` contains enterprise, organization, resource, action, requester, revision/state/base version, open proposed content, and RFC3339 timestamps.
- `PublishDecision` contains deterministic risk/routing state, optional reviewer, organization path, and idempotency.
- Snapshots and proposed content are objects with `additionalProperties: true`; sensitive identifiers are strings; property names are lower snake case.
- New enums have unique generator names so existing exported generated constants remain source-compatible.
- No enterprise-only material, customer details, secrets, routes, runtime services, persistence, or unrelated formatting changes were introduced.

## Concern

The only environmental limitation is that GNU Make is not installed. Generator stability was proven by executing the Makefile recipe commands directly twice and comparing SHA-256 hashes.
