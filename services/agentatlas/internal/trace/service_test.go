package trace

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
)

type fakeTraceStore struct {
	traces    []db.CreateAnswerTraceParams
	steps     []db.InsertAnswerTraceStepParams
	evidence  []db.InsertAnswerTraceEvidenceParams
	events    []db.InsertAnswerTraceModelEventParams
	auditRefs []db.InsertAnswerTraceAuditRefParams
}

func (f *fakeTraceStore) CreateAnswerTrace(_ context.Context, arg db.CreateAnswerTraceParams) (db.AnswerTrace, error) {
	f.traces = append(f.traces, arg)
	return db.AnswerTrace{ID: arg.ID, EnterpriseID: arg.EnterpriseID, CaseTicketID: arg.CaseTicketID,
		QuestionHash: arg.QuestionHash, AnswerHash: arg.AnswerHash,
		SpaceIds: arg.SpaceIds, EvidencePointerIds: arg.EvidencePointerIds, ModelRoute: arg.ModelRoute}, nil
}
func (f *fakeTraceStore) GetAnswerTrace(context.Context, string) (db.AnswerTrace, error) {
	return db.AnswerTrace{}, nil
}
func (f *fakeTraceStore) InsertAnswerTraceStep(_ context.Context, arg db.InsertAnswerTraceStepParams) error {
	f.steps = append(f.steps, arg)
	return nil
}
func (f *fakeTraceStore) InsertAnswerTraceEvidence(_ context.Context, arg db.InsertAnswerTraceEvidenceParams) error {
	f.evidence = append(f.evidence, arg)
	return nil
}
func (f *fakeTraceStore) InsertAnswerTraceModelEvent(_ context.Context, arg db.InsertAnswerTraceModelEventParams) error {
	f.events = append(f.events, arg)
	return nil
}
func (f *fakeTraceStore) InsertAnswerTraceAuditRef(_ context.Context, arg db.InsertAnswerTraceAuditRefParams) error {
	f.auditRefs = append(f.auditRefs, arg)
	return nil
}

func TestCreatePersistsAllFacets(t *testing.T) {
	store := &fakeTraceStore{}
	svc := NewService(store)
	row, err := svc.Create(context.Background(), Record{
		EnterpriseID: "ent_1", CaseTicketID: "tick_1", ActorUserID: "u_zhang",
		Question: "我的工作内容是什么？", SanitizedQuestionSummary: "员工询问工作内容",
		SpaceIDs: []string{"spc_1"}, RetrievalPlanID: "rp_1",
		EvidencePointerIDs: []string{"ev_1", "ev_2"}, ReadGrantIDs: []string{"g_1"},
		ModelRoute: "llmrouter/deepseek-v4", Answer: "你当前负责 MES 工单专项。",
		Steps: []Step{{Kind: "verify_ticket"}, {Kind: "retrieve"}, {Kind: "generate"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(row.QuestionHash, "sha256:") || !strings.HasPrefix(row.AnswerHash, "sha256:") {
		t.Fatalf("hashes: %+v", row)
	}
	if len(store.steps) != 3 || store.steps[2].StepNo != 3 {
		t.Fatalf("steps: %+v", store.steps)
	}
	if len(store.evidence) != 2 || store.evidence[0].GrantID != "g_1" || store.evidence[1].GrantID != "" {
		t.Fatalf("evidence grants: %+v", store.evidence)
	}
	if len(store.events) != 1 {
		t.Fatalf("model events: %+v", store.events)
	}
	if _, err := svc.Create(context.Background(), Record{}); err == nil {
		t.Fatal("missing enterprise/ticket must fail")
	}
}

type flakyNexus struct {
	nexusclient.Mock
	fail bool
}

func (f *flakyNexus) AppendAuditEvidence(ctx context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	if f.fail {
		return nexus.AppendAuditEvidenceResponse{}, errors.New("audit chain unavailable")
	}
	return f.Mock.AppendAuditEvidence(ctx, req)
}

func TestAppendAuditFailsClosed(t *testing.T) {
	store := &fakeTraceStore{}
	svc := NewService(store)
	row := db.AnswerTrace{ID: "tr_1", EnterpriseID: "ent_1", QuestionHash: "h", AnswerHash: "h"}

	bad := &flakyNexus{Mock: *nexusclient.NewMock(), fail: true}
	if _, err := svc.AppendAudit(context.Background(), bad, "tick_1", row); err == nil {
		t.Fatal("audit failure must fail closed")
	}
	if len(store.auditRefs) != 0 {
		t.Fatal("no audit ref may be recorded on failure")
	}

	good := &flakyNexus{Mock: *nexusclient.NewMock()}
	ref, err := svc.AppendAudit(context.Background(), good, "tick_1", row)
	if err != nil || ref == "" {
		t.Fatalf("audit append: %q %v", ref, err)
	}
	if len(store.auditRefs) != 1 || store.auditRefs[0].AuditRefID != ref {
		t.Fatalf("audit ref rows: %+v", store.auditRefs)
	}
}
