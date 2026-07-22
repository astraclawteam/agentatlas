package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/jackc/pgx/v5/pgtype"
)

type fakeDreamRunStore struct {
	run         db.GetDreamRunViewRow
	views       map[string]db.GetDreamRunViewRow
	runs        []db.DreamRun
	children    []db.DreamRun
	annotations []db.CreateDreamAnnotationParams
	listCalls   int
}

func (f *fakeDreamRunStore) GetDreamRunView(_ context.Context, p db.GetDreamRunViewParams) (db.GetDreamRunViewRow, error) {
	if row, ok := f.views[p.RunID]; ok && row.EnterpriseID == p.EnterpriseID {
		return row, nil
	}
	if p.EnterpriseID != f.run.EnterpriseID || p.RunID != f.run.ID {
		return db.GetDreamRunViewRow{}, errors.New("not found")
	}
	return f.run, nil
}
func (f *fakeDreamRunStore) ListDreamRunsByOrg(_ context.Context, p db.ListDreamRunsByOrgParams) ([]db.DreamRun, error) {
	f.listCalls++
	if p.EnterpriseID != f.run.EnterpriseID || p.OrgUnitID != f.run.OrgUnitID {
		return nil, errors.New("outside enterprise")
	}
	return f.runs, nil
}
func (f *fakeDreamRunStore) CreateDreamAnnotation(_ context.Context, p db.CreateDreamAnnotationParams) (db.DreamRunAnnotation, error) {
	f.annotations = append(f.annotations, p)
	return db.DreamRunAnnotation{ID: p.ID, EnterpriseID: p.EnterpriseID, RunID: p.RunID, AnnotationType: p.AnnotationType, Body: p.Body, CreatedBy: p.CreatedBy}, nil
}

func (f *fakeDreamRunStore) ListDreamRunAnnotationsByRunBounded(_ context.Context, p db.ListDreamRunAnnotationsByRunBoundedParams) ([]db.DreamRunAnnotation, error) {
	out := make([]db.DreamRunAnnotation, 0, len(f.annotations))
	for _, item := range f.annotations {
		if item.EnterpriseID == p.EnterpriseID && item.RunID == p.RunID {
			out = append(out, db.DreamRunAnnotation{ID: item.ID, EnterpriseID: item.EnterpriseID, RunID: item.RunID, AnnotationType: item.AnnotationType, Body: item.Body, CreatedBy: item.CreatedBy})
		}
	}
	return out, nil
}

func (f *fakeDreamRunStore) ListDreamRunChildrenByParentBounded(_ context.Context, p db.ListDreamRunChildrenByParentBoundedParams) ([]db.DreamRun, error) {
	out := make([]db.DreamRun, 0, len(f.children))
	for _, child := range f.children {
		if child.EnterpriseID == p.EnterpriseID {
			out = append(out, child)
		}
	}
	return out, nil
}

type evidenceNexus struct {
	scopes        []string
	failAudit     bool
	emptyAudit    bool
	orgAuth       map[string]bool
	audits        []nexus.AppendAuditEvidenceRequest
	auditReceipts map[string]string
}

type fakeDreamRerunner struct {
	calls         int
	sourceRun     string
	key           string
	runID         string
	auditRefID    string
	backfillCalls int
	backfillReq   dream.BackfillRequest
}

func (f *fakeDreamRerunner) Backfill(_ context.Context, req dream.BackfillRequest) (string, error) {
	f.backfillCalls++
	f.backfillReq = req
	return "backfill-1", nil
}
func (f *fakeDreamRerunner) LookupBackfill(_ context.Context, req dream.BackfillRequest) (string, bool, error) {
	if f.backfillReq.IdempotencyKey == "" || f.backfillReq.IdempotencyKey != req.IdempotencyKey {
		return "", false, nil
	}
	if f.backfillReq.PolicyID != req.PolicyID || !f.backfillReq.WindowStart.Equal(req.WindowStart) || !f.backfillReq.WindowEnd.Equal(req.WindowEnd) || f.backfillReq.RerunOfRunID != req.RerunOfRunID {
		return "", false, errors.New("idempotency key is bound to another backfill")
	}
	return "backfill-1", true, nil
}

