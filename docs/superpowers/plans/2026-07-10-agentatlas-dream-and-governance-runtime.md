# AgentAtlas Dream and Governance Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the existing AgentAtlas Dream prototype and direct-publish control plane into a hierarchical, workflow-driven, immutable, auditable enterprise-memory and knowledge-governance runtime.

**Architecture:** Published Dream policies pin published Dream workflow versions; schedulers create immutable input/visibility snapshots from local records plus lower-level sanitized Dream summaries; workflow runs produce immutable layered outputs and lineage. A separate governance service owns suggestions, versioned drafts, deterministic risk, upward review, optimistic concurrency, idempotent publish, and browser BFF sessions backed by AgentNexus.

**Tech Stack:** Go, chi, PostgreSQL/sqlc, task runner, Atlas Workflow runtime, AgentNexus client, OpenSearch, object storage, OpenTelemetry

## Global Constraints

- Higher-level Dream consumes lower-level sanitized Dream Summary by default and never silently flattens all employee raw briefs.
- Raw evidence drill-down requires a resource-bound AgentNexus Step Grant and a successful audit append.
- Dream output is immutable and binds policy version, workflow version, input snapshot, visibility snapshot, model route/version, window, and parent run IDs.
- Dream has display, retrieval, and sealed-pointer layers; inputs are sanitized before model invocation, outputs are checked against visibility before persistence, and sealed details remain in object storage.
- Dream stages extract facts/themes, trends, risks, todos, coverage, and missing inputs as structured fields.
- Scheduler uses enterprise timezone, deterministic window IDs, child-before-parent ordering, bounded retry, and idempotent manual rerun/backfill semantics.
- Policy creation does not publish; publishing, disabling, and high-risk policy/workflow changes use the same governance path as knowledge changes.
- Low-risk publishing requires one authorized confirmation; high-risk publishing requires a different upward reviewer.
- Browser API accepts an HttpOnly BFF session; existing service-to-service ticket verification remains supported and fail closed.
- Published knowledge/workflow versions and Dream runs are never mutated in place.

---

## File Map

- Add public models under `services/agentatlas/schemas/` and `sdk/go/` rather than exposing `internal/dream.Policy`.
- Add migration `000002_dream_hierarchy_and_governance.sql` and focused query files.
- Split Dream input resolution, orchestration, output persistence, and policy lifecycle into separate files.
- Add `internal/governance/` for changes/review/publish and `internal/browsersession/` for the Console BFF.
- Extend routers with list/detail/update/review/rerun APIs; preserve existing runtime read APIs during migration.

### Task 1: Define Public Dream and Governance Types

**Files:**
- Create: `services/agentatlas/schemas/dream/dream.schema.json`
- Create: `sdk/go/dream/model.go`
- Create: `sdk/go/governance/model.go`
- Modify: `services/agentatlas/api/openapi/atlas-agent.yaml`
- Test: `services/agentatlas/schemas/dream_schema_test.go`

**Interfaces:**
- Consumes: existing Workflow schema and approved Dream semantics.
- Produces: public `DreamPolicyDefinition`, `DreamRunView`, `DreamSummaryView`, `ChangeDraft`, `RiskAssessment`, and `ReviewRoute`.

- [ ] **Step 1: Write failing schema validation tests**

```go
valid := `{"org_unit_id":"dept-rd","timezone":"Asia/Shanghai","schedule":"0 22 * * *","input_sources":["work_brief","child_dream_summary"],"workflow":{"id":"wf-dream","version":3},"output_space_id":"space-rd","visibility_level":"managers","confirmation_mode":"high_risk_only"}`
if err := ValidateDreamPolicy([]byte(valid)); err != nil { t.Fatal(err) }
```

- [ ] **Step 2: Verify failure**

Run: `cd services/agentatlas && go test ./schemas -run Dream -count=1`

Expected: FAIL because the schema/helper is absent.

- [ ] **Step 3: Define exact public enums and fields**

