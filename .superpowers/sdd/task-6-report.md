# Task 6 Implementation Report

## Scope

Implemented immutable hierarchical Dream output persistence and the governed Dream run HTTP surface.

## Delivered

- Runner persists exact sorted/deduplicated child-run lineage in the same PostgreSQL transaction as:
  - display, retrieval and sealed-pointer summary layers;
  - structured facts/themes/trends/risks/todos;
  - evidence links;
  - pending index jobs;
  - the Dream timeline node.
- Sealed detail is written to object storage before the database transaction. An object write failure prevents all database output. A database failure can leave only an unreachable object; the pointer, summaries and successful display remain absent.
- Run reads expose stored output only after the run reaches `succeeded`. Pending/running/failed runs return no display summary, structured output or sealed pointer.
- Sealed evidence pointers require `dream:evidence:read`.
- Added ticket-guarded routes:
  - `GET /v1/dream/overview?org_unit_id=`
  - `GET /v1/dream/runs?org_unit_id=&window=`
  - `GET /v1/dream/runs/{id}`
  - `POST /v1/dream/runs/{id}/annotations`
  - `POST /v1/dream/runs/{id}/reruns`
  - `POST /v1/dream/runs/{id}/evidence-access`
- Annotation actions are bounded to `confirm|reject|mark_incorrect|comment` and append `dream_run_annotations`; summary records are never updated.
- Evidence access requires the explicit scope, calls AgentNexus Locate + Read with the ticket, enterprise, URI and evidence pointer bound, rejects an empty Step Grant ID, and returns the sanitized detail only after the mandatory audit append succeeds.
- Wired atlas-agent to publish Dream rerun jobs and use the existing pinned-version `Scheduler.Rerun` semantics.
- Added OpenAPI routes and regenerated committed agent API types.

## TDD Evidence

First RED:

```text
go test ./internal/app -run 'DreamRun|DreamEvidence' -count=1
unknown field DreamRuns in struct literal of type AgentRouterDeps
```

Second RED:

```text
go test ./internal/dream ./internal/app -run 'PersistDreamLineage|FailedDreamRun' -count=1
undefined: persistDreamLineage
TestFailedDreamRunNeverExposesStoredSummary: failed output exposed
```

Focused GREEN:

```text
go test ./internal/dream ./internal/app -run 'PersistDreamLineage|FailedDreamRun|DreamRun|DreamEvidence' -count=1
ok internal/dream
ok internal/app
```

## Verification

Fresh dependencies:

- PostgreSQL `postgres:17-alpine`, isolated database/container on `127.0.0.1:15436`.
- MinIO `minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e`, isolated container on `127.0.0.1:19001`.

Fresh real PostgreSQL + MinIO hierarchy/API integration:

```text
go test ./tests/integration -run '^TestDreamHierarchy$' -count=1 -v
--- PASS: TestDreamHierarchy
```

This integration executes the parent published Dream workflow over two child summaries and asserts exact `parent_run_ids`, structured trends/risks/todos, coverage, sealed object detail, index job, timeline node and trace. Against the real persisted run it also proves evidence access returns 403 without `dream:evidence:read`, succeeds through a bound AgentNexus grant, and appends the mandatory audit record.

Service, contracts, static analysis and SDK:

```text
go test ./...                                  PASS
go vet ./...                                   PASS
cd sdk/go && go test ./...                     PASS (6 packages)
sqlc generate                                  PASS
go tool oapi-codegen ... atlas-runtime.yaml    PASS
go tool oapi-codegen ... atlas-agent.yaml      PASS
git diff --check                               PASS
```

Generation stability hashes were identical before and after a second generation:

```text
d6338ded977e51fc031fbdc149b838d203b724e3
d6338ded977e51fc031fbdc149b838d203b724e3
```

Windows race invocation was unavailable because the host Go toolchain has CGO disabled (`go: -race requires cgo`). The equivalent Go 1.26 Linux container verification passed:

```text
docker run --rm ... golang:1.26-bookworm go test -race ./internal/dream ./internal/app
ok internal/dream
ok internal/app
```

## Baseline Test Investigation

`TestDreamJob` fails at `dream_job_test.go:649` even on a newly created database. Systematic comparison used two isolated fresh databases:

- current Task 6 worktree: FAIL `next tick n=0 err=<nil>`;
- detached clean `e0c4485b87679674f32adbc8f65eabd48a5956b3`: identical FAIL at the same line.

Root cause is in the baseline test sequence: it deliberately changes the latest completed Dream to `running`, expires it, and reconciles it to `pending`, then immediately expects `Scheduler.Tick` to create a later window. The Task 5 scheduler correctly skips a policy whose latest run is still `pending`, so `n=0`. Task 6 does not modify scheduler readiness/retry code. This baseline assertion is outside Task 6 and was not hidden or changed.

## Concerns

- PostgreSQL and object storage cannot share one physical transaction. The design guarantees fail-closed visibility: object storage is staged first and all database-visible state is one transaction. A database failure can leave an orphaned, unreferenced object that lifecycle cleanup should eventually collect.
- The current AgentNexus public contract exposes Step Grant issuance through `ReadEvidence` rather than a separate grant-validation method. Binding is enforced by passing the verified ticket, enterprise, resource URI and evidence pointer to that call, then requiring its non-empty grant ID in the mandatory audit append.
