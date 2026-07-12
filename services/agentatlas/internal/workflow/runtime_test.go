package workflow

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// fakeStore mirrors the schema semantics needed by the runtime.
type fakeStore struct {
	workflows     map[string]db.Workflow
	versions      map[string]db.WorkflowVersion // key wf|version
	nodes         []db.InsertWorkflowNodeParams
	edges         []db.InsertWorkflowEdgeParams
	runs          map[string]db.WorkflowRun
	events        []db.WorkflowRunEvent
	nextEvent     int64
	dreamRuns     map[string]db.DreamRun
	outbox        []db.DreamWorkflowLifecycleOutbox
	transitionErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		workflows: map[string]db.Workflow{},
		versions:  map[string]db.WorkflowVersion{},
		runs:      map[string]db.WorkflowRun{},
		dreamRuns: map[string]db.DreamRun{},
	}
}

func TestDreamPauseRemainsDurableWhenEagerLifecycleFails(t *testing.T) {
	store, svc, rt := setup(t)
	ctx := context.Background()
	def := Definition{WorkflowID: "wf_durable_pause", Kind: sdkworkflow.KindDream,
		Nodes: []sdkworkflow.Node{{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm}}, Edges: []sdkworkflow.Edge{}, RiskLevel: sdkworkflow.RiskLow}
	id, err := svc.CreateDraft(ctx, "ent_1", "Dream pause", "admin", def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, "ent_1", id, "admin"); err != nil {
		t.Fatal(err)
	}
	store.dreamRuns["dr_pause"] = db.DreamRun{ID: "dr_pause", EnterpriseID: "ent_1", PolicyID: "p_1", PolicyVersion: 1,
		OrgUnitID: "department:one", WorkflowID: pgtype.Text{String: id, Valid: true}, WorkflowVersion: pgtype.Int4{Int32: 1, Valid: true}, Status: "running", ExecutionOwner: pgtype.Text{String: "dream-owner", Valid: true}, ExecutionLeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}
	rt.SetDreamLifecycleHook(func(_ context.Context, result RunResult) error {
		if result.Status == RunWaitingConfirmation {
			return errors.New("injected reconciliation failure")
		}
		return nil
	})
	result, err := rt.RunDreamPublished(ctx, "ent_1", id, 1, map[string]any{}, VerifiedDreamContext{
		EnterpriseID: "ent_1", DreamRunID: "dr_pause", PolicyID: "p_1", PolicyVersion: 1,
		WorkflowID: id, WorkflowVersion: 1, OrgUnitID: "department:one", DreamExecutionOwner: "dream-owner",
	})
	if err != nil || result.Status != RunWaitingConfirmation || store.runs[result.RunID].Status != RunWaitingConfirmation {
		t.Fatalf("pause was not durably persisted: result=%+v row=%+v err=%v", result, store.runs[result.RunID], err)
	}
}

