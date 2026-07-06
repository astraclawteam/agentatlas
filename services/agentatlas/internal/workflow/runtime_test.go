package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// fakeStore mirrors the schema semantics needed by the runtime.
type fakeStore struct {
	workflows map[string]db.Workflow
	versions  map[string]db.WorkflowVersion // key wf|version
	nodes     []db.InsertWorkflowNodeParams
	edges     []db.InsertWorkflowEdgeParams
	runs      map[string]db.WorkflowRun
	events    []db.WorkflowRunEvent
	nextEvent int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		workflows: map[string]db.Workflow{},
		versions:  map[string]db.WorkflowVersion{},
		runs:      map[string]db.WorkflowRun{},
	}
}

func vkey(wf string, v int32) string { return fmt.Sprintf("%s|%d", wf, v) }

func (f *fakeStore) CreateWorkflowDraft(_ context.Context, arg db.CreateWorkflowDraftParams) (db.Workflow, error) {
	w := db.Workflow{ID: arg.ID, EnterpriseID: arg.EnterpriseID, Name: arg.Name, Kind: arg.Kind, Draft: arg.Draft}
	f.workflows[arg.ID] = w
	return w, nil
}

func (f *fakeStore) UpdateWorkflowDraft(_ context.Context, arg db.UpdateWorkflowDraftParams) (int64, error) {
	w, ok := f.workflows[arg.ID]
	if !ok || w.EnterpriseID != arg.EnterpriseID {
		return 0, nil
	}
	w.Draft = arg.Draft
	f.workflows[arg.ID] = w
	return 1, nil
}

func (f *fakeStore) GetWorkflow(_ context.Context, id string) (db.Workflow, error) {
	if w, ok := f.workflows[id]; ok {
		return w, nil
	}
	return db.Workflow{}, pgx.ErrNoRows
}

func (f *fakeStore) PublishWorkflowVersion(_ context.Context, arg db.PublishWorkflowVersionParams) (db.WorkflowVersion, error) {
	key := vkey(arg.WorkflowID, arg.Version)
	if _, exists := f.versions[key]; exists {
		return db.WorkflowVersion{}, fmt.Errorf("duplicate version %s", key)
	}
	def := make([]byte, len(arg.Definition))
	copy(def, arg.Definition) // immutability: store a private copy
	v := db.WorkflowVersion{WorkflowID: arg.WorkflowID, Version: arg.Version, Definition: def, RiskLevel: arg.RiskLevel}
	f.versions[key] = v
	return v, nil
}

func (f *fakeStore) GetWorkflowVersion(_ context.Context, arg db.GetWorkflowVersionParams) (db.WorkflowVersion, error) {
	if v, ok := f.versions[vkey(arg.WorkflowID, arg.Version)]; ok {
		return v, nil
	}
	return db.WorkflowVersion{}, pgx.ErrNoRows
}

func (f *fakeStore) GetLatestWorkflowVersion(_ context.Context, workflowID string) (db.WorkflowVersion, error) {
	var best db.WorkflowVersion
	found := false
	for _, v := range f.versions {
		if v.WorkflowID == workflowID && (!found || v.Version > best.Version) {
			best, found = v, true
		}
	}
	if !found {
		return db.WorkflowVersion{}, pgx.ErrNoRows
	}
	return best, nil
}

func (f *fakeStore) InsertWorkflowNode(_ context.Context, arg db.InsertWorkflowNodeParams) error {
	f.nodes = append(f.nodes, arg)
	return nil
}

func (f *fakeStore) InsertWorkflowEdge(_ context.Context, arg db.InsertWorkflowEdgeParams) error {
	f.edges = append(f.edges, arg)
	return nil
}

func (f *fakeStore) CreateWorkflowRun(_ context.Context, arg db.CreateWorkflowRunParams) (db.WorkflowRun, error) {
	r := db.WorkflowRun{ID: arg.ID, WorkflowID: arg.WorkflowID, Version: arg.Version,
		EnterpriseID: arg.EnterpriseID, Status: arg.Status, Input: arg.Input}
	f.runs[arg.ID] = r
	return r, nil
}

func (f *fakeStore) UpdateWorkflowRunStatus(_ context.Context, arg db.UpdateWorkflowRunStatusParams) (int64, error) {
	r, ok := f.runs[arg.ID]
	if !ok {
		return 0, nil
	}
	r.Status = arg.Status
	if arg.Output != nil {
		r.Output = arg.Output
	}
	f.runs[arg.ID] = r
	return 1, nil
}

func (f *fakeStore) GetWorkflowRun(_ context.Context, id string) (db.WorkflowRun, error) {
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return db.WorkflowRun{}, pgx.ErrNoRows
}

