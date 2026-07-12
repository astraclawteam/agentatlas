package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/jackc/pgx/v5/pgtype"
)

type fakeDreamRunStore struct {
	run         db.GetDreamRunViewRow
	runs        []db.DreamRun
	annotations []db.CreateDreamAnnotationParams
	listCalls   int
}

func (f *fakeDreamRunStore) GetDreamRunView(_ context.Context, p db.GetDreamRunViewParams) (db.GetDreamRunViewRow, error) {
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

type evidenceNexus struct {
	scopes     []string
	denyRead   bool
	failAudit  bool
	emptyAudit bool
	orgAuth    map[string]bool
	locates    []nexus.LocateEvidenceRequest
	reads      []nexus.ReadEvidenceRequest
	audits     []nexus.AppendAuditEvidenceRequest
}

type fakeDreamRerunner struct{ calls int }

func (f *fakeDreamRerunner) Rerun(context.Context, string, string, string) (string, error) {
	f.calls++
	return "rerun-1", nil
}

func (n *evidenceNexus) VerifyTicket(context.Context, nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	return nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent-1", ActorUserID: "manager-1", Scopes: n.scopes, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (n *evidenceNexus) LocateEvidence(_ context.Context, p nexus.LocateEvidenceRequest) (nexus.LocateEvidenceResponse, error) {
	n.locates = append(n.locates, p)
	if p.ResourceURI != "" {
		if !n.orgAuth[p.QueryIntent+"|"+p.ResourceURI] {
			return nexus.LocateEvidenceResponse{}, nexusclient.ErrDenied
		}
		return nexus.LocateEvidenceResponse{ResourceURI: p.ResourceURI, SourceSystem: "agentatlas"}, nil
	}
	return nexus.LocateEvidenceResponse{ResourceURI: "s3://sealed/run-1"}, nil
}
func (n *evidenceNexus) ReadEvidence(_ context.Context, p nexus.ReadEvidenceRequest) (nexus.ReadEvidenceResponse, error) {
	n.reads = append(n.reads, p)
	if n.denyRead {
		return nexus.ReadEvidenceResponse{}, nexusclient.ErrDenied
	}
	return nexus.ReadEvidenceResponse{GrantID: "grant-bound", ContentType: "text/markdown", SanitizedExcerpt: "sanitized detail", ContentHash: "sha256:abc"}, nil
}
func (n *evidenceNexus) AppendAuditEvidence(_ context.Context, p nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	n.audits = append(n.audits, p)
	if n.failAudit {
		return nexus.AppendAuditEvidenceResponse{}, errors.New("audit down")
	}
	if n.emptyAudit {
		return nexus.AppendAuditEvidenceResponse{}, nil
	}
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
	router := NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store})

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
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store}), http.MethodGet, "/v1/dream/runs/run-1", nil)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
	t.Run("cross org list denied before query", func(t *testing.T) {
		store.listCalls = 0
		nx := &evidenceNexus{scopes: []string{"dream:read"}, orgAuth: map[string]bool{}}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store}), http.MethodGet, "/v1/dream/runs?org_unit_id=department:other", nil)
		if resp.Code != http.StatusForbidden || store.listCalls != 0 {
			t.Fatalf("status=%d listCalls=%d body=%s", resp.Code, store.listCalls, resp.Body.String())
		}
		if len(nx.locates) != 1 || nx.locates[0].EnterpriseID != "ent-1" || nx.locates[0].ResourceURI != dreamOrgResourceURI("ent-1", "department:other") {
			t.Fatalf("authorization binding=%+v", nx.locates)
		}
	})
	t.Run("cross org detail denied", func(t *testing.T) {
		nx := &evidenceNexus{scopes: []string{"dream:read"}, orgAuth: map[string]bool{}}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store}), http.MethodGet, "/v1/dream/runs/run-1", nil)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
	})
	t.Run("cross org annotation denied", func(t *testing.T) {
		store.annotations = nil
		nx := &evidenceNexus{scopes: []string{"dream:annotate"}, orgAuth: map[string]bool{}}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store}), http.MethodPost, "/v1/dream/runs/run-1/annotations", map[string]any{"action": "comment", "comment": "x"})
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
		NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store, DreamRerun: rerunner}).ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden || rerunner.calls != 0 {
			t.Fatalf("status=%d calls=%d", resp.Code, rerunner.calls)
		}
	})
	t.Run("cross org evidence denied before grant", func(t *testing.T) {
		nx := &evidenceNexus{scopes: []string{"dream:evidence:read"}, orgAuth: map[string]bool{}}
		resp := requestDream(t, NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store}), http.MethodPost, "/v1/dream/runs/run-1/evidence-access", nil)
		if resp.Code != http.StatusForbidden || len(nx.reads) != 0 {
			t.Fatalf("status=%d reads=%v", resp.Code, nx.reads)
		}
	})
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
			resp := requestDream(t, NewAgentRouter(AgentRouterDeps{Nexus: nx, DreamRuns: store}), http.MethodPost, "/v1/dream/runs/run-1/evidence-access", nil)
			if resp.Code != tc.want {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			if tc.want == http.StatusOK {
				if len(nx.reads) != 1 || nx.reads[0].EvidencePointerID != "ev-sealed" || len(nx.audits) != 1 || nx.audits[0].Details["grant_id"] != "grant-bound" || !bytes.Contains(resp.Body.Bytes(), []byte("sanitized detail")) {
					t.Fatalf("binding/audit/response = %+v %+v %s", nx.reads, nx.audits, resp.Body.String())
				}
			}
			if (tc.failAudit || tc.emptyAudit) && bytes.Contains(resp.Body.Bytes(), []byte("sanitized detail")) {
				t.Fatal("detail leaked before mandatory audit")
			}
		})
	}
}
