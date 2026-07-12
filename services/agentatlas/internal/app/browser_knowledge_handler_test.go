package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestBrowserKnowledgeListUsesExactAuthorizedOrganizationAndBoundedSearch(t *testing.T) {
	items := &fakeBrowserKnowledgeStore{items: []db.ListBrowserKnowledgeItemsRow{{
		ID: "node-1", SummaryText: "MES 异常工单处理", SourceType: "sop_change",
		NodeTime: pgtype.Timestamptz{Time: time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC), Valid: true}, ScopeName: "研发一部",
	}}}
	orgs := &fakeBrowserOrgStore{spaces: []db.KnowledgeSpace{{
		ID: "space-rd", EnterpriseID: "ent-1", Name: "研发一部", Kind: "department", OrgScope: "department:dept-rd", OrgVersion: 7,
	}}}
	authorizer := &fakeBrowserAuthorizer{decisions: map[string]nexus.BrowserAuthorizationDecision{
		"dept-rd": {Decision: "allow", OrgVersion: 7, OrgUnitIDs: []string{"dept-rd"}},
	}}
	changes := governance.NewService(governance.NewMemoryStore(func() time.Time { return time.Now().UTC() }), governance.StaticRouteResolver{}, &governance.MemoryAuditAppender{}, governance.NewMemoryPublisher(), time.Now)
	router, cookie := browserKnowledgeRouter(t, orgs, items, authorizer, changes)

	req := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/knowledge?org_unit_id=dept-rd&query=MES", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("knowledge status=%d body=%s", rr.Code, rr.Body.String())
	}
	if authorizer.calls != 1 || authorizer.last.OrgUnitID != "dept-rd" || authorizer.last.OrgVersion != 7 || authorizer.last.ResourceType != "knowledge_space" || authorizer.last.Action != "knowledge.read" {
		t.Fatalf("authorization=%+v calls=%d", authorizer.last, authorizer.calls)
	}
	if items.last.EnterpriseID != "ent-1" || items.last.SpaceID != "space-rd" || items.last.OrgScope != "department:dept-rd" || items.last.SearchQuery != "MES" || items.last.ResultLimit != 101 {
		t.Fatalf("query not exactly scoped and bounded: %+v", items.last)
	}
	var body struct {
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
		Items []struct {
			Title      string `json:"title"`
			TypeLabel  string `json:"type_label"`
			ScopeLabel string `json:"scope_label"`
		} `json:"items"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &body) != nil || body.Organization.Name != "研发一部" || len(body.Items) != 1 || body.Items[0].Title != "MES 异常工单处理" || body.Items[0].TypeLabel != "SOP" || body.Items[0].ScopeLabel != "研发一部" {
		t.Fatalf("unexpected presentation: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "ent-1") || strings.Contains(rr.Body.String(), "department:dept-rd") {
		t.Fatalf("internal identifier leaked: %s", rr.Body.String())
	}
}

func TestBrowserKnowledgeFailsClosedForWrongScopeDecisionAndOversizedResults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision nexus.BrowserAuthorizationDecision
		items    []db.ListBrowserKnowledgeItemsRow
		want     int
	}{
		{name: "mismatched decision", decision: nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: 7, OrgUnitIDs: []string{"dept-other"}}, want: http.StatusForbidden},
		{name: "oversized", decision: nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: 7, OrgUnitIDs: []string{"dept-rd"}}, items: make([]db.ListBrowserKnowledgeItemsRow, 101), want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orgs := &fakeBrowserOrgStore{spaces: []db.KnowledgeSpace{{ID: "space-rd", EnterpriseID: "ent-1", Name: "研发一部", OrgScope: "department:dept-rd", OrgVersion: 7}}}
			store := &fakeBrowserKnowledgeStore{items: tc.items}
			authorizer := &fakeBrowserAuthorizer{decisions: map[string]nexus.BrowserAuthorizationDecision{"dept-rd": tc.decision}}
			router, cookie := browserKnowledgeRouter(t, orgs, store, authorizer, nil)
			req := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/knowledge?org_unit_id=dept-rd", nil)
			req.AddCookie(cookie)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestBrowserKnowledgeOrganizationNameNeverFallsBackToAnIdentifier(t *testing.T) {
	space := db.KnowledgeSpace{ID: "space-rd", Name: "dept-rd", OrgScope: "department:dept-rd"}
	if got := safeKnowledgeSpaceName(space, "dept-rd"); got != "未命名组织" {
		t.Fatalf("identifier-equivalent organization name leaked: %q", got)
	}
}

func TestBrowserKnowledgeFailsClosedWhenOrganizationScanExceedsBound(t *testing.T) {
	spaces := make([]db.KnowledgeSpace, 1001)
	for i := range spaces {
		spaces[i] = db.KnowledgeSpace{ID: "space", EnterpriseID: "ent-1", Name: "组织", OrgScope: "department:other", OrgVersion: 7}
	}
	spaces[0] = db.KnowledgeSpace{ID: "space-rd", EnterpriseID: "ent-1", Name: "研发一部", OrgScope: "department:dept-rd", OrgVersion: 7}
	authorizer := &fakeBrowserAuthorizer{decisions: map[string]nexus.BrowserAuthorizationDecision{"dept-rd": {Decision: "allow", OrgVersion: 7, OrgUnitIDs: []string{"dept-rd"}}}}
	router, cookie := browserKnowledgeRouter(t, &fakeBrowserOrgStore{spaces: spaces}, &fakeBrowserKnowledgeStore{}, authorizer, nil)
	req := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/knowledge?org_unit_id=dept-rd", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("oversized organization scan did not fail closed: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

type fakeBrowserKnowledgeStore struct {
	items []db.ListBrowserKnowledgeItemsRow
	last  db.ListBrowserKnowledgeItemsParams
}

func (f *fakeBrowserKnowledgeStore) ListBrowserKnowledgeItems(_ context.Context, arg db.ListBrowserKnowledgeItemsParams) ([]db.ListBrowserKnowledgeItemsRow, error) {
	f.last = arg
	return f.items, nil
}

func browserKnowledgeRouter(t *testing.T, orgs browserSessionOrgStore, knowledge browserKnowledgeStore, authorizer *fakeBrowserAuthorizer, changes *governance.Service) (*chi.Mux, *http.Cookie) {
	t.Helper()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	oidc := &fakeAtlasOIDC{profile: browsersession.Identity{EnterpriseID: "ent-1", UserID: "user-1", DisplayName: "陈经理", OrgVersion: 7, OrgUnitIDs: []string{"dept-rd"}, Permissions: []string{"knowledge:read", "approve_high_risk"}}}
	sessions, err := browsersession.New(browsersession.Config{Issuer: "https://nexus.example", ClientID: "agentatlas", ClientSecret: "secret", RedirectURI: "https://atlas.example/auth/callback"}, browsersession.NewMemoryStore(func() time.Time { return now }), oidc, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	router := NewAgentRouter(AgentRouterDeps{Nexus: adminMock(), BrowserSessions: sessions, BrowserOrgStore: orgs, BrowserKnowledgeStore: knowledge, BrowserAuthorizer: authorizer, Changes: changes})
	login := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/login?return_to=%2Fknowledge", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, login)
	callback := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/callback?state="+oidc.last.State+"&code=one-use", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, callback)
	if rr.Code != http.StatusFound || len(rr.Result().Cookies()) == 0 {
		t.Fatalf("callback=%d %s", rr.Code, rr.Body.String())
	}
	return router, rr.Result().Cookies()[0]
}