func (f *fakeStore) InsertWorkflowRunEvent(_ context.Context, arg db.InsertWorkflowRunEventParams) (db.WorkflowRunEvent, error) {
	f.nextEvent++
	ev := db.WorkflowRunEvent{ID: f.nextEvent, RunID: arg.RunID, NodeID: arg.NodeID, Status: arg.Status, Detail: arg.Detail}
	f.events = append(f.events, ev)
	return ev, nil
}

func (f *fakeStore) ListWorkflowRunEvents(_ context.Context, runID string) ([]db.WorkflowRunEvent, error) {
	var out []db.WorkflowRunEvent
	for _, e := range f.events {
		if e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) eventStatuses(runID, nodeID string) []string {
	var out []string
	for _, e := range f.events {
		if e.RunID == runID && e.NodeID == nodeID {
			out = append(out, e.Status)
		}
	}
	return out
}

func setup(t *testing.T) (*fakeStore, *Service, *Runtime) {
	t.Helper()
	store := newFakeStore()
	svc, err := NewService(store)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	rt := NewRuntime(store, svc, NewRegistry())
	return store, svc, rt
}

func TestCreateDraftRejectsInvalidDefinition(t *testing.T) {
	_, svc, _ := setup(t)
	def := validDef("wf_bad")
	def.Nodes[0].Type = "custom.hack"
	if _, err := svc.CreateDraft(context.Background(), "ent_1", "bad", "admin", def); err == nil {
		t.Fatal("invalid draft must be rejected")
	}
}

func TestPublishedVersionImmutable(t *testing.T) {
	store, svc, _ := setup(t)
	ctx := context.Background()

	id, err := svc.CreateDraft(ctx, "ent_1", "MES SOP", "admin", validDef("wf_1"))
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	v1, err := svc.Publish(ctx, "ent_1", id, "admin")
	if err != nil || v1 != 1 {
		t.Fatalf("publish v1: %v (v=%d)", err, v1)
	}
	v1def := string(store.versions[vkey(id, 1)].Definition)

	def2 := validDef(id)
	def2.Nodes = append(def2.Nodes, sdkworkflow.Node{ID: "extra", Type: sdkworkflow.NodeInputManual})
	def2.Edges = append(def2.Edges, sdkworkflow.Edge{From: "out", To: "extra"})
	if err := svc.UpdateDraft(ctx, "ent_1", id, def2); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	v2, err := svc.Publish(ctx, "ent_1", id, "admin")
	if err != nil || v2 != 2 {
		t.Fatalf("publish v2: %v (v=%d)", err, v2)
	}
	if got := string(store.versions[vkey(id, 1)].Definition); got != v1def {
		t.Fatal("published v1 definition changed after re-publish — versions must be immutable")
	}
	d1, _ := svc.VersionDefinition(ctx, id, 1)
	d2, _ := svc.VersionDefinition(ctx, id, 2)
	if len(d1.Nodes) != 3 || len(d2.Nodes) != 4 {
		t.Fatalf("version node counts: v1=%d v2=%d", len(d1.Nodes), len(d2.Nodes))
	}
}

func TestRunPausesAtHumanConfirmAndResumes(t *testing.T) {
	store, svc, rt := setup(t)
	ctx := context.Background()

	id, _ := svc.CreateDraft(ctx, "ent_1", "MES SOP", "admin", validDef("wf_run"))
	if _, err := svc.Publish(ctx, "ent_1", id, "admin"); err != nil {
		t.Fatalf("publish: %v", err)
	}

	runID, status, err := rt.StartRun(ctx, "ent_1", id, 1, map[string]any{"q": "test"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if status != RunWaitingConfirmation {
		t.Fatalf("status = %s, want waiting_confirmation", status)
	}
	if got := store.eventStatuses(runID, "confirm"); len(got) != 1 || got[0] != NodeWaitingConfirmation {
		t.Fatalf("confirm events = %v", got)
	}
	if got := store.eventStatuses(runID, "in"); len(got) != 2 || got[0] != NodeRunning || got[1] != NodeSucceeded {
		t.Fatalf("in events = %v", got)
	}

	// Approval only records the decision and hands the run back to pending —
	// execution is the worker's job (it owns the wired registry).
	resumed, err := rt.Resume(ctx, runID, "ent_1", true, "看过了")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed != RunPending {
		t.Fatalf("resume status = %s, want pending (re-enqueue for the worker)", resumed)
	}
	if store.runs[runID].Status != RunPending {
		t.Fatalf("run row status = %s, want pending", store.runs[runID].Status)
	}

	// Worker path: claim (pending→running keeps persisted state via COALESCE)
	// then continue from the node after the confirm gate.
	if _, err := store.UpdateWorkflowRunStatus(ctx, db.UpdateWorkflowRunStatusParams{ID: runID, Status: RunRunning, Output: nil}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	final, err := rt.ExecuteClaimed(ctx, runID)
	if err != nil {
		t.Fatalf("execute claimed: %v", err)
	}
	if final != RunSucceeded {
		t.Fatalf("final = %s", final)
	}
	if store.runs[runID].Status != RunSucceeded {
		t.Fatalf("run row status = %s", store.runs[runID].Status)
	}
	if got := store.eventStatuses(runID, "out"); len(got) != 2 || got[1] != NodeSucceeded {
		t.Fatalf("out events = %v", got)
	}
	// The pre-gate node must NOT re-execute on resume (state carried over).
	if got := store.eventStatuses(runID, "in"); len(got) != 2 {
		t.Fatalf("in node re-executed after resume: events = %v", got)
	}
}

func TestResumeCrossEnterpriseForbidden(t *testing.T) {
	_, svc, rt := setup(t)
	ctx := context.Background()

	id, _ := svc.CreateDraft(ctx, "ent_1", "MES SOP", "admin", validDef("wf_xent"))
	_, _ = svc.Publish(ctx, "ent_1", id, "admin")
	runID, _, err := rt.StartRun(ctx, "ent_1", id, 1, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := rt.Resume(ctx, runID, "ent_intruder", true, "sneaky"); !errors.Is(err, ErrRunForbidden) {
		t.Fatalf("cross-enterprise resume must fail closed, got err=%v", err)
	}
}

func TestCreatePendingCrossEnterpriseForbidden(t *testing.T) {
	_, svc, rt := setup(t)
	ctx := context.Background()

	id, _ := svc.CreateDraft(ctx, "ent_1", "MES SOP", "admin", validDef("wf_xpend"))
	_, _ = svc.Publish(ctx, "ent_1", id, "admin")
	if _, err := rt.CreatePending(ctx, "ent_intruder", id, 1, nil); !errors.Is(err, ErrWorkflowForbidden) {
		t.Fatalf("cross-enterprise run start must fail closed, got err=%v", err)
	}
}

func TestResumeRejectCancelsRun(t *testing.T) {
	_, svc, rt := setup(t)
	ctx := context.Background()
	store := rt.store.(*fakeStore)

	id, _ := svc.CreateDraft(ctx, "ent_1", "MES SOP", "admin", validDef("wf_rej"))
	_, _ = svc.Publish(ctx, "ent_1", id, "admin")
	runID, _, err := rt.StartRun(ctx, "ent_1", id, 1, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	status, err := rt.Resume(ctx, runID, "ent_1", false, "不批准")
	if err != nil || status != RunCancelled {
		t.Fatalf("reject: status=%s err=%v", status, err)
	}
	if store.runs[runID].Status != RunCancelled {
		t.Fatalf("run row = %s", store.runs[runID].Status)
	}
}

func TestRunFailsLoudOnUnwiredExecutor(t *testing.T) {
	store, svc, rt := setup(t)
	ctx := context.Background()

	def := Definition{
		WorkflowID: "wf_parse", Version: 0, Kind: sdkworkflow.KindIngestion,
		Nodes: []sdkworkflow.Node{
			{ID: "in", Type: sdkworkflow.NodeInputManual},
			{ID: "parse", Type: sdkworkflow.NodeParserDocument},
		},
		Edges:     []sdkworkflow.Edge{{From: "in", To: "parse"}},
		RiskLevel: sdkworkflow.RiskLow,
	}
	id, err := svc.CreateDraft(ctx, "ent_1", "ingest", "admin", def)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := svc.Publish(ctx, "ent_1", id, "admin"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	runID, status, err := rt.StartRun(ctx, "ent_1", id, 1, nil)
	if !errors.Is(err, ErrExecutorNotWired) {
		t.Fatalf("want ErrExecutorNotWired, got %v", err)
	}
	if status != RunFailed || store.runs[runID].Status != RunFailed {
		t.Fatalf("status=%s row=%s", status, store.runs[runID].Status)
	}
	if got := store.eventStatuses(runID, "parse"); len(got) != 2 || got[1] != NodeFailed {
		t.Fatalf("parse events = %v", got)
	}
}

func TestTopoOrderRejectsCycle(t *testing.T) {
	def := validDef("wf_cycle")
	def.Edges = append(def.Edges, sdkworkflow.Edge{From: "out", To: "in"})
	if _, err := topoOrder(def); err == nil {
		t.Fatal("cycle must be rejected")
	}
}