func TestDreamWorkflowBindingRejectsForgedPolicyOrOrg(t *testing.T) {
	store, svc, rt := setup(t)
	ctx := context.Background()
	def := Definition{WorkflowID: "wf_bound_context", Kind: sdkworkflow.KindDream,
		Nodes: []sdkworkflow.Node{{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm}}, Edges: []sdkworkflow.Edge{}, RiskLevel: sdkworkflow.RiskLow}
	id, err := svc.CreateDraft(ctx, "ent_1", "Dream binding", "admin", def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, "ent_1", id, "admin"); err != nil {
		t.Fatal(err)
	}
	store.dreamRuns["dr_bound"] = db.DreamRun{ID: "dr_bound", EnterpriseID: "ent_1", PolicyID: "p_real", PolicyVersion: 7,
		OrgUnitID: "department:real", WorkflowID: pgtype.Text{String: id, Valid: true}, WorkflowVersion: pgtype.Int4{Int32: 1, Valid: true}, Status: "running", ExecutionOwner: pgtype.Text{String: "dream-owner", Valid: true}, ExecutionLeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}
	for name, mutate := range map[string]func(*VerifiedDreamContext){
		"policy": func(d *VerifiedDreamContext) { d.PolicyID = "p_forged" },
		"org":    func(d *VerifiedDreamContext) { d.OrgUnitID = "department:forged" },
	} {
		t.Run(name, func(t *testing.T) {
			dream := VerifiedDreamContext{EnterpriseID: "ent_1", DreamRunID: "dr_bound", PolicyID: "p_real", PolicyVersion: 7,
				WorkflowID: id, WorkflowVersion: 1, OrgUnitID: "department:real", DreamExecutionOwner: "dream-owner"}
			mutate(&dream)
			if _, err := rt.RunDreamPublished(ctx, "ent_1", id, 1, map[string]any{}, dream); err == nil {
				t.Fatal("forged Dream context created a workflow run")
			}
		})
	}
	if len(store.runs) != 0 {
		t.Fatalf("forged contexts persisted workflow runs: %d", len(store.runs))
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

func (f *fakeStore) CreateBoundDreamWorkflowRun(_ context.Context, arg db.CreateBoundDreamWorkflowRunParams) (db.CreateBoundDreamWorkflowRunRow, error) {
	dream, ok := f.dreamRuns[arg.DreamRunID]
	if !ok || dream.EnterpriseID != arg.EnterpriseID || dream.PolicyID != arg.PolicyID || dream.PolicyVersion != arg.PolicyVersion || dream.ExecutionOwner != arg.DreamExecutionOwner ||
		dream.OrgUnitID != arg.OrgUnitID || dream.WorkflowID != arg.WorkflowID || dream.WorkflowVersion != arg.Version || dream.Status != "running" || dream.WorkflowRunID.Valid {
		return db.CreateBoundDreamWorkflowRunRow{}, pgx.ErrNoRows
	}
	run := db.WorkflowRun{ID: arg.ID, WorkflowID: arg.WorkflowID.String, Version: arg.Version.Int32,
		EnterpriseID: arg.EnterpriseID, Status: arg.Status, Input: arg.Input, Output: arg.Output,
		ExecutionOwner: arg.ExecutionOwner, ExecutionLeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}
	f.runs[arg.ID] = run
	dream.WorkflowRunID = pgtype.Text{String: arg.ID, Valid: true}
	f.dreamRuns[dream.ID] = dream
	return db.CreateBoundDreamWorkflowRunRow{ID: run.ID, WorkflowID: run.WorkflowID, Version: run.Version,
		EnterpriseID: run.EnterpriseID, Status: run.Status, Input: run.Input, Output: run.Output}, nil
}

func (f *fakeStore) TransitionDreamWorkflowRun(_ context.Context, arg db.TransitionDreamWorkflowRunParams) (int64, error) {
	if f.transitionErr != nil {
		return 0, f.transitionErr
	}
	run, ok := f.runs[arg.ID]
	if !ok || run.EnterpriseID != arg.EnterpriseID || run.Status != arg.ExpectedStatus || run.StateRevision != arg.ExpectedRevision || run.ExecutionOwner != arg.ExpectedOwner {
		return 0, nil
	}
	run.Status, run.Output, run.StateRevision = arg.Status, arg.RunOutput, run.StateRevision+1
	if arg.Status != RunRunning {
		run.ExecutionOwner, run.ExecutionLeaseExpiresAt = pgtype.Text{}, pgtype.Timestamptz{}
	}
	f.runs[arg.ID] = run
	f.nextEvent++
	f.events = append(f.events, db.WorkflowRunEvent{ID: f.nextEvent, RunID: arg.ID, NodeID: arg.NodeID, Status: arg.EventStatus, Detail: arg.EventDetail})
	for _, event := range f.outbox {
		if event.WorkflowRunID == arg.ID && event.Status == arg.Status {
			return run.StateRevision, nil
		}
	}
	for _, dream := range f.dreamRuns {
		if dream.WorkflowRunID.Valid && dream.WorkflowRunID.String == arg.ID && dream.EnterpriseID == arg.EnterpriseID {
			f.outbox = append(f.outbox, db.DreamWorkflowLifecycleOutbox{ID: int64(len(f.outbox) + 1), EnterpriseID: arg.EnterpriseID,
				DreamRunID: dream.ID, WorkflowRunID: arg.ID, Status: arg.Status, LifecycleError: arg.LifecycleError})
			return run.StateRevision, nil
		}
	}
	return 0, nil
}

func (f *fakeStore) TransitionWorkflowRunWithEvent(_ context.Context, arg db.TransitionWorkflowRunWithEventParams) (int64, error) {
	if f.transitionErr != nil {
		return 0, f.transitionErr
	}
	run, ok := f.runs[arg.ID]
	if !ok || run.Status != arg.ExpectedStatus || run.StateRevision != arg.ExpectedRevision || run.ExecutionOwner != arg.ExpectedOwner {
		return 0, nil
	}
	run.Status, run.Output, run.StateRevision = arg.Status, arg.RunOutput, run.StateRevision+1
	if arg.Status != RunRunning {
		run.ExecutionOwner, run.ExecutionLeaseExpiresAt = pgtype.Text{}, pgtype.Timestamptz{}
	}
	f.runs[arg.ID] = run
	f.nextEvent++
	f.events = append(f.events, db.WorkflowRunEvent{ID: f.nextEvent, RunID: arg.ID, NodeID: arg.NodeID, Status: arg.EventStatus, Detail: arg.EventDetail})
	return run.StateRevision, nil
}

func (f *fakeStore) ClaimWorkflowRunLease(_ context.Context, arg db.ClaimWorkflowRunLeaseParams) (db.WorkflowRun, error) {
	run, ok := f.runs[arg.ID]
	if !ok || run.Status != RunPending || run.ExecutionOwner.Valid {
		return db.WorkflowRun{}, pgx.ErrNoRows
	}
	run.Status, run.ExecutionOwner = RunRunning, arg.ExecutionOwner
	run.ExecutionLeaseExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
	run.StateRevision++
	f.runs[arg.ID] = run
	return run, nil
}

func (f *fakeStore) RenewWorkflowRunLease(_ context.Context, arg db.RenewWorkflowRunLeaseParams) (int64, error) {
	run, ok := f.runs[arg.ID]
	if !ok || run.Status != RunRunning || run.ExecutionOwner != arg.ExecutionOwner {
		return 0, nil
	}
	run.ExecutionLeaseExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
	f.runs[arg.ID] = run
	return 1, nil
}

func TestDreamAuditAndStateTransitionAreAtomic(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		store, _, rt, dream := setupBoundDreamRuntime(t, "wf_atomic_pause", []sdkworkflow.Node{{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm}})
		store.transitionErr = errors.New("injected transition failure")
		result, err := rt.RunDreamPublished(context.Background(), dream.EnterpriseID, dream.WorkflowID, dream.WorkflowVersion, map[string]any{}, dream)
		if err == nil || len(store.eventStatuses(result.RunID, "confirm")) != 0 || store.runs[result.RunID].Status != RunRunning {
			t.Fatalf("pause was not atomic: result=%+v events=%v row=%+v err=%v", result, store.eventStatuses(result.RunID, "confirm"), store.runs[result.RunID], err)
		}
	})
	t.Run("rejection", func(t *testing.T) {
		store, _, rt, dream := setupBoundDreamRuntime(t, "wf_atomic_reject", []sdkworkflow.Node{{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm}})
		result, err := rt.RunDreamPublished(context.Background(), dream.EnterpriseID, dream.WorkflowID, dream.WorkflowVersion, map[string]any{}, dream)
		if err != nil || result.Status != RunWaitingConfirmation {
			t.Fatalf("pause setup: %+v %v", result, err)
		}
		before := len(store.events)
		store.transitionErr = errors.New("injected transition failure")
		if _, err := rt.Resume(context.Background(), result.RunID, dream.EnterpriseID, false, "reject"); err == nil {
			t.Fatal("rejection transition failure was discarded")
		}
		if len(store.events) != before || store.runs[result.RunID].Status != RunWaitingConfirmation {
			t.Fatalf("rejection was not atomic: events=%v row=%+v", store.events, store.runs[result.RunID])
		}
	})
	t.Run("node_failure", func(t *testing.T) {
		store, _, rt, dream := setupBoundDreamRuntime(t, "wf_atomic_failure", []sdkworkflow.Node{{ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}})
		store.transitionErr = errors.New("injected transition failure")
		result, err := rt.RunDreamPublished(context.Background(), dream.EnterpriseID, dream.WorkflowID, dream.WorkflowVersion, map[string]any{}, dream)
		if err == nil || !reflect.DeepEqual(store.eventStatuses(result.RunID, "aggregate"), []string{NodeRunning}) || store.runs[result.RunID].Status != RunRunning {
			t.Fatalf("failure was not atomic: result=%+v events=%v row=%+v err=%v", result, store.eventStatuses(result.RunID, "aggregate"), store.runs[result.RunID], err)
		}
	})
	t.Run("final_success", func(t *testing.T) {
		store, _, rt, dream := setupBoundDreamRuntime(t, "wf_atomic_success", []sdkworkflow.Node{{ID: "input", Type: sdkworkflow.NodeInputManual}})
		store.transitionErr = errors.New("injected transition failure")
		result, err := rt.RunDreamPublished(context.Background(), dream.EnterpriseID, dream.WorkflowID, dream.WorkflowVersion, map[string]any{}, dream)
		if err == nil || !reflect.DeepEqual(store.eventStatuses(result.RunID, "input"), []string{NodeRunning}) || store.runs[result.RunID].Status != RunRunning {
			t.Fatalf("success was not atomic: result=%+v events=%v row=%+v err=%v", result, store.eventStatuses(result.RunID, "input"), store.runs[result.RunID], err)
		}
	})
}