```go
type DreamSource string
const (SourceWorkBrief DreamSource = "work_brief"; SourceChildSummary DreamSource = "child_dream_summary"; SourceProjectRecord DreamSource = "project_record"; SourceSOPUpdate DreamSource = "sop_update"; SourceAgentAnswer DreamSource = "agent_answer"; SourceExternalEvidence DreamSource = "external_evidence"; SourceCompletedTask DreamSource = "completed_task"; SourceRiskEvent DreamSource = "risk_event")
type WorkflowRef struct { ID string `json:"id"`; Version int32 `json:"version"` }
type StructuredSignal struct { ID, Title, Detail, Severity, EvidencePointerID string }
```

`DreamRunView` contains `run_id`, `status`, `org_unit_id`, `window_start`, `window_end`, `policy_version`, `workflow`, `parent_run_ids`, `input_count`, `coverage`, `missing_inputs`, `facts`, `themes`, `trends`, `risks`, `todos`, `display_summary`, `evidence_pointer_id`, and `rerun_of_run_id`.

- [ ] **Step 4: Generate and verify contracts**

Run: `make generate && go test ./schemas ./internal/app -run Contract -count=1`

Expected: PASS and generated OpenAPI code is stable on a second run.

- [ ] **Step 5: Commit**

```bash
git add services/agentatlas/schemas services/agentatlas/api sdk/go
git commit -m "feat: define public Dream and governance schemas"
```

### Task 2: Migrate Dream Lineage and Governance Persistence

**Files:**
- Create: `services/agentatlas/db/migrations/000002_dream_hierarchy_and_governance.sql`
- Modify: `services/agentatlas/db/queries/dream.sql`
- Create: `services/agentatlas/db/queries/governance.sql`
- Test: `services/agentatlas/tests/integration/dream_governance_schema_test.go`

**Interfaces:**
- Consumes: existing `dream_policies`, `dream_runs`, `dream_inputs`, `dream_summaries`, workflows, spaces, and timeline.
- Produces: immutable run snapshots/lineage, structured output, annotations, and governed changes.

- [ ] **Step 1: Write the failing integration test**

```go
_, err := pool.Exec(ctx, `insert into dream_run_lineage(run_id,parent_run_id,relation) values('parent','child','child_summary')`)
if err != nil { t.Fatal(err) }
_, err = pool.Exec(ctx, `update dream_runs set policy_version=2 where id='parent'`)
if err == nil { t.Fatal("immutable run accepted update") }
```

- [ ] **Step 2: Verify failure before migration**

Run: `go test ./tests/integration -run DreamGovernanceSchema -count=1`

Expected: FAIL because tables/columns/triggers do not exist.

- [ ] **Step 3: Add normalized fields and immutable history**

Add `org_unit_id`, `policy_version`, `workflow_id`, `workflow_version`, `timezone`, `input_snapshot jsonb`, `visibility_snapshot jsonb`, `model_route`, `model_version`, `attempt`, `rerun_of_run_id`, `coverage jsonb`, and `missing_inputs jsonb` to runs. Add `facts`, `themes`, `trends`, `todos`, and `risk_signals` JSONB to summaries. Add `dream_run_lineage`, `dream_run_annotations`, `change_drafts`, `change_versions`, `change_reviews`, and `publish_operations` with enterprise-scoped unique/idempotency constraints.

Use a trigger that rejects updates to immutable run identity/snapshot/version/window fields after insert; status/error/finished timestamps remain updateable.

- [ ] **Step 4: Add sqlc queries**

Provide `ListChildSpaces`, `ListCompletedChildDreamRuns`, `ListDreamRunsByOrg`, `GetDreamRunView`, `CreateDreamRunLineage`, `CreateDreamAnnotation`, `CreateChangeDraft`, `UpdateChangeDraftIfRevision`, `CreateChangeVersion`, `CreateChangeReview`, and `GetOrCreatePublishOperation`.

- [ ] **Step 5: Generate and test**

