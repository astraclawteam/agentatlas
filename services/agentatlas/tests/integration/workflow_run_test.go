// Workflow-run integration test: a published workflow executes end to end
// through workflow.Runtime + the worker job handler with REAL executors
// (artifact parse via httptest docling, summarize, trace.append) against real
// PostgreSQL + MinIO; a second run pauses at human.confirm and resumes.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres minio minio-init
//	ATLAS_TEST_POSTGRES_DSN=... ATLAS_TEST_OBJECT_ENDPOINT=http://localhost:9000 \
//	  go test ./tests/integration -run TestWorkflowRun
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

func TestWorkflowRun(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	objEndpoint := os.Getenv("ATLAS_TEST_OBJECT_ENDPOINT")
	if dsn == "" || objEndpoint == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN and ATLAS_TEST_OBJECT_ENDPOINT (compose postgres+minio)")
	}
	ctx := context.Background()

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)

	objects, err := storage.NewObjectStore(config.ObjectStorage{
		Endpoint: objEndpoint, Bucket: "agentatlas",
		AccessKey: envOr("ATLAS_TEST_OBJECT_ACCESS_KEY", "atlas"),
		SecretKey: envOr("ATLAS_TEST_OBJECT_SECRET_KEY", "atlas-secret-key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}

	docling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "success",
			"document": map[string]any{"md_content": "# MES 异常工单排查\n\n第一步：确认工单编号。"},
		})
	}))
	defer docling.Close()
	registry, err := parsergateway.NewRegistry(parsergateway.NewDoclingProvider(docling.URL))
	if err != nil {
		t.Fatal(err)
	}

	runner := tasks.NewRunner(tasks.NewMemBus())
	artifactSvc := artifacts.NewService(q, objects, parsergateway.NewGateway(registry), runner, nil)
	traceSvc := trace.NewService(q)

	wfSvc, err := workflow.NewService(q)
	if err != nil {
		t.Fatal(err)
	}
	execRegistry := workflow.NewRegistryWithServices(workflow.Executors{
		Artifacts: artifactSvc,
		Documents: artifactSvc,
		Summarize: artifacts.NewSummarizer(nil), // deterministic degraded mode
		Traces:    traceSvc,
	})
	runtime := workflow.NewRuntime(q, wfSvc, execRegistry)
	if err := workflow.NewRunJobHandler(runtime, q, runner).RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}

	entID := newID("ent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: entID, Name: "工作流测试企业"}); err != nil {
		t.Fatal(err)
	}
	artifact, err := artifactSvc.IngestUpload(ctx, entID, "mes.md", "text/markdown", []byte("# MES SOP"))
	if err != nil {
		t.Fatal(err)
	}

	// ---- run 1: input -> parser.document -> transform.summarize -> trace.append ----
	def := workflow.Definition{
		Kind: sdkworkflow.KindIngestion, RiskLevel: sdkworkflow.RiskMedium,
		Nodes: []sdkworkflow.Node{
			{ID: "in", Type: sdkworkflow.NodeInputManual},
			{ID: "parse", Type: sdkworkflow.NodeParserDocument},
			{ID: "sum", Type: sdkworkflow.NodeTransformSummarize},
			{ID: "tr", Type: sdkworkflow.NodeTraceAppend},
		},
		Edges: []sdkworkflow.Edge{{From: "in", To: "parse"}, {From: "parse", To: "sum"}, {From: "sum", To: "tr"}},
	}
	wfID, err := wfSvc.CreateDraft(ctx, entID, "摄入流水线", "admin", def)
	if err != nil {
		t.Fatal(err)
	}
	version, err := wfSvc.Publish(ctx, entID, wfID, "admin")
	if err != nil {
		t.Fatal(err)
	}

	runID, err := runtime.CreatePending(ctx, entID, wfID, version, map[string]any{
		"artifact_id": artifact.ID, "ticket_id": "tick_wf", "question": "MES 文档要点",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Enqueue(ctx, workflow.JobTypeRun, runID); err != nil {
		t.Fatal(err) // MemBus executes synchronously
	}
	run, err := q.GetWorkflowRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != workflow.RunSucceeded {
		events, _ := q.ListWorkflowRunEvents(ctx, runID)
		t.Fatalf("run status = %s, events = %+v", run.Status, events)
	}
	events, err := q.ListWorkflowRunEvents(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	succeeded := map[string]bool{}
	for _, ev := range events {
		if ev.Status == workflow.NodeSucceeded {
			succeeded[ev.NodeID] = true
		}
	}
	for _, nodeID := range []string{"in", "parse", "sum", "tr"} {
		if !succeeded[nodeID] {
			t.Fatalf("node %s has no succeeded event: %+v", nodeID, events)
		}
	}
	// trace.append persisted a real answer trace bound to this run
	var state struct {
		Outputs map[string]map[string]any `json:"outputs"`
	}
	if err := json.Unmarshal(run.Output, &state); err != nil {
		t.Fatal(err)
	}
	traceID, _ := state.Outputs["tr"]["trace_id"].(string)
	if traceID == "" {
		t.Fatalf("trace.append produced no trace id: %v", state.Outputs)
	}
	traceRow, err := traceSvc.Get(ctx, traceID)
	if err != nil || traceRow.WorkflowRunID.String != runID {
		t.Fatalf("trace row: %+v err=%v", traceRow, err)
	}

	// ---- run 2: human.confirm pauses, approve resumes to completion ----
	confirmDef := workflow.Definition{
		Kind: sdkworkflow.KindSOP, RiskLevel: sdkworkflow.RiskHigh,
		Nodes: []sdkworkflow.Node{
			{ID: "in", Type: sdkworkflow.NodeInputManual},
			{ID: "gate", Type: sdkworkflow.NodeHumanConfirm, RequiresConfirmation: true},
			{ID: "tr", Type: sdkworkflow.NodeTraceAppend},
		},
		Edges: []sdkworkflow.Edge{{From: "in", To: "gate"}, {From: "gate", To: "tr"}},
	}
	cwfID, err := wfSvc.CreateDraft(ctx, entID, "确认门", "admin", confirmDef)
	if err != nil {
		t.Fatal(err)
	}
	cver, err := wfSvc.Publish(ctx, entID, cwfID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	crunID, err := runtime.CreatePending(ctx, entID, cwfID, cver, map[string]any{"ticket_id": "tick_wf"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Enqueue(ctx, workflow.JobTypeRun, crunID); err != nil {
		t.Fatal(err)
	}
	paused, err := q.GetWorkflowRun(ctx, crunID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != workflow.RunWaitingConfirmation {
		t.Fatalf("run must pause at human.confirm, got %s", paused.Status)
	}
	// Approve records the decision and puts the run back to pending; the
	// worker (re-enqueue) executes the post-gate nodes — the serving plane
	// never runs nodes itself (its registry is intentionally inert).
	status, err := runtime.Resume(ctx, crunID, entID, true, "看过了")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if status != workflow.RunPending {
		t.Fatalf("resume status = %s, want pending", status)
	}
	if err := runner.Enqueue(ctx, workflow.JobTypeRun, crunID); err != nil {
		t.Fatal(err) // MemBus executes synchronously — real claim + ExecuteClaimed
	}
	resumed, err := q.GetWorkflowRun(ctx, crunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != workflow.RunSucceeded {
		events, _ := q.ListWorkflowRunEvents(ctx, crunID)
		t.Fatalf("post-approve run status = %s, events = %+v", resumed.Status, events)
	}
	// The post-gate trace.append node genuinely executed on the worker path.
	cevents, err := q.ListWorkflowRunEvents(ctx, crunID)
	if err != nil {
		t.Fatal(err)
	}
	trDone := false
	for _, ev := range cevents {
		if ev.NodeID == "tr" && ev.Status == workflow.NodeSucceeded {
			trDone = true
		}
	}
	if !trDone {
		t.Fatalf("post-gate node tr never succeeded: %+v", cevents)
	}
	t.Logf("run1=%s succeeded with trace %s; run2 paused → approved → worker completed", runID, traceID)
}