func setupBoundDreamRuntime(t *testing.T, workflowID string, nodes []sdkworkflow.Node) (*fakeStore, *Service, *Runtime, VerifiedDreamContext) {
	t.Helper()
	store, svc, rt := setup(t)
	def := Definition{WorkflowID: workflowID, Kind: sdkworkflow.KindDream, Nodes: nodes, Edges: []sdkworkflow.Edge{}, RiskLevel: sdkworkflow.RiskLow}
	id, err := svc.CreateDraft(context.Background(), "ent_atomic", "Atomic Dream", "admin", def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(context.Background(), "ent_atomic", id, "admin"); err != nil {
		t.Fatal(err)
	}
	dream := VerifiedDreamContext{EnterpriseID: "ent_atomic", DreamRunID: "dr_" + workflowID, PolicyID: "policy_atomic", PolicyVersion: 1,
		WorkflowID: id, WorkflowVersion: 1, OrgUnitID: "department:atomic", DreamExecutionOwner: "dream-owner"}
	store.dreamRuns[dream.DreamRunID] = db.DreamRun{ID: dream.DreamRunID, EnterpriseID: dream.EnterpriseID, PolicyID: dream.PolicyID,
		PolicyVersion: dream.PolicyVersion, OrgUnitID: dream.OrgUnitID, WorkflowID: pgtype.Text{String: id, Valid: true},
		WorkflowVersion: pgtype.Int4{Int32: 1, Valid: true}, Status: "running", ExecutionOwner: pgtype.Text{String: "dream-owner", Valid: true}, ExecutionLeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}
	return store, svc, rt, dream
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

func TestRunPublishedReturnsOutputsFromExactlyPinnedVersion(t *testing.T) {
	store, svc, rt := setup(t)
	ctx := context.Background()
	def1 := Definition{WorkflowID: "wf_pinned", Kind: sdkworkflow.KindDream,
		Nodes: []sdkworkflow.Node{{ID: "v1", Type: sdkworkflow.NodeInputManual}}, Edges: []sdkworkflow.Edge{}, RiskLevel: sdkworkflow.RiskLow}
	id, err := svc.CreateDraft(ctx, "ent_1", "Dream", "admin", def1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, "ent_1", id, "admin"); err != nil {
		t.Fatal(err)
	}
	def2 := def1
	def2.Nodes = append(def2.Nodes, sdkworkflow.Node{ID: "v2", Type: sdkworkflow.NodeInputEvidencePointer, Config: map[string]any{"evidence_pointer_id": "ev-v2"}})
	def2.Edges = []sdkworkflow.Edge{{From: "v1", To: "v2"}}
	if err := svc.UpdateDraft(ctx, "ent_1", id, def2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, "ent_1", id, "admin"); err != nil {
		t.Fatal(err)
	}

	result, err := rt.RunPublished(ctx, "ent_1", id, 1, map[string]any{"value": "safe"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunSucceeded || result.Outputs["v1"]["input"] == nil || result.Outputs["v2"] != nil {
		t.Fatalf("result = %+v", result)
	}
	if stored := store.runs[result.RunID]; stored.Version != 1 || stored.WorkflowID != id {
		t.Fatalf("stored binding = %s@%d", stored.WorkflowID, stored.Version)
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
	owner := pgtype.Text{String: "test-workflow-owner", Valid: true}
	claimed := store.runs[runID]
	claimed.ExecutionOwner = owner
	claimed.ExecutionLeaseExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
	store.runs[runID] = claimed
	final, err := rt.ExecuteClaimed(ctx, runID, owner)
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

func TestFailedDreamWorkflowNotifiesTerminalLifecycle(t *testing.T) {
	store, svc, rt := setup(t)
	ctx := context.Background()
	def := Definition{WorkflowID: "wf_dream_fail", Kind: sdkworkflow.KindDream, Nodes: []sdkworkflow.Node{{ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}}, Edges: []sdkworkflow.Edge{}, RiskLevel: sdkworkflow.RiskLow}
	id, err := svc.CreateDraft(ctx, "ent_1", "Dream fail", "admin", def)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, "ent_1", id, "admin"); err != nil {
		t.Fatal(err)
	}
	store.dreamRuns["dr_1"] = db.DreamRun{ID: "dr_1", EnterpriseID: "ent_1", PolicyID: "p_1", PolicyVersion: 1,
		OrgUnitID: "department:one", WorkflowID: pgtype.Text{String: id, Valid: true}, WorkflowVersion: pgtype.Int4{Int32: 1, Valid: true}, Status: "running", ExecutionOwner: pgtype.Text{String: "dream-owner", Valid: true}, ExecutionLeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}}
	var statuses []string
	rt.SetDreamLifecycleHook(func(_ context.Context, result RunResult) error {
		statuses = append(statuses, result.Status)
		return nil
	})
	_, err = rt.RunDreamPublished(ctx, "ent_1", id, 1, map[string]any{}, VerifiedDreamContext{EnterpriseID: "ent_1", DreamRunID: "dr_1", PolicyID: "p_1", PolicyVersion: 1, WorkflowID: id, WorkflowVersion: 1, OrgUnitID: "department:one", DreamExecutionOwner: "dream-owner"})
	if err == nil || !reflect.DeepEqual(statuses, []string{RunFailed}) {
		t.Fatalf("statuses=%v err=%v runs=%d", statuses, err, len(store.runs))
	}
}

func TestTopoOrderRejectsCycle(t *testing.T) {
	def := validDef("wf_cycle")
	def.Edges = append(def.Edges, sdkworkflow.Edge{From: "out", To: "in"})
	if _, err := topoOrder(def); err == nil {
		t.Fatal("cycle must be rejected")
	}
}