Run: `make generate && go test ./tests/integration -run DreamGovernanceSchema -count=1`

Expected: PASS, including cross-enterprise foreign-key/query isolation and immutable history checks.

- [ ] **Step 6: Commit**

```bash
git add services/agentatlas/db services/agentatlas/tests/integration/dream_governance_schema_test.go
git commit -m "feat: persist Dream lineage and governed changes"
```

### Task 3: Build Pluggable, Sanitizing Dream Input Resolvers

**Files:**
- Create: `services/agentatlas/internal/dream/input.go`
- Create: `services/agentatlas/internal/dream/input_brief.go`
- Create: `services/agentatlas/internal/dream/input_child_summary.go`
- Create: `services/agentatlas/internal/dream/input_timeline.go`
- Test: `services/agentatlas/internal/dream/input_test.go`

**Interfaces:**
- Consumes: run window, policy sources, organization/space graph, masking policy, lower-level successful runs.
- Produces: `[]ResolvedInput`, `Coverage`, and `[]MissingInput`; no resolver returns unmasked model text.

- [ ] **Step 1: Write child-summary-first tests**

```go
inputs, coverage, err := resolver.Resolve(ctx, Request{OrgUnitID:"company", Sources:[]dream.Source{dream.SourceChildSummary}})
if err != nil { t.Fatal(err) }
if got := sourceTypes(inputs); !reflect.DeepEqual(got, []string{"child_dream_summary","child_dream_summary"}) { t.Fatal(got) }
if store.RawBriefReads != 0 { t.Fatal("company rollup read employee briefs") }
if coverage.ExpectedChildren != 3 || coverage.CompletedChildren != 2 { t.Fatal(coverage) }
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/dream -run Input -count=1`

Expected: FAIL because current Runner directly reads space members/work briefs.

- [ ] **Step 3: Define resolver contract**

```go
type ResolvedInput struct { SourceType dream.Source; SourceID, OrgUnitID, EvidencePointerID, SanitizedText string; Visibility []string; ParentRunID string }
type Coverage struct { ExpectedChildren, CompletedChildren, InputCount int }
type InputResolver interface { Resolve(context.Context, ResolveRequest) ([]ResolvedInput, Coverage, []MissingInput, error) }
```

- [ ] **Step 4: Implement resolvers**

