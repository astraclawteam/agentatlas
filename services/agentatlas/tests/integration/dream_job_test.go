// Dream job integration test: real PostgreSQL + real MinIO from compose.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres minio minio-init
//	ATLAS_TEST_POSTGRES_DSN=... ATLAS_TEST_OBJECT_ENDPOINT=http://localhost:9000 \
//	  go test ./tests/integration -run TestDreamJob
package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

type failingPublishBus struct{}

func (failingPublishBus) Publish(context.Context, string, string) error {
	return fmt.Errorf("injected publish failure")
}

type countingPublishBus struct {
	publishes int
	ids       []string
}

func (b *countingPublishBus) Publish(_ context.Context, _, id string) error {
	b.publishes++
	b.ids = append(b.ids, id)
	return nil
}

func (b *countingPublishBus) published(id string) bool {
	for _, candidate := range b.ids {
		if candidate == id {
			return true
		}
	}
	return false
}

type failingLifecycleStore struct{ *db.Queries }

func (s failingLifecycleStore) PublishDreamWorkflowWait(context.Context, db.PublishDreamWorkflowWaitParams) (db.DreamRun, error) {
	return db.DreamRun{}, fmt.Errorf("injected lifecycle store failure")
}

type failingLifecycleCompletionStore struct{ *db.Queries }

func (s failingLifecycleCompletionStore) CompleteDreamWorkflowLifecycle(context.Context, int64) error {
	return fmt.Errorf("injected lifecycle completion failure")
}
func (b *countingPublishBus) Subscribe(context.Context, string, func(context.Context, string)) (func(), error) {
	return func() {}, nil
}
func (failingPublishBus) Subscribe(context.Context, string, func(context.Context, string)) (func(), error) {
	return func() {}, nil
}