func (f *fakeDreamRerunner) Rerun(_ context.Context, _, sourceRun, key, auditRefID string) (string, error) {
	f.calls++
	f.sourceRun, f.key, f.auditRefID = sourceRun, key, auditRefID
	f.runID = "rerun-1"
	return f.runID, nil
}
func (f *fakeDreamRerunner) LookupRerun(_ context.Context, _, sourceRun, key string) (string, bool, error) {
	if f.key == "" || f.key != key {
		return "", false, nil
	}
	if f.sourceRun != sourceRun {
		return "", false, errors.New("idempotency key is bound to another rerun")
	}
	return f.runID, true, nil
}

func (n *evidenceNexus) VerifyTicket(context.Context, nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	return nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent-1", ActorUserID: "manager-1", Scopes: n.scopes, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (n *evidenceNexus) AppendAuditEvidence(_ context.Context, p nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	if n.failAudit {
		return nexus.AppendAuditEvidenceResponse{}, errors.New("audit down")
	}
	if n.emptyAudit {
		return nexus.AppendAuditEvidenceResponse{}, nil
	}
	if p.IdempotencyKey != "" {
		if n.auditReceipts == nil {
			n.auditReceipts = map[string]string{}
		}
		if ref := n.auditReceipts[p.IdempotencyKey]; ref != "" {
			return nexus.AppendAuditEvidenceResponse{AuditRefID: ref}, nil
		}
		n.audits = append(n.audits, p)
		ref := fmt.Sprintf("audit-%d", len(n.audits))
		n.auditReceipts[p.IdempotencyKey] = ref
		return nexus.AppendAuditEvidenceResponse{AuditRefID: ref}, nil
	}
	n.audits = append(n.audits, p)
	return nexus.AppendAuditEvidenceResponse{AuditRefID: "audit-1"}, nil
}

func allowDreamOrg(n *evidenceNexus, action, enterprise, org string) {
	if n.orgAuth == nil {
		n.orgAuth = map[string]bool{}
	}
	n.orgAuth[action+"|"+dreamOrgResourceURI(enterprise, org)] = true
}
func (*evidenceNexus) SubscribeOrgEvents(context.Context, string, int64, nexus.OrgEventHandler) error {
	return nil
}

func dreamRunFixture() db.GetDreamRunViewRow {
	return db.GetDreamRunViewRow{
		ID: "run-1", EnterpriseID: "ent-1", OrgUnitID: "department:rd", Status: "succeeded",
		WindowStart:   pgtype.Timestamptz{Time: time.Date(2026, 7, 9, 14, 0, 0, 0, time.UTC), Valid: true},
		WindowEnd:     pgtype.Timestamptz{Time: time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC), Valid: true},
		PolicyVersion: 2, WorkflowID: pgtype.Text{String: "wf-dream", Valid: true}, WorkflowVersion: pgtype.Int4{Int32: 3, Valid: true},
		ParentRunIds: []string{"child-1", "child-2"}, InputCount: 2, DisplaySummary: "safe display",
		Facts: []byte(`[]`), Themes: []byte(`[]`), Trends: []byte(`[{"id":"trend-1","title":"T","detail":"D","severity":"info","evidence_pointer_id":"ev-1"}]`),
		Risks: []byte(`[]`), Todos: []byte(`[]`), Coverage: []byte(`{"expected_children":2,"completed_children":2,"input_count":2}`), MissingInputs: []byte(`[]`),
		EvidencePointerID: pgtype.Text{String: "ev-sealed", Valid: true}, ModelRoute: "llmrouter/x", ModelVersion: "v1", Attempt: 1, IdempotencyKey: "idem-1",
		InputSnapshot: []byte(`{"source_counts":[],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"managers","org_unit_ids":["department:rd"],"masked_field_count":0}`),
	}
}