`work_brief` resolves direct authorized members for employee/project scopes. `child_dream_summary` resolves only successful display/retrieval layers from immediate child orgs for the same window. Timeline sources resolve project records, SOP updates, Agent answers, external evidence, completed tasks, and risks by `source_type`. Apply masking and visibility convergence before populating `SanitizedText`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/dream -run 'Input|Mask|Visibility' -count=1`

Expected: PASS for sanitization before consumption, missing child reporting, no raw parent reads, source deduplication, and visibility intersection.

- [ ] **Step 6: Commit**

```bash
git add services/agentatlas/internal/dream
git commit -m "feat: resolve sanitized hierarchical Dream inputs"
```

### Task 4: Make Published Dream Workflows the Only Execution Path

**Files:**
- Create: `services/agentatlas/internal/dream/orchestrator.go`
- Modify: `services/agentatlas/internal/dream/runner.go`
- Modify: `services/agentatlas/internal/workflow/executors.go`
- Modify: `services/agentatlas/internal/workflow/runtime.go`
- Test: `services/agentatlas/internal/dream/orchestrator_test.go`

**Interfaces:**
- Consumes: pinned `WorkflowRef`, resolved sanitized inputs, and `workflow.Runtime`.
- Produces: one workflow run ID and typed `DreamOutput`; no scheduler-to-synthesizer bypass.

- [ ] **Step 1: Write the failing workflow-binding test**

```go
out, err := orchestrator.Run(ctx, DreamExecution{Workflow: dream.WorkflowRef{ID:"wf-dream",Version:3}, Inputs:inputs})
if err != nil { t.Fatal(err) }
if runtime.StartedWorkflow != "wf-dream" || runtime.StartedVersion != 3 { t.Fatal(runtime) }
if synth.DirectCalls != 0 { t.Fatal("bypassed workflow runtime") }
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/dream -run Orchestrator -count=1`

Expected: FAIL because current Runner calls `Synthesizer.Aggregate` directly.

- [ ] **Step 3: Define typed Dream workflow input/output**

```go
type WorkflowInput struct { OrgUnitID string; WindowStart, WindowEnd time.Time; Inputs []ResolvedInput; Coverage Coverage; Missing []MissingInput }
type DreamOutput struct { Display, Retrieval, SealedDetail string; Facts, Themes, Trends, Risks, Todos []StructuredSignal; Source, ModelRoute, ModelVersion string }
```

The `dream.aggregate` executor consumes this input and returns `DreamOutput`. `trace.append` records run/workflow/policy/evidence lineage. A `human.confirm` node pauses through the existing workflow runtime.

- [ ] **Step 4: Replace the direct aggregation path**

`Runner.execute` loads the pinned workflow version from the Dream run, calls the orchestrator, validates typed output, then persists it. Remove direct `r.synth.Aggregate` from Runner; keep synthesizer only inside the `dream.aggregate` node executor.

- [ ] **Step 5: Run workflow and Dream tests**

Run: `go test ./internal/workflow ./internal/dream -run 'Dream|Runtime|Executor' -count=1`

Expected: PASS for pinned-version execution, pause/resume, invalid output rejection, and no direct bypass.

- [ ] **Step 6: Commit**

```bash
git add services/agentatlas/internal/dream services/agentatlas/internal/workflow
git commit -m "refactor: execute Dream through published workflows"
```

### Task 5: Schedule Child-Before-Parent Runs with Timezone and Retry Semantics

**Files:**
- Modify: `services/agentatlas/internal/dream/scheduler.go`
- Create: `services/agentatlas/internal/dream/hierarchy.go`
- Test: `services/agentatlas/internal/dream/scheduler_hierarchy_test.go`

**Interfaces:**
- Consumes: published policies, versioned org tree, enterprise timezone, completed child windows.
- Produces: deterministic pending runs only when dependencies are complete or explicitly recorded missing.

- [ ] **Step 1: Write ordering/timezone tests**

```go
dispatched, err := scheduler.Tick(ctx, "ent-1", time.Date(2026,7,10,14,0,0,0,time.UTC)) // 22:00 Asia/Shanghai
if err != nil { t.Fatal(err) }
if !reflect.DeepEqual(dispatchedIDs, []string{"run-team-a","run-team-b"}) { t.Fatal(dispatchedIDs) }
// Department waits until both child runs terminate or are marked missing.
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/dream -run SchedulerHierarchy -count=1`

Expected: FAIL because current scheduler uses UTC and scans policies without hierarchy ordering.

- [ ] **Step 3: Implement deterministic readiness**

Calculate schedule in `time.LoadLocation(policy.Timezone)`, hash `(policy_id, policy_version, window_end, rerun_sequence)` into run ID, sort due policies by org depth descending, and dispatch a parent only after every expected child has `succeeded`, terminal `failed` with policy-allowed partial mode, or an explicit missing-input record.

- [ ] **Step 4: Implement retry/rerun semantics**

Automatic attempts increment up to `max_attempts`; a manual rerun creates a new immutable run with `rerun_of_run_id` and its own idempotency key. Backfill takes explicit window start/end and cannot overlap an existing successful window unless `rerun_of_run_id` is supplied.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/dream -run 'Scheduler|Due|Hierarchy|Rerun' -count=1`

Expected: PASS for Shanghai DST-independent schedule, child ordering, duplicate ticks, retry cap, partial mode, and manual rerun lineage.

```bash
git add services/agentatlas/internal/dream
git commit -m "feat: schedule hierarchical Dream windows"
```

