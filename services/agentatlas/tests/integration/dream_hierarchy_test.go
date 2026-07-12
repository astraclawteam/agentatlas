package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
	"github.com/jackc/pgx/v5/pgtype"
)

type failingDreamTraceStore struct{ *db.Queries }

func (f failingDreamTraceStore) CreateAnswerTrace(context.Context, db.CreateAnswerTraceParams) (db.AnswerTrace, error) {
	return db.AnswerTrace{}, fmt.Errorf("injected mandatory trace failure")
}

func TestDreamHierarchy(t *testing.T) {
	dsn, endpoint := os.Getenv("ATLAS_TEST_POSTGRES_DSN"), os.Getenv("ATLAS_TEST_OBJECT_ENDPOINT")
	if dsn == "" || endpoint == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN and ATLAS_TEST_OBJECT_ENDPOINT")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)
	objects, err := storage.NewObjectStore(config.ObjectStorage{Endpoint: endpoint, Bucket: "agentatlas", AccessKey: envOr("ATLAS_TEST_OBJECT_ACCESS_KEY", "atlas"), SecretKey: envOr("ATLAS_TEST_OBJECT_SECRET_KEY", "atlas-secret-key")})
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}

	ent, parentSpace := newID("ent-hierarchy"), newID("space-parent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: ent, Name: "Hierarchy output"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{ID: parentSpace, EnterpriseID: ent, Kind: "department", Name: "Parent", OrgScope: "department:parent", OrgVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{EnterpriseID: ent, SpaceID: parentSpace, ScopeKind: "department", ScopeID: "parent"}); err != nil {
		t.Fatal(err)
	}

	wfSvc, err := workflow.NewService(q)
	if err != nil {
		t.Fatal(err)
	}
	wfID := newID("wf-hierarchy")
	if _, err := wfSvc.CreateDraft(ctx, ent, "Hierarchy Dream", "integration", workflow.Definition{WorkflowID: wfID, Kind: sdkworkflow.KindDream, RiskLevel: sdkworkflow.RiskLow, Nodes: []sdkworkflow.Node{{ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}, {ID: "trace", Type: sdkworkflow.NodeTraceAppend}}, Edges: []sdkworkflow.Edge{{From: "aggregate", To: "trace"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := wfSvc.Publish(ctx, ent, wfID, "integration"); err != nil {
		t.Fatal(err)
	}

	windowStart := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)
	childRuns := make([]string, 0, 2)
	for i, child := range []string{"a", "b"} {
		spaceID, org := newID("space-child"), "project_group:"+child
		if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{ID: spaceID, EnterpriseID: ent, Kind: "project_group", Name: "Child " + child, OrgScope: org, OrgVersion: 1}); err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{EnterpriseID: ent, SpaceID: spaceID, ScopeKind: "project_group", ScopeID: child, ParentScopeKind: pgtype.Text{String: "department", Valid: true}, ParentScopeID: pgtype.Text{String: "parent", Valid: true}}); err != nil {
			t.Fatal(err)
		}
		policyID, runID, pointerID := newID("policy-child"), newID("run-child"), newID("ev-child")
		if _, err := q.CreateDreamPolicy(ctx, db.CreateDreamPolicyParams{ID: policyID, EnterpriseID: ent, OrgScope: org, Status: "published", Draft: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.PublishDreamPolicyVersion(ctx, db.PublishDreamPolicyVersionParams{PolicyID: policyID, Version: 1, Definition: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{ID: runID, PolicyID: policyID, Version: 1, EnterpriseID: ent, Status: "running", WindowStart: ts(windowStart), WindowEnd: ts(windowEnd), OrgUnitID: org, PolicyVersion: 1, WorkflowID: wfID, WorkflowVersion: 1, Timezone: "UTC", InputSnapshot: []byte(`{"source_counts":[],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(fmt.Sprintf(`{"visibility_level":"managers","org_unit_ids":[%q,"department:parent"],"masked_field_count":0}`, org)), ModelRoute: "workflow/" + wfID, ModelVersion: "v1", Attempt: 1, Coverage: []byte(`{"expected_children":0,"completed_children":0,"input_count":0}`), MissingInputs: []byte(`[]`), IdempotencyKey: runID}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{ID: pointerID, EnterpriseID: ent, ResourceType: "dream_sealed_summary", ResourceRef: "seed://" + runID, SourceSystem: "integration", RequiredScopes: []string{"dream:evidence:read"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateDreamSummary(ctx, db.CreateDreamSummaryParams{ID: newID("sum-child"), RunID: runID, EnterpriseID: ent, SpaceID: spaceID, Layer: "retrieval", SummaryText: fmt.Sprintf("child %d sanitized summary", i+1), EvidencePointerID: pgtype.Text{String: pointerID, Valid: true}, RiskSignals: []byte(`[]`), Facts: []byte(`[]`), Themes: []byte(`[]`), Trends: []byte(`[]`), Todos: []byte(`[]`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: runID, Status: "succeeded", Error: ""}); err != nil {
			t.Fatal(err)
		}
		childRuns = append(childRuns, runID)
	}

	policySvc := dream.NewPolicyService(q)
	policyID, err := policySvc.CreateDraft(ctx, ent, dream.Policy(sdkdream.DreamPolicyDefinition{OrgUnitID: "department:parent", Timezone: "UTC", Schedule: "0 14 * * *", InputSources: []sdkdream.Source{sdkdream.SourceChildDreamSummary}, Workflow: sdkdream.WorkflowRef{ID: wfID, Version: 1}, OutputSpaceID: parentSpace, VisibilityLevel: sdkdream.VisibilityManagers, EvidenceRetention: sdkdream.EvidencePointerPlusDisplaySummary, ConfirmationMode: sdkdream.ConfirmationNever, MaxAttempts: 3}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policySvc.Publish(ctx, policyID); err != nil {
		t.Fatal(err)
	}
	parentRun := newID("run-parent")
	coverageRaw, _ := json.Marshal(sdkdream.Coverage{ExpectedChildren: 2, CompletedChildren: 2, InputCount: 2})
	if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{ID: parentRun, PolicyID: policyID, Version: 1, EnterpriseID: ent, Status: "pending", WindowStart: ts(windowStart), WindowEnd: ts(windowEnd), OrgUnitID: "department:parent", PolicyVersion: 1, WorkflowID: wfID, WorkflowVersion: 1, Timezone: "UTC", InputSnapshot: []byte(`{"source_counts":[{"source_type":"child_dream_summary","count":0}],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"managers","org_unit_ids":["department:parent"],"masked_field_count":0}`), ModelRoute: "workflow/" + wfID, ModelVersion: "v1", Attempt: 1, Coverage: coverageRaw, MissingInputs: []byte(`[]`), IdempotencyKey: parentRun, OrgVersion: 1}); err != nil {
		t.Fatal(err)
	}

	aggregate := func(_ context.Context, in workflow.DreamAggregateInput) (map[string]any, error) {
		if len(in.Inputs) != 2 {
			return nil, fmt.Errorf("inputs=%d", len(in.Inputs))
		}
		ev := in.Inputs[0].EvidencePointerID
		signal := map[string]any{"id": "signal-1", "title": "Structured", "detail": "sanitized", "severity": "warning", "evidence_pointer_id": ev}
		return map[string]any{"display": "parent display", "retrieval": "parent retrieval", "sealed_detail": "parent sealed detail", "facts": []any{signal}, "themes": []any{}, "trends": []any{signal}, "risks": []any{signal}, "todos": []any{signal}, "source": "integration"}, nil
	}
	taskRunner := tasks.NewRunner(tasks.NewMemBus())
	runtime := workflow.NewRuntime(q, wfSvc, workflow.NewRegistryWithServices(workflow.Executors{Dream: aggregate, Traces: trace.NewService(q)}))
	dreamRunner := dream.NewRunner(q, objects, policySvc, taskRunner, dream.NewOrchestrator(runtime))
	runtime.SetDreamLifecycleHook(dreamRunner.WorkflowLifecycle)
	if err := dreamRunner.RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := workflow.NewRunJobHandler(runtime, q, taskRunner).RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := taskRunner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := taskRunner.Enqueue(ctx, dream.JobTypeDream, parentRun); err != nil {
		t.Fatal(err)
	}

	run, err := q.GetDreamRun(ctx, parentRun)
	if err != nil || run.Status != "succeeded" {
		t.Fatalf("parent=%+v err=%v", run, err)
	}
	view, err := q.GetDreamRunView(ctx, db.GetDreamRunViewParams{EnterpriseID: ent, RunID: parentRun})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ParentRunIds) != 2 || view.ParentRunIds[0] != childRuns[0] && view.ParentRunIds[0] != childRuns[1] || view.DisplaySummary != "parent display" || !view.EvidencePointerID.Valid {
		t.Fatalf("view=%+v", view)
	}
	for name, raw := range map[string][]byte{"trends": view.Trends, "risks": view.Risks, "todos": view.Todos} {
		var signals []sdkdream.StructuredSignal
		if json.Unmarshal(raw, &signals) != nil || len(signals) != 1 {
			t.Fatalf("%s=%s", name, raw)
		}
	}
	sealedKey := fmt.Sprintf("dreams/%s/%s.md", ent, parentRun)
	if detail, err := objects.Get(ctx, sealedKey); err != nil || string(detail) != "parent sealed detail" {
		t.Fatalf("sealed=%q err=%v", detail, err)
	}
	for name, query := range map[string]string{"index": `select count(*) from index_jobs where enterprise_id=$1 and source_type='dream_summary'`, "timeline": `select count(*) from timeline_nodes where enterprise_id=$1 and source_type='dream_summary'`, "trace": `select count(*) from answer_traces where enterprise_id=$1 and workflow_run_id is not null`} {
		var count int
		if err := pool.QueryRow(ctx, query, ent).Scan(&count); err != nil || count < 1 {
			t.Fatalf("%s=%d err=%v", name, count, err)
		}
	}

	failedRun := newID("run-parent-trace-fail")
	if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{ID: failedRun, PolicyID: policyID, Version: 1, EnterpriseID: ent, Status: "pending", WindowStart: ts(windowStart), WindowEnd: ts(windowEnd), OrgUnitID: "department:parent", PolicyVersion: 1, WorkflowID: wfID, WorkflowVersion: 1, Timezone: "UTC", InputSnapshot: []byte(`{"source_counts":[{"source_type":"child_dream_summary","count":0}],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"managers","org_unit_ids":["department:parent"],"masked_field_count":0}`), ModelRoute: "workflow/" + wfID, ModelVersion: "v1", Attempt: 1, Coverage: coverageRaw, MissingInputs: []byte(`[]`), IdempotencyKey: failedRun, OrgVersion: 1}); err != nil {
		t.Fatal(err)
	}
	failingTasks := tasks.NewRunner(tasks.NewMemBus())
	failingRuntime := workflow.NewRuntime(q, wfSvc, workflow.NewRegistryWithServices(workflow.Executors{Dream: aggregate, Traces: trace.NewService(failingDreamTraceStore{Queries: q})}))
	failingRunner := dream.NewRunner(q, objects, policySvc, failingTasks, dream.NewOrchestrator(failingRuntime))
	failingRuntime.SetDreamLifecycleHook(failingRunner.WorkflowLifecycle)
	if err := failingRunner.RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := workflow.NewRunJobHandler(failingRuntime, q, failingTasks).RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := failingTasks.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := failingTasks.Enqueue(ctx, dream.JobTypeDream, failedRun); err != nil {
		t.Fatal(err)
	}
	failed, err := q.GetDreamRun(ctx, failedRun)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("trace failure run=%+v err=%v", failed, err)
	}
	var exposed int
	if err := pool.QueryRow(ctx, `select count(*) from dream_summaries where run_id=$1`, failedRun).Scan(&exposed); err != nil || exposed != 0 {
		t.Fatalf("trace failure exposed summaries=%d err=%v", exposed, err)
	}

	nx := nexusclient.NewMock()
	nx.Tickets["no-evidence"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: ent, ActorUserID: "manager", Scopes: []string{"dream:read"}}
	nx.Tickets["bound-evidence"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: ent, ActorUserID: "manager", Scopes: []string{"dream:evidence:read"}}
	orgURI := "agentatlas://dream/enterprises/" + url.PathEscape(ent) + "/org-units/" + url.PathEscape("department:parent")
	nx.Locations[orgURI] = nexus.LocateEvidenceResponse{ResourceURI: orgURI, SourceSystem: "agentatlas"}
	nx.Locations[view.EvidencePointerID.String] = nexus.LocateEvidenceResponse{ResourceURI: "s3://sealed/" + parentRun, SourceSystem: "object-storage"}
	nx.Reads["s3://sealed/"+parentRun] = nexus.ReadEvidenceResponse{GrantID: "grant-hierarchy", ContentType: "text/markdown", SanitizedExcerpt: "sanitized parent detail", ContentHash: "sha256:detail"}
	router := app.NewAgentRouter(app.AgentRouterDeps{Nexus: nx, Store: q})
	for ticket, want := range map[string]int{"no-evidence": http.StatusForbidden, "bound-evidence": http.StatusOK} {
		req := httptest.NewRequest(http.MethodPost, "/v1/dream/runs/"+parentRun+"/evidence-access", nil)
		req.Header.Set("X-Nexus-Ticket", ticket)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != want {
			t.Fatalf("evidence %s status=%d body=%s", ticket, resp.Code, resp.Body.String())
		}
	}
	if len(nx.AuditLog) != 1 || nx.AuditLog[0].Details["grant_id"] != "grant-hierarchy" || nx.AuditLog[0].ResourceID != parentRun {
		t.Fatalf("mandatory evidence audit=%+v", nx.AuditLog)
	}
}