func requestDream(t *testing.T, router http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("X-Nexus-Ticket", "ticket-1")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestDreamRunReadAndAppendOnlyAnnotation(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run, runs: []db.DreamRun{{ID: run.ID, EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID}}}
	nx := &evidenceNexus{scopes: []string{"dream:read", "dream:annotate"}}
	allowDreamOrg(nx, "dream:read", "ent-1", "department:rd")
	allowDreamOrg(nx, "dream:annotate", "ent-1", "department:rd")
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", WorkCaseContextFor: alwaysWorkCaseBacked, Nexus: nx, DreamRuns: store})

	resp := requestDream(t, router, http.MethodGet, "/v1/dream/runs/run-1", nil)
	if resp.Code != http.StatusOK || !bytes.Contains(resp.Body.Bytes(), []byte(`"parent_run_ids":["child-1","child-2"]`)) || !bytes.Contains(resp.Body.Bytes(), []byte(`"trend-1"`)) {
		t.Fatalf("detail = %d %s", resp.Code, resp.Body.String())
	}
	before := append([]byte(nil), store.run.DisplaySummary...)
	resp = requestDream(t, router, http.MethodPost, "/v1/dream/runs/run-1/annotations", map[string]any{"action": "mark_incorrect", "comment": "wrong source"})
	if resp.Code != http.StatusCreated || len(store.annotations) != 1 || store.annotations[0].AnnotationType != "mark_incorrect" || !bytes.Equal(before, []byte(store.run.DisplaySummary)) {
		t.Fatalf("annotation = %d %s %+v", resp.Code, resp.Body.String(), store.annotations)
	}
}

func TestDreamRoutesRequireScopeAndOrgBoundNexusAuthorization(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run, runs: []db.DreamRun{{ID: run.ID, EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID}}}

	t.Run("read scope", func(t *testing.T) {
		nx := &evidenceNexus{}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{decision: "deny"}, Nexus: nx, DreamRuns: store}), http.MethodGet, "/v1/dream/runs/run-1", nil)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
	t.Run("cross org list denied before query", func(t *testing.T) {
		store.listCalls = 0
		nx := &evidenceNexus{scopes: []string{"dream:read"}, orgAuth: map[string]bool{}}
		authz := &allowOrgAuthorization{decision: "deny"}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{OrgAuthorization: authz, Nexus: nx, DreamRuns: store}), http.MethodGet, "/v1/dream/runs?org_unit_id=department:other", nil)
		if resp.Code != http.StatusForbidden || store.listCalls != 0 {
			t.Fatalf("status=%d listCalls=%d body=%s", resp.Code, store.listCalls, resp.Body.String())
		}
		// The denial must come from the AUTHORIZATION surface, bound to the exact
		// org unit - not from an evidence lookup echoing a synthesized URI.
		if authz.lastRequest.OrgUnitID != "department:other" || authz.lastRequest.Action == "" {
			t.Fatalf("authorization binding=%+v", authz.lastRequest)
		}
		// That authorization no longer touches evidence locate is now enforced by
		// the type system: the retired method does not exist.
	})
	t.Run("cross org detail denied", func(t *testing.T) {
		nx := &evidenceNexus{scopes: []string{"dream:read"}, orgAuth: map[string]bool{}}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{decision: "deny"}, Nexus: nx, DreamRuns: store}), http.MethodGet, "/v1/dream/runs/run-1", nil)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
	t.Run("cross org annotation denied", func(t *testing.T) {
		store.annotations = nil
		nx := &evidenceNexus{scopes: []string{"dream:annotate"}, orgAuth: map[string]bool{}}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{decision: "deny"}, Nexus: nx, DreamRuns: store}), http.MethodPost, "/v1/dream/runs/run-1/annotations", map[string]any{"action": "comment", "comment": "x"})
		if resp.Code != http.StatusForbidden || len(store.annotations) != 0 {
			t.Fatalf("status=%d annotations=%v", resp.Code, store.annotations)
		}
	})
	t.Run("cross org rerun denied", func(t *testing.T) {
		nx := &evidenceNexus{scopes: []string{"dream:rerun"}, orgAuth: map[string]bool{}}
		rerunner := &fakeDreamRerunner{}
		req := httptest.NewRequest(http.MethodPost, "/v1/dream/runs/run-1/reruns", nil)
		req.Header.Set("X-Nexus-Ticket", "ticket-1")
		req.Header.Set("Idempotency-Key", "idem")
		resp := httptest.NewRecorder()
		NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{decision: "deny"}, Nexus: nx, DreamRuns: store, DreamRerun: rerunner}).ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden || rerunner.calls != 0 {
			t.Fatalf("status=%d calls=%d", resp.Code, rerunner.calls)
		}
	})
	t.Run("cross org evidence denied before grant", func(t *testing.T) {
		nx := &evidenceNexus{scopes: []string{"dream:evidence:read"}, orgAuth: map[string]bool{}}
		evd := &fakeFrozenEvidence{}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{decision: "deny"}, Evidence: evd, Nexus: nx, DreamRuns: store}), http.MethodPost, "/v1/dream/runs/run-1/evidence-access", nil)
		// Denied before any evidence is touched: the read must not have happened.
		if resp.Code != http.StatusForbidden || len(evd.reads) != 0 || len(evd.locates) != 0 {
			t.Fatalf("status=%d locates=%v reads=%v", resp.Code, evd.locates, evd.reads)
		}
	})
}