### Task 6: Persist Immutable Structured Outputs and Governed Evidence Drill-Down

**Files:**
- Modify: `services/agentatlas/internal/dream/runner.go`
- Modify: `services/agentatlas/internal/dream/summary.go`
- Create: `services/agentatlas/internal/app/dream_run_handler.go`
- Modify: `services/agentatlas/internal/app/agent_server.go`
- Test: `services/agentatlas/tests/integration/dream_hierarchy_test.go`

**Interfaces:**
- Consumes: `DreamOutput`, lineage snapshots, AgentNexus Step Grant client.
- Produces: list/detail/annotate/rerun/evidence APIs and immutable three-layer summaries.

- [ ] **Step 1: Write the integration test**

The test seeds two child summaries, runs a department workflow, verifies `parent_run_ids`, structured trends/risks/todos/coverage, object-store sealed detail, OpenSearch job, and timeline node. It then proves evidence detail returns 403 without `dream:evidence:read`, succeeds with the bound grant, and appends audit evidence.

- [ ] **Step 2: Verify failure**

Run: `go test ./tests/integration -run DreamHierarchy -count=1`

Expected: FAIL on missing lineage/structured output/routes.

- [ ] **Step 3: Persist all output fields atomically**

Use a database transaction for summary layers, lineage, evidence links, index jobs, and timeline node. If object storage, index enqueue, trace append, or mandatory audit fails, mark the run failed and do not expose a successful display summary.

- [ ] **Step 4: Add read and action routes**

```text
GET  /v1/dream/overview?org_unit_id=
GET  /v1/dream/runs?org_unit_id=&window=
GET  /v1/dream/runs/{id}
POST /v1/dream/runs/{id}/annotations
POST /v1/dream/runs/{id}/reruns
POST /v1/dream/runs/{id}/evidence-access
```

Annotation actions are `confirm`, `reject`, `mark_incorrect`, and `comment`; they never update the stored summary.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/dream ./internal/app ./tests/integration -run Dream -count=1`

Expected: PASS for atomicity, immutability, grant binding, audit failure, annotations, and rerun lineage.

```bash
git add services/agentatlas/internal/dream services/agentatlas/internal/app services/agentatlas/tests/integration
git commit -m "feat: expose immutable hierarchical Dream runs"
```

### Task 7: Separate Policy Draft, Review, Publish, Disable, and Backfill

**Files:**
- Modify: `services/agentatlas/internal/dream/policy.go`
- Modify: `services/agentatlas/internal/app/dream_handler.go`
- Test: `services/agentatlas/internal/app/dream_policy_lifecycle_test.go`

**Interfaces:**
- Consumes: public Dream policy schema, workflow version verifier, governance client.
- Produces: explicit policy lifecycle routes; `POST /dream-policies` never publishes.

- [ ] **Step 1: Write lifecycle tests**

```go
created := post("/v1/dream-policies", policy)
assert.Equal(t, "draft", created.Status)
assert.Zero(t, created.Version)
review := post("/v1/dream-policies/"+created.ID+"/review", nil)
assert.Equal(t, "high", review.RiskLevel)
assert.NotEqual(t, requester, review.ReviewerUserID)
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/app -run DreamPolicyLifecycle -count=1`

Expected: FAIL because create currently immediately calls `Publish`.

- [ ] **Step 3: Implement explicit lifecycle**

Add update with optimistic `revision`, check, submit-for-review, approve/reject, publish, disable, manual rerun, and backfill. Publishing validates that the pinned Dream workflow exists and is published. High-risk policy fields include organization scope, visibility, masking, raw evidence access, workflow binding, and confirmation mode.

- [ ] **Step 4: Expose routes and audit every transition**

```text
POST /v1/dream-policies
PUT  /v1/dream-policies/{id}
POST /v1/dream-policies/{id}/review
POST /v1/dream-policies/{id}/decisions
POST /v1/dream-policies/{id}/publish
POST /v1/dream-policies/{id}/disable
```

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/dream ./internal/app -run 'Policy|Lifecycle' -count=1`

