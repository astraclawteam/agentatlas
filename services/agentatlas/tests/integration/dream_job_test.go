// Dream job integration test: real PostgreSQL + real MinIO from compose.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres minio minio-init
//	ATLAS_TEST_POSTGRES_DSN=... ATLAS_TEST_OBJECT_ENDPOINT=http://localhost:9000 \
//	  go test ./tests/integration -run TestDreamJob
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

type failingPublishBus struct{}

func (failingPublishBus) Publish(context.Context, string, string) error {
	return fmt.Errorf("injected publish failure")
}

type countingPublishBus struct{ publishes int }

func (b *countingPublishBus) Publish(context.Context, string, string) error {
	b.publishes++
	return nil
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
		Nodes: []sdkworkflow.Node{{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm}, {ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}}, Edges: []sdkworkflow.Edge{{From: "confirm", To: "aggregate"}},
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
	wfRuntime := workflow.NewRuntime(q, wfSvc, workflow.NewRegistryWithServices(workflow.Executors{Dream: synth.AggregateWorkflowInput}))
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