func TestDreamRerunReceiptReconcilesAfterRunBeforeCompletionFailure(t *testing.T) {
	store := &fakeDreamRunStore{run: dreamRunFixture()}
	nx := &evidenceNexus{scopes: []string{"dream:rerun"}}
	allowDreamOrg(nx, "dream:rerun", "ent-1", store.run.OrgUnitID)
	rerunner := &fakeDreamRerunner{}
	policyStore := newFakePolicyStore()
	policyStore.failCompleteOnce = true
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", WorkCaseContextFor: alwaysWorkCaseBacked, Nexus: nx, DreamRuns: store, DreamRerun: rerunner, Dreams: dream.NewPolicyService(policyStore)})
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/dream/runs/run-1/reruns", nil)
		req.Header.Set("X-Nexus-Ticket", "ticket-1")
		req.Header.Set("Idempotency-Key", "rerun-receipt-key-0001")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	if first := request(); first.Code != http.StatusInternalServerError {
		t.Fatalf("first=%d body=%s", first.Code, first.Body.String())
	}
	if retry := request(); retry.Code != http.StatusAccepted {
		t.Fatalf("retry=%d body=%s", retry.Code, retry.Body.String())
	}
	if rerunner.calls != 1 || len(nx.audits) != 1 {
		t.Fatalf("reruns=%d semantic_audits=%d", rerunner.calls, len(nx.audits))
	}
}

func TestDreamBackfillReceiptReconcilesAfterRunBeforeCompletionFailure(t *testing.T) {
	nx := &evidenceNexus{scopes: []string{"edit"}}
	allowDreamOrg(nx, "edit", "ent-1", "pg_mes")
	runner := &fakeDreamRerunner{}
	policyStore := newFakePolicyStore()
	raw, _ := json.Marshal(canonicalDreamPolicyBody())
	policyStore.policies["policy-1"] = db.DreamPolicy{ID: "policy-1", EnterpriseID: "ent-1", OrgScope: "department:rd", Status: "published", Draft: raw, RequesterUserID: "manager-1", PermissionMode: "direct_edit", RiskReasons: []byte(`[]`), ReviewOrgPath: []byte(`[]`)}
	policyStore.failCompleteOnce = true
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", WorkCaseContextFor: alwaysWorkCaseBacked, Nexus: nx, DreamRerun: runner, Dreams: dream.NewPolicyService(policyStore)})
	body := map[string]any{"window_start": "2026-07-01T00:00:00Z", "window_end": "2026-07-02T00:00:00Z"}
	request := func() *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/dream-policies/policy-1/backfills", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Nexus-Ticket", "ticket-1")
		req.Header.Set("Idempotency-Key", "backfill-receipt-key-0001")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	if first := request(); first.Code != http.StatusInternalServerError {
		t.Fatalf("first=%d body=%s", first.Code, first.Body.String())
	}
	if retry := request(); retry.Code != http.StatusAccepted {
		t.Fatalf("retry=%d body=%s", retry.Code, retry.Body.String())
	}
	if runner.backfillCalls != 1 || len(nx.audits) != 1 {
		t.Fatalf("backfills=%d semantic_audits=%d", runner.backfillCalls, len(nx.audits))
	}
}