Expected: PASS; direct publish without required decision is denied and audit failure prevents state transition.

```bash
git add services/agentatlas/internal/dream services/agentatlas/internal/app
git commit -m "feat: govern Dream policy lifecycle"
```

### Task 8: Add Browser BFF Sessions and Unified Knowledge/Workflow Governance

**Files:**
- Create: `services/agentatlas/internal/browsersession/service.go`
- Create: `services/agentatlas/internal/app/browser_session_handler.go`
- Create: `services/agentatlas/internal/governance/service.go`
- Create: `services/agentatlas/internal/app/change_handler.go`
- Modify: `services/agentatlas/internal/app/agent_server.go`
- Modify: `services/agentatlas/internal/app/workflow_handler.go`
- Test: `services/agentatlas/tests/e2e/browser_governance_test.go`

**Interfaces:**
- Consumes: AgentNexus OIDC/token, authorization, approval, ticket/grant, and audit APIs.
- Produces: HttpOnly Atlas session, suggestion/draft/diff/review/idempotent publish APIs, and governed workflow publishing.

- [ ] **Step 1: Write browser return and governance E2E tests**

The test opens `/auth/login?return_to=/changes/chg-1`, completes PKCE against fake AgentNexus, verifies `atlas_session` is HttpOnly/Secure, creates a suggestion as a non-editor, updates a draft with revision 1, gets a deterministic high-risk route, rejects requester self-review, approves as the upward reviewer, publishes twice with the same idempotency key, and observes one version/audit event.

- [ ] **Step 2: Verify failure**

Run: `go test ./tests/e2e -run BrowserGovernance -count=1`

Expected: FAIL because Atlas browser sessions/change services are absent.

- [ ] **Step 3: Implement BFF session flow**

`GET /auth/login` creates PKCE/state/nonce server-side and redirects to AgentNexus. `/auth/callback` exchanges the one-time code on the server, verifies the returned ID token against AgentNexus discovery/JWKS (`iss`, `aud`, signature, expiry, nonce, enterprise/user claims), stores only opaque `atlas_session` in an HttpOnly/Secure/SameSite=Lax cookie, validates `return_to` as same-origin relative path, and redirects. `GET /api/session` returns identity, organization scopes, permissions, and display name. `POST /auth/logout` revokes both Atlas and AgentNexus sessions.

- [ ] **Step 4: Implement governed changes**

```go
type ChangeService interface {
  Suggest(context.Context, Actor, SuggestionInput) (ChangeDraft, error)
  UpdateDraft(context.Context, Actor, string, int64, json.RawMessage) (ChangeDraft, error)
  Assess(context.Context, Actor, string) (RiskAssessment, error)
  Submit(context.Context, Actor, string) (ReviewRoute, error)
  Decide(context.Context, Actor, string, DecisionInput) error
  Publish(context.Context, Actor, string, string) (PublishedVersion, error)
}
```

Optimistic conflicts return 409 with current revision and diff data. Published versions are immutable. Publish writes audit successfully before the public active pointer moves.

- [ ] **Step 5: Route workflow publishing through governance**

Keep create/update draft APIs, add list/get/diff, and replace direct `POST /workflows/{id}/publish` behavior with creation/consumption of a governed change. Existing service clients may use the compatibility route only while it enforces the same assessment/review/idempotency service.

- [ ] **Step 6: Run complete backend verification**

Run: `make generate && go test ./... && go build ./...`

Expected: all unit/integration/E2E tests PASS; existing ticket-guarded service calls remain valid; browser calls no longer require a pasted ticket.

- [ ] **Step 7: Commit**

```bash
git add services/agentatlas/internal/browsersession services/agentatlas/internal/governance services/agentatlas/internal/app services/agentatlas/tests/e2e services/agentatlas/api services/agentatlas/db
git commit -m "feat: govern Atlas browser maintenance operations"
```