func TestDreamDirectSuccessLeaseRecovery(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN")
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
	entID, policyID, runID := newID("direct_ent"), newID("direct_policy"), newID("direct_dream")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: entID, Name: "Direct lease"}); err != nil {
		t.Fatal(err)
	}
	wfSvc, err := workflow.NewService(q)
	if err != nil {
		t.Fatal(err)
	}
	wfID, err := wfSvc.CreateDraft(ctx, entID, "Direct Dream", "admin", workflow.Definition{
		WorkflowID: newID("direct_wf"), Kind: sdkworkflow.KindDream,
		Nodes:     []sdkworkflow.Node{{ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}, {ID: "trace", Type: sdkworkflow.NodeTraceAppend}},
		Edges:     []sdkworkflow.Edge{{From: "aggregate", To: "trace"}},
		RiskLevel: sdkworkflow.RiskLow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wfSvc.Publish(ctx, entID, wfID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into dream_policies(id,enterprise_id,org_scope,status,draft) values($1,$2,'department:direct','published','{}')`, policyID, entID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into dream_policy_versions(policy_id,version,definition) values($1,1,'{}')`, policyID); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Hour)
	if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{
		ID: runID, PolicyID: policyID, Version: 1, EnterpriseID: entID, Status: "pending",
		WindowStart: pgtype.Timestamptz{Time: start, Valid: true}, WindowEnd: pgtype.Timestamptz{Time: start.Add(time.Hour), Valid: true},
		OrgUnitID: "department:direct", PolicyVersion: 1, WorkflowID: wfID, WorkflowVersion: 1, Timezone: "UTC",
		InputSnapshot: []byte(`{"source_counts":[],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"members","org_unit_ids":["department:direct"],"masked_field_count":0}`),
		ModelRoute: "workflow/" + wfID, ModelVersion: "v1", Attempt: 1, Coverage: []byte(`{"expected_children":0,"completed_children":0,"input_count":0}`), MissingInputs: []byte(`[]`), IdempotencyKey: runID,
	}); err != nil {
		t.Fatal(err)
	}
	owner := pgtype.Text{String: "direct-active-owner", Valid: true}
	if rows, err := q.ClaimDreamRunLease(ctx, db.ClaimDreamRunLeaseParams{ID: runID, ExecutionOwner: owner}); err != nil || rows != 1 {
		t.Fatalf("claim direct Dream: rows=%d err=%v", rows, err)
	}
	bus := &countingPublishBus{}
	taskRunner := tasks.NewRunner(bus)
	taskRunner.AllowEnqueue(dream.JobTypeDream)
	synth := dream.NewSynthesizer(nil)
	wfRuntime := workflow.NewRuntime(q, wfSvc, workflow.NewRegistryWithServices(workflow.Executors{Dream: synth.AggregateWorkflowInput, Traces: trace.NewService(q)}))
	consumer := dream.NewRunner(q, nil, dream.NewPolicyService(q), taskRunner, dream.NewOrchestrator(wfRuntime))
	wfRuntime.SetDreamLifecycleHook(consumer.WorkflowLifecycle)
	result, err := dream.NewOrchestrator(wfRuntime).Run(ctx, dream.DreamExecution{
		EnterpriseID: entID, DreamRunID: runID, PolicyID: policyID, PolicyVersion: 1,
		ExecutionOwner: "direct-active-owner",
		Workflow:       sdkdream.WorkflowRef{ID: wfID, Version: 1},
		Input: dream.WorkflowInput{OrgUnitID: "department:direct", WindowStart: start, WindowEnd: start.Add(time.Hour),
			Inputs: []dream.ResolvedInput{}, Missing: []dream.MissingInput{}, RiskSignalRules: []string{}},
	})
	if err != nil || result.Status != workflow.RunSucceeded {
		t.Fatalf("direct workflow result=%+v err=%v", result, err)
	}
	var processed bool
	if err := pool.QueryRow(ctx, `select processed_at is not null from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='succeeded'`, result.WorkflowRunID).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if processed || bus.publishes != 0 {
		t.Fatalf("active direct success consumed replay: processed=%v publishes=%d", processed, bus.publishes)
	}
	if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: runID, Status: "succeeded", Error: ""}); err != nil {
		t.Fatal(err)
	}
	if err := consumer.ReconcileWorkflowLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select processed_at is not null from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='succeeded'`, result.WorkflowRunID).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if !processed || bus.publishes != 0 {
		t.Fatalf("normal direct completion raced: processed=%v publishes=%d", processed, bus.publishes)
	}
	if _, err := pool.Exec(ctx, `update dream_runs set status='running',execution_owner='crashed-owner',execution_lease_expires_at=now()-interval '1 second' where id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dream_workflow_lifecycle_outbox set processed_at=null where workflow_run_id=$1 and status='succeeded'`, result.WorkflowRunID); err != nil {
		t.Fatal(err)
	}
	restartBus := &countingPublishBus{}
	restartTasks := tasks.NewRunner(restartBus)
	restartTasks.AllowEnqueue(dream.JobTypeDream)
	if err := dream.NewRunner(q, nil, dream.NewPolicyService(q), restartTasks, dream.NewOrchestrator(wfRuntime)).ReconcileWorkflowLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := q.GetDreamRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select processed_at is not null from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='succeeded'`, result.WorkflowRunID).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "pending" || !processed || restartBus.publishes != 1 {
		t.Fatalf("direct restart recovery: run=%+v processed=%v publishes=%d", recovered, processed, restartBus.publishes)
	}
	prebindID := newID("expired_prebind")
	if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{
		ID: prebindID, PolicyID: policyID, Version: 1, EnterpriseID: entID, Status: "pending",
		WindowStart: pgtype.Timestamptz{Time: start.Add(time.Hour), Valid: true}, WindowEnd: pgtype.Timestamptz{Time: start.Add(2 * time.Hour), Valid: true},
		OrgUnitID: "department:direct", PolicyVersion: 1, WorkflowID: wfID, WorkflowVersion: 1, Timezone: "UTC",
		InputSnapshot: []byte(`{"source_counts":[],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"members","org_unit_ids":["department:direct"],"masked_field_count":0}`),
		ModelRoute: "workflow/" + wfID, ModelVersion: "v1", Attempt: 1, Coverage: []byte(`{"expected_children":0,"completed_children":0,"input_count":0}`), MissingInputs: []byte(`[]`), IdempotencyKey: prebindID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ClaimDreamRunLease(ctx, db.ClaimDreamRunLeaseParams{ID: prebindID, ExecutionOwner: pgtype.Text{String: "dead-prebind", Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dream_runs set execution_lease_expires_at=now()-interval '1 second' where id=$1`, prebindID); err != nil {
		t.Fatal(err)
	}
	failingPrebindTasks := tasks.NewRunner(failingPublishBus{})
	failingPrebindTasks.AllowEnqueue(dream.JobTypeDream)
	if err := dream.NewRunner(q, nil, dream.NewPolicyService(q), failingPrebindTasks, nil).RecoverExpiredExecutions(ctx); err == nil {
		t.Fatal("expired pre-binding Dream publish failure was discarded")
	}
	prebindBus := &countingPublishBus{}
	prebindTasks := tasks.NewRunner(prebindBus)
	prebindTasks.AllowEnqueue(dream.JobTypeDream)
	if err := dream.NewRunner(q, nil, dream.NewPolicyService(q), prebindTasks, nil).RecoverExpiredExecutions(ctx); err != nil {
		t.Fatal(err)
	}
	prebind, err := q.GetDreamRun(ctx, prebindID)
	if err != nil || prebind.Status != "pending" || !prebindBus.published(prebindID) {
		t.Fatalf("expired pre-binding Dream not recovered: run=%+v publishes=%d err=%v", prebind, prebindBus.publishes, err)
	}
	ownerB := pgtype.Text{String: "prebind-owner-b", Valid: true}
	if _, err := q.ClaimDreamRunLease(ctx, db.ClaimDreamRunLeaseParams{ID: prebindID, ExecutionOwner: ownerB}); err != nil {
		t.Fatal(err)
	}
	staleWorkflowID := newID("stale_bind")
	staleInput := []byte(`{"org_unit_id":"department:direct","window_start":"2026-07-12T00:00:00Z","window_end":"2026-07-12T01:00:00Z","inputs":[{"source_type":"work_brief","source_id":"stale-input","org_unit_id":"department:direct","evidence_pointer_id":"","sanitized_text":"safe","visibility":["members"],"parent_run_id":""}],"coverage":{"expected_children":0,"completed_children":0,"input_count":1},"missing":[],"risk_signal_rules":[]}`)
	if _, err := q.CreateBoundDreamWorkflowRun(ctx, db.CreateBoundDreamWorkflowRunParams{
		DreamRunID: prebindID, EnterpriseID: entID, PolicyID: policyID, PolicyVersion: 1,
		OrgUnitID: "department:direct", WorkflowID: pgtype.Text{String: wfID, Valid: true}, Version: pgtype.Int4{Int32: 1, Valid: true},
		ID: staleWorkflowID, Status: workflow.RunRunning, Input: staleInput, Output: []byte(`{"next_index":0,"outputs":{},"input":{}}`),
		ExecutionOwner: pgtype.Text{String: "workflow-owner-a", Valid: true}, DreamExecutionOwner: pgtype.Text{String: "dead-prebind", Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale pre-binding owner created workflow/input: %v", err)
	}
	var staleInputs, staleWorkflows int
	if err := pool.QueryRow(ctx, `select count(*) from dream_inputs where run_id=$1`, prebindID).Scan(&staleInputs); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from workflow_runs where id=$1`, staleWorkflowID).Scan(&staleWorkflows); err != nil {
		t.Fatal(err)
	}
	if staleInputs != 0 || staleWorkflows != 0 {
		t.Fatalf("stale pre-binding effects: inputs=%d workflows=%d", staleInputs, staleWorkflows)
	}
	pendingWorkflowID := newID("expired_workflow")
	if _, err := q.CreateWorkflowRun(ctx, db.CreateWorkflowRunParams{ID: pendingWorkflowID, WorkflowID: wfID, Version: 1, EnterpriseID: entID, Status: workflow.RunPending, Input: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ClaimWorkflowRunLease(ctx, db.ClaimWorkflowRunLeaseParams{ID: pendingWorkflowID, ExecutionOwner: pgtype.Text{String: "dead-workflow", Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update workflow_runs set execution_lease_expires_at=now()-interval '1 second' where id=$1`, pendingWorkflowID); err != nil {
		t.Fatal(err)
	}
	failingWorkflowTasks := tasks.NewRunner(failingPublishBus{})
	failingWorkflowTasks.AllowEnqueue(workflow.JobTypeRun)
	failingHandler := workflow.NewRunJobHandler(wfRuntime, q, failingWorkflowTasks)
	if err := failingHandler.RecoverPendingRuns(ctx); err == nil {
		t.Fatal("workflow recovery publish failure was discarded")
	}
	restartWorkflowBus := &countingPublishBus{}
	restartWorkflowTasks := tasks.NewRunner(restartWorkflowBus)
	restartWorkflowTasks.AllowEnqueue(workflow.JobTypeRun)
	if err := workflow.NewRunJobHandler(wfRuntime, q, restartWorkflowTasks).RecoverPendingRuns(ctx); err != nil {
		t.Fatal(err)
	}
	recoveredWorkflow, err := q.GetWorkflowRun(ctx, pendingWorkflowID)
	if err != nil || recoveredWorkflow.Status != workflow.RunPending || !restartWorkflowBus.published(pendingWorkflowID) {
		t.Fatalf("expired/pending workflow recovery: run=%+v publishes=%d err=%v", recoveredWorkflow, restartWorkflowBus.publishes, err)
	}
}

func TestDreamJob(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	objEndpoint := os.Getenv("ATLAS_TEST_OBJECT_ENDPOINT")
	if dsn == "" || objEndpoint == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN and ATLAS_TEST_OBJECT_ENDPOINT")
	}
	ctx := context.Background()

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	q := db.New(pool)

	objects, err := storage.NewObjectStore(config.ObjectStorage{
		Endpoint: objEndpoint, Bucket: "agentatlas",
		AccessKey: envOr("ATLAS_TEST_OBJECT_ACCESS_KEY", "atlas"),
		SecretKey: envOr("ATLAS_TEST_OBJECT_SECRET_KEY", "atlas-secret-key"),
	})
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	// org fixture: enterprise + project group space + 2 members + briefs
	entID := newID("ent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: entID, Name: "测试企业"}); err != nil {
		t.Fatal(err)
	}
	spaceID := newID("spc")
	if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
		ID: spaceID, EnterpriseID: entID, Kind: "project_group",
		Name: "MES 异常工单专项组", OrgScope: "project_group:pg1", OrgVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"u_zhang", "u_li"} {
		if err := q.UpsertSpaceMember(ctx, db.UpsertSpaceMemberParams{
			SpaceID: spaceID, UserID: u, DisplayName: u, OrgVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	windowEnd := time.Date(2026, 7, 6, 22, 0, 0, 0, time.UTC)
	for i, item := range []struct{ user, summary string }{
		{"u_zhang", "完成分拣规则联调，风险:接口限流未定，电话13800138000。"},
		{"u_li", "整理 MES 工单模板。"},
	} {
		epID := newID("ev")
		if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
			ID: epID, EnterpriseID: entID, ResourceType: "work_brief",
			ResourceRef: "fs://briefs/" + item.user, SourceSystem: "filesystem",
			RequiredScopes: []string{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateWorkBrief(ctx, db.CreateWorkBriefParams{
			ID: newID("wb"), EnterpriseID: entID, EmployeeUserID: item.user,
			BriefDate: pgtype.Date{Time: windowEnd.Add(-6 * time.Hour), Valid: true},
			Summary:   item.summary, SourceHash: newID("sha"), EvidencePointerID: epID,
			Topics: []string{}, ProjectRefs: []string{},
		}); err != nil {
			t.Fatalf("brief %d: %v", i, err)
		}
	}
	wfSvc, err := workflow.NewService(q)
	if err != nil {
		t.Fatal(err)
	}
	dreamWorkflowID := newID("wf_dream")
	_, err = wfSvc.CreateDraft(ctx, entID, "Published Dream", "integration", workflow.Definition{
		WorkflowID: dreamWorkflowID, Kind: sdkworkflow.KindDream, RiskLevel: sdkworkflow.RiskLow,
		Nodes: []sdkworkflow.Node{{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm}, {ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}, {ID: "trace", Type: sdkworkflow.NodeTraceAppend}}, Edges: []sdkworkflow.Edge{{From: "confirm", To: "aggregate"}, {From: "aggregate", To: "trace"}},
	})
	if err != nil {
		t.Fatalf("Dream workflow: %v", err)
	}
	if _, err := wfSvc.Publish(ctx, entID, dreamWorkflowID, "integration"); err != nil {
		t.Fatalf("Dream workflow version: %v", err)
	}

	// policy: draft -> publish
	policySvc := dream.NewPolicyService(q)
	policyID, err := policySvc.CreateDraft(ctx, entID, dream.Policy(sdkdream.DreamPolicyDefinition{
		OrgUnitID: "project_group:pg1", Timezone: "UTC", Schedule: "0 22 * * *",
		InputSources:      []sdkdream.Source{sdkdream.SourceWorkBrief},
		Workflow:          sdkdream.WorkflowRef{ID: dreamWorkflowID, Version: 1},
		VisibilityLevel:   sdkdream.VisibilityMembers,
		MaskingRules:      []string{`1[3-9]\d{9}`},
		RiskSignalRules:   []string{`风险[:：]\S+`},
		EvidenceRetention: sdkdream.EvidencePointerPlusDisplaySummary,
		ConfirmationMode:  sdkdream.ConfirmationHighRiskOnly,
		OutputSpaceID:     spaceID,
	}))
	if err != nil {
		t.Fatalf("policy draft: %v", err)
	}
	if _, err := policySvc.Publish(ctx, policyID); err != nil {
		t.Fatalf("policy publish: %v", err)
	}

	// scheduler tick dispatches; MemBus executes synchronously
	runner := tasks.NewRunner(tasks.NewMemBus())
	synth := dream.NewSynthesizer(nil)
	wfRuntime := workflow.NewRuntime(q, wfSvc, workflow.NewRegistryWithServices(workflow.Executors{Dream: synth.AggregateWorkflowInput, Traces: trace.NewService(q)}))
	dreamRunner := dream.NewRunner(q, objects, policySvc, runner, dream.NewOrchestrator(wfRuntime))
	wfRuntime.SetDreamLifecycleHook(dreamRunner.WorkflowLifecycle)
	if err := dreamRunner.RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := workflow.NewRunJobHandler(wfRuntime, q, runner).RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler := dream.NewScheduler(q, policySvc, runner)
	now := windowEnd.Add(30 * time.Minute)
	n, err := scheduler.Tick(ctx, entID, now)
	if err != nil || n != 1 {
		t.Fatalf("tick dispatched %d err=%v", n, err)
	}
	dreamRun, err := q.GetLatestDreamRunForPolicy(ctx, policyID)
	if err != nil {
		t.Fatal(err)
	}
	if dreamRun.Status != "waiting_confirmation" || !dreamRun.WorkflowRunID.Valid {
		t.Fatalf("paused Dream=%+v", dreamRun)
	}
	// Re-open the durable wait record and simulate a process dying while its
	// Dream-side store transition is unavailable. A fresh runner recovers it.
	if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: dreamRun.ID, Status: "running", Error: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dream_workflow_lifecycle_outbox set processed_at=null,attempts=0,last_error='' where workflow_run_id=$1 and status='waiting_confirmation'`, dreamRun.WorkflowRunID); err != nil {
		t.Fatal(err)
	}
	storeFailureRunner := dream.NewRunner(failingLifecycleStore{Queries: q}, objects, policySvc, runner, dream.NewOrchestrator(wfRuntime))
	if err := storeFailureRunner.ReconcileWorkflowLifecycle(ctx); err == nil {
		t.Fatal("injected lifecycle store failure was discarded")
	}
	if err := dream.NewRunner(q, objects, policySvc, runner, dream.NewOrchestrator(wfRuntime)).ReconcileWorkflowLifecycle(ctx); err != nil {
		t.Fatalf("restart wait recovery: %v", err)
	}
	dreamRun, err = q.GetDreamRun(ctx, dreamRun.ID)
	if err != nil || dreamRun.Status != "waiting_confirmation" {
		t.Fatalf("wait recovery stranded Dream=%+v err=%v", dreamRun, err)
	}
	if _, err := pool.Exec(ctx, `update dream_workflow_lifecycle_outbox set processed_at=null,attempts=0,last_error='' where workflow_run_id=$1 and status='waiting_confirmation'`, dreamRun.WorkflowRunID); err != nil {
		t.Fatal(err)
	}
	completionFailureRunner := dream.NewRunner(failingLifecycleCompletionStore{Queries: q}, objects, policySvc, runner, dream.NewOrchestrator(wfRuntime))
	if err := completionFailureRunner.ReconcileWorkflowLifecycle(ctx); err == nil {
		t.Fatal("injected lifecycle completion failure was discarded")
	}
	var completionAttempts int32
	if err := pool.QueryRow(ctx, `select attempts from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='waiting_confirmation'`, dreamRun.WorkflowRunID).Scan(&completionAttempts); err != nil {
		t.Fatal(err)
	}
	if completionAttempts != 1 {
		t.Fatalf("completion failure attempts=%d, want 1", completionAttempts)
	}
	if err := dream.NewRunner(q, objects, policySvc, runner, dream.NewOrchestrator(wfRuntime)).ReconcileWorkflowLifecycle(ctx); err != nil {
		t.Fatalf("restart completion recovery: %v", err)
	}
	if sums, _ := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{SpaceID: spaceID, Layer: "display", Limit: 5}); len(sums) != 0 {
		t.Fatal("paused Dream exposed output")
	}
	// A source arriving while paused must not enter the bound workflow input.
	latePointer := newID("ev")
	if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{ID: latePointer, EnterpriseID: entID, ResourceType: "work_brief", ResourceRef: "fs://briefs/late", SourceSystem: "filesystem", RequiredScopes: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateWorkBrief(ctx, db.CreateWorkBriefParams{ID: newID("wb"), EnterpriseID: entID, EmployeeUserID: "u_zhang", BriefDate: pgtype.Date{Time: windowEnd.Add(-time.Hour), Valid: true}, Summary: "late mutable source", SourceHash: newID("sha"), EvidencePointerID: latePointer, Topics: []string{}, ProjectRefs: []string{}}); err != nil {
		t.Fatal(err)
	}
	status, err := wfRuntime.Resume(ctx, dreamRun.WorkflowRunID.String, entID, true, "approved")
	if err != nil || status != workflow.RunPending {
		t.Fatalf("resume=%s err=%v", status, err)
	}
	_, err = pool.Exec(ctx, `CREATE OR REPLACE FUNCTION task4_fail_retrieval() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.layer='retrieval' THEN RAISE EXCEPTION 'task4 injected mid-transaction failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER task4_fail_retrieval BEFORE INSERT ON dream_summaries FOR EACH ROW EXECUTE FUNCTION task4_fail_retrieval()`)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Enqueue(ctx, workflow.JobTypeRun, dreamRun.WorkflowRunID.String); err != nil {
		t.Fatal(err)
	}
	dreamRun, err = q.GetDreamRun(ctx, dreamRun.ID)
	if err != nil || dreamRun.Status != "failed" {
		t.Fatalf("injected Dream=%+v err=%v", dreamRun, err)
	}
	sealedKey := fmt.Sprintf("dreams/%s/%s.md", entID, dreamRun.ID)
	if _, err := objects.Get(ctx, sealedKey); err != nil {
		t.Fatalf("staged sealed object must remain retryable: %v", err)
	}
	for name, query := range map[string]string{
		"summaries": `select count(*) from dream_summaries where run_id=$1 and $2::text=$2::text and $3::text=$3::text`,
		"links":     `select count(*) from dream_evidence_pointers l join dream_summaries s on s.id=l.dream_summary_id where s.run_id=$1 and $2::text=$2::text and $3::text=$3::text`,
		"indexes":   `select count(*) from index_jobs where enterprise_id=$2 and source_type='dream_summary' and $1::text=$1::text and $3::text=$3::text`,
		"timeline":  `select count(*) from timeline_nodes where enterprise_id=$2 and source_type='dream_summary' and $1::text=$1::text and $3::text=$3::text`,
		"pointer":   `select count(*) from evidence_pointers where enterprise_id=$2 and resource_ref=$3 and $1::text=$1::text`,
	} {
		var count int
		if err := pool.QueryRow(ctx, query, dreamRun.ID, entID, sealedKey).Scan(&count); err != nil {
			t.Fatalf("%s count: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("%s visible after rollback: %d", name, count)
		}
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER task4_fail_retrieval ON dream_summaries; DROP FUNCTION task4_fail_retrieval()`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: dreamRun.ID, Status: "pending", Error: ""}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Enqueue(ctx, dream.JobTypeDream, dreamRun.ID); err != nil {
		t.Fatal(err)
	}
	dreamRun, err = q.GetDreamRun(ctx, dreamRun.ID)
	if err != nil || dreamRun.Status != "succeeded" {
		t.Fatalf("idempotent retry Dream=%+v err=%v", dreamRun, err)
	}
	var workflowRuns int
	if err := pool.QueryRow(ctx, `select count(*) from workflow_runs where id=$1`, dreamRun.WorkflowRunID).Scan(&workflowRuns); err != nil || workflowRuns != 1 {
		t.Fatalf("workflow run count=%d err=%v", workflowRuns, err)
	}
	if got, err := objects.Get(ctx, sealedKey); err != nil || len(got) == 0 {
		t.Fatalf("sealed retry object incoherent: %v", err)
	}
	if dreamRun.WorkflowRunID.String == "" {
		t.Fatal("workflow binding lost")
	}
	// idempotent double tick
	n2, err := scheduler.Tick(ctx, entID, now)
	if err != nil || n2 != 0 {
		t.Fatalf("double tick dispatched %d err=%v", n2, err)
	}

	// layered summaries
	display, err := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
		SpaceID: spaceID, Layer: "display", Limit: 5,
	})
	if err != nil || len(display) != 1 {
		t.Fatalf("display summaries: %v n=%d", err, len(display))
	}
	if !strings.Contains(display[0].SummaryText, "2 名成员") || !strings.Contains(display[0].SummaryText, "风险信号 1 项") {
		t.Fatalf("display = %s", display[0].SummaryText)
	}
	var boundInputs int
	if err := pool.QueryRow(ctx, `select count(*) from dream_inputs where run_id=$1`, dreamRun.ID).Scan(&boundInputs); err != nil || boundInputs != 2 {
		t.Fatalf("bound inputs changed during pause: %d err=%v", boundInputs, err)
	}
	retrieval, _ := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
		SpaceID: spaceID, Layer: "retrieval", Limit: 5,
	})
	if len(retrieval) != 1 || strings.Contains(retrieval[0].SummaryText, "13800138000") {
		t.Fatalf("retrieval layer: %+v", retrieval)
	}

	// sealed detail lives in object storage; DB keeps pointer only
	sealed, _ := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
		SpaceID: spaceID, Layer: "sealed_pointer", Limit: 5,
	})
	if len(sealed) != 1 || sealed[0].SummaryText != "" || sealed[0].SealedObjectKey == "" {
		t.Fatalf("sealed layer: %+v", sealed)
	}
	raw, err := objects.Get(ctx, sealed[0].SealedObjectKey)
	if err != nil {
		t.Fatalf("sealed object: %v", err)
	}
	// A conflicting full persisted workflow output is rejected by the reserved
	// hash before the referenced object can be overwritten.
	if _, err := pool.Exec(ctx, `update workflow_runs set output=jsonb_set(output,'{outputs,aggregate,retrieval}','"conflicting retrieval"'::jsonb) where id=$1`, dreamRun.WorkflowRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: dreamRun.ID, Status: "pending", Error: ""}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Enqueue(ctx, dream.JobTypeDream, dreamRun.ID); err != nil {
		t.Fatal(err)
	}
	conflicted, err := q.GetDreamRun(ctx, dreamRun.ID)
	if err != nil || conflicted.Status != "failed" || !strings.Contains(conflicted.Error, "conflict") {
		t.Fatalf("conflict run=%+v err=%v", conflicted, err)
	}
	afterConflict, err := objects.Get(ctx, sealed[0].SealedObjectKey)
	if err != nil || !bytes.Equal(raw, afterConflict) {
		t.Fatal("conflicting retry overwrote referenced sealed object")
	}
	// A redispatch publication failure remains durable in the lifecycle outbox.
	// A newly instantiated runner can recover it without rewriting workflow success.
	if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: dreamRun.ID, Status: "waiting_confirmation", Error: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dream_workflow_lifecycle_outbox set processed_at=null,attempts=0,last_error='' where workflow_run_id=$1 and status='succeeded'`, dreamRun.WorkflowRunID); err != nil {
		t.Fatal(err)
	}
	failingTasks := tasks.NewRunner(failingPublishBus{})
	failingTasks.AllowEnqueue(dream.JobTypeDream)
	failingRunner := dream.NewRunner(q, objects, policySvc, failingTasks, dream.NewOrchestrator(wfRuntime))
	if err := failingRunner.ReconcileWorkflowLifecycle(ctx); err == nil {
		t.Fatal("injected publication failure was discarded")
	}
	undispatched, err := q.GetDreamRun(ctx, dreamRun.ID)
	if err != nil || undispatched.Status != "pending" {
		t.Fatalf("undispatched Dream=%+v err=%v", undispatched, err)
	}
	var attempts int32
	var lastError, workflowStatus string
	var processed bool
	if err := pool.QueryRow(ctx, `select attempts,last_error,processed_at is not null from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='succeeded'`, dreamRun.WorkflowRunID).Scan(&attempts, &lastError, &processed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select status from workflow_runs where id=$1`, dreamRun.WorkflowRunID).Scan(&workflowStatus); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastError == "" || processed || workflowStatus != workflow.RunSucceeded {
		t.Fatalf("failed lifecycle was not durable: attempts=%d error=%q processed=%v workflow=%s", attempts, lastError, processed, workflowStatus)
	}
	recoveredBus := &countingPublishBus{}
	recoveredTasks := tasks.NewRunner(recoveredBus)
	recoveredTasks.AllowEnqueue(dream.JobTypeDream)
	recoveredRunner := dream.NewRunner(q, objects, policySvc, recoveredTasks, dream.NewOrchestrator(wfRuntime))
	if err := recoveredRunner.ReconcileWorkflowLifecycle(ctx); err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if recoveredBus.publishes != 1 {
		t.Fatalf("restart recovery publications=%d", recoveredBus.publishes)
	}
	if err := pool.QueryRow(ctx, `select processed_at is not null from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='succeeded'`, dreamRun.WorkflowRunID).Scan(&processed); err != nil || !processed {
		t.Fatalf("recovered event not completed: processed=%v err=%v", processed, err)
	}
	// A direct no-confirm workflow can succeed while its owning Dream Runner is
	// still persisting output. Its active lease keeps the success event pending;
	// after simulated process death/expiry, a fresh Runner recovers the Dream.
	if _, err := pool.Exec(ctx, `update dream_runs set status='running',execution_owner='owner-active',execution_lease_expires_at=now()+interval '1 hour' where id=$1`, dreamRun.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dream_workflow_lifecycle_outbox set processed_at=null,attempts=0,last_error='' where workflow_run_id=$1 and status='succeeded'`, dreamRun.WorkflowRunID); err != nil {
		t.Fatal(err)
	}
	activeBus := &countingPublishBus{}
	activeTasks := tasks.NewRunner(activeBus)
	activeTasks.AllowEnqueue(dream.JobTypeDream)
	if err := dream.NewRunner(q, objects, policySvc, activeTasks, dream.NewOrchestrator(wfRuntime)).ReconcileWorkflowLifecycle(ctx); err != nil {
		t.Fatalf("active lease reconciliation: %v", err)
	}
	if err := pool.QueryRow(ctx, `select processed_at is not null from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='succeeded'`, dreamRun.WorkflowRunID).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if processed || activeBus.publishes != 0 {
		t.Fatalf("active direct execution lost replay: processed=%v publishes=%d", processed, activeBus.publishes)
	}
	if _, err := pool.Exec(ctx, `update dream_runs set execution_lease_expires_at=now()-interval '1 second' where id=$1`, dreamRun.ID); err != nil {
		t.Fatal(err)
	}
	expiredBus := &countingPublishBus{}
	expiredTasks := tasks.NewRunner(expiredBus)
	expiredTasks.AllowEnqueue(dream.JobTypeDream)
	if err := dream.NewRunner(q, objects, policySvc, expiredTasks, dream.NewOrchestrator(wfRuntime)).ReconcileWorkflowLifecycle(ctx); err != nil {
		t.Fatalf("expired lease recovery: %v", err)
	}
	if err := pool.QueryRow(ctx, `select processed_at is not null from dream_workflow_lifecycle_outbox where workflow_run_id=$1 and status='succeeded'`, dreamRun.WorkflowRunID).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	recoveredLeaseRun, err := q.GetDreamRun(ctx, dreamRun.ID)
	if err != nil || !processed || expiredBus.publishes != 1 || recoveredLeaseRun.Status != "pending" {
		t.Fatalf("expired direct execution not recovered: run=%+v processed=%v publishes=%d err=%v", recoveredLeaseRun, processed, expiredBus.publishes, err)
	}
	// Rejection terminalizes the next bound Dream instead of stranding waiting.
	nextNow := now.Add(24 * time.Hour)
	if n, err := scheduler.Tick(ctx, entID, nextNow); err != nil || n != 1 {
		t.Fatalf("next tick n=%d err=%v", n, err)
	}
	rejected, err := q.GetLatestDreamRunForPolicy(ctx, policyID)
	if err != nil || rejected.Status != "waiting_confirmation" {
		t.Fatalf("rejected setup=%+v err=%v", rejected, err)
	}
	if status, err := wfRuntime.Resume(ctx, rejected.WorkflowRunID.String, entID, false, "reject"); err != nil || status != workflow.RunCancelled {
		t.Fatalf("reject status=%s err=%v", status, err)
	}
	rejected, err = q.GetDreamRun(ctx, rejected.ID)
	if err != nil || rejected.Status != "failed" {
		t.Fatalf("rejected Dream stranded: %+v err=%v", rejected, err)
	}
	if !strings.Contains(string(raw), "## ") || strings.Contains(string(raw), "13800138000") {
		t.Fatalf("sealed content wrong or unmasked: %s", raw)
	}

	// timeline node on the space
	nodes, err := q.ListTimelineNodes(ctx, db.ListTimelineNodesParams{SpaceID: spaceID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range nodes {
		if n.SourceType == "dream_summary" {
			found = true
		}
	}
	if !found {
		t.Fatal("dream timeline node missing")
	}
}