func TestFailedDreamRunNeverExposesStoredSummary(t *testing.T) {
	run := dreamRunFixture()
	run.Status = "failed"
	view, err := dreamRunView(run)
	if err != nil {
		t.Fatal(err)
	}
	if view.DisplaySummary != "" || view.EvidencePointerID != "" || len(view.Trends) != 0 {
		t.Fatalf("failed output exposed: %+v", view)
	}
}

func TestDreamEvidenceAccessRequiresScopeBoundGrantAndAudit(t *testing.T) {
	run := dreamRunFixture()
	store := &fakeDreamRunStore{run: run}
	for _, tc := range []struct {
		name       string
		scopes     []string
		failAudit  bool
		emptyAudit bool
		want       int
	}{
		{"missing scope", []string{"dream:read"}, false, false, http.StatusForbidden},
		{"audit failure", []string{"dream:evidence:read"}, true, false, http.StatusInternalServerError},
		{"empty audit reference", []string{"dream:evidence:read"}, false, true, http.StatusInternalServerError},
		{"success", []string{"dream:evidence:read"}, false, false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nx := &evidenceNexus{scopes: tc.scopes, failAudit: tc.failAudit, emptyAudit: tc.emptyAudit}
			allowDreamOrg(nx, "dream:evidence:read", "ent-1", "department:rd")
			evd := &fakeFrozenEvidence{}
			resp := requestDream(t, NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", WorkCaseContextFor: alwaysWorkCaseBacked, Evidence: evd, Nexus: nx, DreamRuns: store}), http.MethodPost, "/v1/dream/runs/run-1/evidence-access", nil)
			if resp.Code != tc.want {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			if tc.want == http.StatusOK {
				// The read must be bound to the located opaque handle, and the
				// freshness disclosure must survive to the caller.
				if len(evd.reads) != 1 || evd.reads[0].EvidenceRef != "evd_0123456789abcdef0123" || len(nx.audits) != 1 || nx.audits[0].Details["decision"] != nexusclient.ReadAllow {
					t.Fatalf("binding/audit = %+v %+v", evd.reads, nx.audits)
				}
				if !bytes.Contains(resp.Body.Bytes(), []byte("served_from_cache")) {
					t.Fatalf("freshness disclosure dropped: %s", resp.Body.String())
				}
			}
			if (tc.failAudit || tc.emptyAudit) && bytes.Contains(resp.Body.Bytes(), []byte("sealed-detail")) {
				t.Fatal("detail leaked before mandatory audit")
			}
		})
	}
}

// TestDreamEvidenceAccessRefusesADeniedRead pins the refusal path, which had no
// coverage because the retired guard tripped on an absent Step Grant long
// before any decision was consulted.
//
// AgentNexus answers a refusal with a SUCCESSFUL 200 carrying decision "deny"
// and no data. The handler must therefore fail closed on the decision itself —
// and must not audit an evidence read that never happened.
func TestDreamEvidenceAccessRefusesADeniedRead(t *testing.T) {
	store := &fakeDreamRunStore{run: dreamRunFixture()}
	nx := &evidenceNexus{scopes: []string{"dream:evidence:read"}}
	allowDreamOrg(nx, "dream:evidence:read", "ent-1", "department:rd")
	evd := &fakeFrozenEvidence{deny: true}
	resp := requestDream(t, NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", WorkCaseContextFor: alwaysWorkCaseBacked, Evidence: evd, Nexus: nx, DreamRuns: store}), http.MethodPost, "/v1/dream/runs/run-1/evidence-access", nil)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("a denied read must not be served: status=%d body=%s", resp.Code, resp.Body.String())
	}
	if len(nx.audits) != 0 {
		t.Fatalf("a denied read was audited as an evidence read: %+v", nx.audits)
	}
}
