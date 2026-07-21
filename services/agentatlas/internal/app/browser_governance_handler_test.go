package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestBrowserSessionEnrichesAuthorizedOrganizationTreeWithoutGrantingAncestors(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	oidc := &fakeAtlasOIDC{profile: browsersession.Identity{EnterpriseID: "ent-1", UserID: "user-1", DisplayName: "User One", OrgVersion: 7, OrgUnitIDs: []string{"dept-rd"}, Permissions: []string{"knowledge:read"}}}
	sessions, err := browsersession.New(browsersession.Config{Issuer: "https://nexus.example", ClientID: "agentatlas", ClientSecret: "secret", RedirectURI: "https://atlas.example/auth/callback"}, browsersession.NewMemoryStore(clock), oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	orgs := &fakeBrowserOrgStore{
		enterprise: db.Enterprise{ID: "ent-1", Name: "示例企业"},
		spaces: []db.KnowledgeSpace{
			{ID: "space-company", EnterpriseID: "ent-1", Kind: "company", Name: "全公司", OrgScope: "company:root", OrgVersion: 4},
			{ID: "space-rd", EnterpriseID: "ent-1", Kind: "department", Name: "研发一部", OrgScope: "department:dept-rd", OrgVersion: 7},
		},
		bindings: []db.OrgScopeBinding{
			{EnterpriseID: "ent-1", SpaceID: "space-company", ScopeKind: "company", ScopeID: "root"},
			{EnterpriseID: "ent-1", SpaceID: "space-rd", ScopeKind: "department", ScopeID: "dept-rd", ParentScopeKind: pgtype.Text{String: "company", Valid: true}, ParentScopeID: pgtype.Text{String: "root", Valid: true}},
		},
	}
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", Nexus: adminMock(), BrowserSessions: sessions, BrowserOrgStore: orgs})

	login := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/login?return_to=%2Fknowledge", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, login)
	if rr.Code != http.StatusFound || oidc.last.State == "" {
		t.Fatalf("login=%d state=%q body=%s", rr.Code, oidc.last.State, rr.Body.String())
	}
	callback := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/callback?state="+oidc.last.State+"&code=one-use", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, callback)
	if rr.Code != http.StatusFound || len(rr.Result().Cookies()) == 0 {
		t.Fatalf("callback=%d body=%s", rr.Code, rr.Body.String())
	}
	cookie := rr.Result().Cookies()[0]

	me := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/session", nil)
	me.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, me)
	if rr.Code != http.StatusOK {
		t.Fatalf("session=%d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		EnterpriseName string `json:"enterprise_name"`
		OrgTree        []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Selectable bool   `json:"selectable"`
			Children   []struct {
				ID, Name   string
				Selectable bool `json:"selectable"`
			} `json:"children"`
		} `json:"org_tree"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.EnterpriseName != "示例企业" || len(body.OrgTree) != 1 || body.OrgTree[0].Name != "全公司" || body.OrgTree[0].Selectable || len(body.OrgTree[0].Children) != 1 || body.OrgTree[0].Children[0].Name != "研发一部" || !body.OrgTree[0].Children[0].Selectable {
		t.Fatalf("unexpected organization presentation: %+v", body)
	}
}

func TestAdvancedLegacyRoutesAreSessionGuardedAuthorizedAndFailClosed(t *testing.T) {
	for _, path := range []string{"knowledge", "dream", "workflows", "evidence", "assistant"} {
		t.Run(path, func(t *testing.T) {
			router, cookie, authorizer := legacyTestRouter(t, true)
			req := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/legacy/"+path, nil)
			req.AddCookie(cookie)
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("legacy %s status=%d body=%s", path, rr.Code, rr.Body.String())
			}
			if authorizer.calls != 1 || authorizer.last.Action != path+".read" {
				t.Fatalf("legacy %s authorization=%+v calls=%d", path, authorizer.last, authorizer.calls)
			}
		})
	}

	router, cookie, authorizer := legacyTestRouter(t, true)
	upload := httptest.NewRequest(http.MethodPost, "https://atlas.example/api/legacy/assistant/attachments", strings.NewReader("--bounded"))
	upload.Header.Set("Origin", "https://atlas.example")
	upload.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, upload)
	if rr.Code != http.StatusServiceUnavailable || authorizer.last.Action != "assistant.upload" {
		t.Fatalf("legacy upload status=%d auth=%+v body=%s", rr.Code, authorizer.last, rr.Body.String())
	}

	deniedRouter, deniedCookie, deniedAuthorizer := legacyTestRouter(t, false)
	denied := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/legacy/knowledge", nil)
	denied.AddCookie(deniedCookie)
	rr = httptest.NewRecorder()
	deniedRouter.ServeHTTP(rr, denied)
	if rr.Code != http.StatusForbidden || deniedAuthorizer.calls != 0 {
		t.Fatalf("non-advanced legacy status=%d auth calls=%d", rr.Code, deniedAuthorizer.calls)
	}
}

func TestLegacyKnowledgeAuthorizesAndFiltersEveryExactOrganization(t *testing.T) {
	orgs := &fakeBrowserOrgStore{spaces: []db.KnowledgeSpace{
		{ID: "space-a", EnterpriseID: "ent-1", Name: "组织 A", OrgScope: "department:dept-a", OrgVersion: 7},
		{ID: "space-b", EnterpriseID: "ent-1", Name: "组织 B", OrgScope: "department:dept-b", OrgVersion: 7},
	}}
	authorizer := &fakeBrowserAuthorizer{decisions: map[string]nexus.BrowserAuthorizationDecision{
		"dept-a": {Decision: "allow", OrgVersion: 7, OrgUnitIDs: []string{"dept-a"}},
		"dept-b": {Decision: "deny", OrgVersion: 7, OrgUnitIDs: []string{"dept-b"}},
	}}
	router, cookie := legacyRouterForProfile(t, browsersession.Identity{
		EnterpriseID: "ent-1", UserID: "user-1", OrgVersion: 7,
		OrgUnitIDs: []string{"dept-a", "dept-b"}, AdvancedModeAllowed: true,
	}, orgs, authorizer)

	req := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/legacy/knowledge", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "组织 A") || strings.Contains(rr.Body.String(), "组织 B") {
		t.Fatalf("exact organization filter status=%d body=%s", rr.Code, rr.Body.String())
	}
	if authorizer.calls != 2 {
		t.Fatalf("expected one authorization per sealed organization, got %d", authorizer.calls)
	}
}

func TestLegacyAuthorizationRejectsMismatchedDecisionOrganization(t *testing.T) {
	authorizer := &fakeBrowserAuthorizer{decisions: map[string]nexus.BrowserAuthorizationDecision{
		"dept-a": {Decision: "allow", OrgVersion: 7, OrgUnitIDs: []string{"dept-other"}},
	}}
	router, cookie := legacyRouterForProfile(t, browsersession.Identity{
		EnterpriseID: "ent-1", UserID: "user-1", OrgVersion: 7,
		OrgUnitIDs: []string{"dept-a"}, AdvancedModeAllowed: true,
	}, &fakeBrowserOrgStore{}, authorizer)
	req := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/legacy/knowledge", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("mismatched decision organization accepted: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLegacyWorkflowFailsClosedWithoutAuthoritativeOrganizationBinding(t *testing.T) {
	lister := &fakeLegacyWorkflowLister{drafts: []workflow.DraftView{{ID: "wf-company", Name: "全企业流程"}}}
	h := &legacyBrowserHandler{workflows: lister}
	items, err := h.items(context.Background(), browsersession.Session{Identity: browsersession.Identity{
		EnterpriseID: "ent-1", OrgVersion: 7, OrgUnitIDs: []string{"dept-child"},
	}}, "workflows", map[string]bool{"dept-child": true})
	if !errors.Is(err, errLegacyUnavailable) || len(items) != 0 || lister.calls != 0 {
		t.Fatalf("unbound workflow did not fail closed before listing: items=%v err=%v calls=%d", items, err, lister.calls)
	}
}

type fakeLegacyWorkflowLister struct {
	calls  int
	drafts []workflow.DraftView
}

func (f *fakeLegacyWorkflowLister) ListDrafts(context.Context, string, int32) ([]workflow.DraftView, error) {
	f.calls++
	return f.drafts, nil
}

func TestOrganizationPresentationRejectsIdentifierEquivalentNames(t *testing.T) {
	for _, name := range []string{"dept-rd", "department:dept-rd", "space-rd", "DEPARTMENT_DEPT_RD"} {
		t.Run(name, func(t *testing.T) {
			store := &fakeBrowserOrgStore{
				enterprise: db.Enterprise{ID: "ent-1", Name: "企业"},
				spaces:     []db.KnowledgeSpace{{ID: "space-rd", EnterpriseID: "ent-1", Kind: "department", Name: name, OrgScope: "department:dept-rd", OrgVersion: 7}},
				bindings:   []db.OrgScopeBinding{{EnterpriseID: "ent-1", SpaceID: "space-rd", ScopeKind: "department", ScopeID: "dept-rd"}},
			}
			h := &browserSessionHandler{orgs: store}
			_, tree := h.organizationPresentation(context.Background(), browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-1", OrgVersion: 7, OrgUnitIDs: []string{"dept-rd"}}})
			if len(tree) != 1 || tree[0].Name != "未命名组织" || tree[0].Selectable {
				t.Fatalf("identifier-equivalent name leaked: %+v", tree)
			}
		})
	}
}

func legacyTestRouter(t *testing.T, advanced bool) (*chi.Mux, *http.Cookie, *fakeBrowserAuthorizer) {
	t.Helper()
	authorizer := &fakeBrowserAuthorizer{}
	router, cookie := legacyRouterForProfile(t, browsersession.Identity{EnterpriseID: "ent-1", UserID: "user-1", DisplayName: "User One", OrgVersion: 7, OrgUnitIDs: []string{"dept-rd"}, Permissions: []string{"knowledge:read"}, AdvancedModeAllowed: advanced}, nil, authorizer)
	return router, cookie, authorizer
}

func legacyRouterForProfile(t *testing.T, profile browsersession.Identity, orgs browserSessionOrgStore, authorizer *fakeBrowserAuthorizer) (*chi.Mux, *http.Cookie) {
	t.Helper()
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	oidc := &fakeAtlasOIDC{profile: profile}
	sessions, err := browsersession.New(browsersession.Config{Issuer: "https://nexus.example", ClientID: "agentatlas", ClientSecret: "secret", RedirectURI: "https://atlas.example/auth/callback"}, browsersession.NewMemoryStore(clock), oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", Nexus: adminMock(), BrowserSessions: sessions, BrowserAuthorizer: authorizer, BrowserOrgStore: orgs})
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

type fakeBrowserAuthorizer struct {
	calls     int
	last      nexus.BrowserAuthorizationRequest
	decisions map[string]nexus.BrowserAuthorizationDecision
}

func (f *fakeBrowserAuthorizer) AuthorizeBrowserOperation(_ context.Context, _ string, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	f.calls++
	f.last = req
	if f.decisions != nil {
		if decision, ok := f.decisions[req.OrgUnitID]; ok {
			return decision, nil
		}
		return nexus.BrowserAuthorizationDecision{Decision: "deny", OrgVersion: req.OrgVersion, OrgUnitIDs: []string{req.OrgUnitID}}, nil
	}
	return nexus.BrowserAuthorizationDecision{Decision: "allow", OrgVersion: req.OrgVersion, OrgUnitIDs: []string{req.OrgUnitID}}, nil
}
func (*fakeBrowserAuthorizer) AppendAuditEvidenceWithBearer(context.Context, string, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, nil
}

type fakeBrowserOrgStore struct {
	enterprise db.Enterprise
	spaces     []db.KnowledgeSpace
	bindings   []db.OrgScopeBinding
}

func (f *fakeBrowserOrgStore) GetEnterprise(context.Context, string) (db.Enterprise, error) {
	return f.enterprise, nil
}
func (f *fakeBrowserOrgStore) ListBrowserKnowledgeSpacesByEnterprise(context.Context, string) ([]db.KnowledgeSpace, error) {
	return f.spaces, nil
}
func (f *fakeBrowserOrgStore) ListOrgScopeBindingsByEnterprise(context.Context, string) ([]db.OrgScopeBinding, error) {
	return f.bindings, nil
}

func TestBrowserSessionRoutesUseSecureCookieAndCSRF(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	oidc := &fakeAtlasOIDC{profile: browsersession.Identity{EnterpriseID: "ent-1", UserID: "user-1", DisplayName: "User One", OrgVersion: 7, OrgUnitIDs: []string{"team"}, Permissions: []string{"suggest"}}}
	sessions, err := browsersession.New(browsersession.Config{Issuer: "https://nexus.example", ClientID: "agentatlas", ClientSecret: "secret", RedirectURI: "https://atlas.example/auth/callback"}, browsersession.NewMemoryStore(clock), oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	changes := governance.NewService(governance.NewMemoryStore(clock), governance.StaticRouteResolver{ReviewerUserID: "manager", OrgPath: []string{"team", "department"}}, &governance.MemoryAuditAppender{}, governance.NewMemoryPublisher(), clock)
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", Nexus: adminMock(), BrowserSessions: sessions, Changes: changes})
	login := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/login?return_to=%2Fchanges%2Fchg-1", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, login)
	if rr.Code != http.StatusFound || !strings.Contains(rr.Header().Get("Location"), "state=") {
		t.Fatalf("login=%d location=%s", rr.Code, rr.Header().Get("Location"))
	}
	state := oidc.last.State
	callback := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/callback?state="+state+"&code=one-use", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, callback)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/changes/chg-1" {
		t.Fatalf("callback=%d location=%s body=%s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	cookie := rr.Result().Cookies()[0]
	if cookie.Name != "atlas_session" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie=%+v", cookie)
	}
	me := httptest.NewRequest(http.MethodGet, "https://atlas.example/api/session", nil)
	me.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, me)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "User One") {
		t.Fatalf("session=%d %s", rr.Code, rr.Body.String())
	}
	body := `{"org_unit_id":"team","resource_type":"knowledge_entry","resource_id":"kb-1","action":"update","proposed_content":{"title":"fixed"}}`
	bad := httptest.NewRequest(http.MethodPost, "https://atlas.example/api/changes/suggestions", strings.NewReader(body))
	bad.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, bad)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing csrf accepted: %d %s", rr.Code, rr.Body.String())
	}
	good := httptest.NewRequest(http.MethodPost, "https://atlas.example/api/changes/suggestions", strings.NewReader(body))
	good.Header.Set("Origin", "https://atlas.example")
	good.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, good)
	if rr.Code != http.StatusCreated {
		t.Fatalf("suggest=%d %s", rr.Code, rr.Body.String())
	}
	var draft map[string]any
	if json.Unmarshal(rr.Body.Bytes(), &draft) != nil || draft["permission_mode"] != "suggestion_only" {
		t.Fatalf("draft=%v", draft)
	}
	logout := httptest.NewRequest(http.MethodPost, "https://atlas.example/auth/logout", nil)
	logout.Header.Set("Origin", "https://atlas.example")
	logout.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, logout)
	if rr.Code != http.StatusNoContent || oidc.logouts != 1 {
		t.Fatalf("logout=%d upstream=%d", rr.Code, oidc.logouts)
	}
}

func TestSameOriginCSRFAcceptsTLSOriginBehindReverseProxy(t *testing.T) {
	called := false
	handler := sameOriginCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://atlas.internal/api/changes", nil)
	req.Host = "atlas.example"
	req.Header.Set("Origin", "https://atlas.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || !called {
		t.Fatalf("TLS-terminated same origin rejected: status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSameOriginCSRFRejectsAmbiguousForwardedProtocol(t *testing.T) {
	handler := sameOriginCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest(http.MethodPost, "http://atlas.internal/api/changes", nil)
	req.Host = "atlas.example"
	req.Header.Set("Origin", "https://atlas.example")
	req.Header.Add("X-Forwarded-Proto", "https")
	req.Header.Add("X-Forwarded-Proto", "http")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("ambiguous forwarded protocol accepted: %d", rr.Code)
	}
}

type fakeAtlasOIDC struct {
	profile     browsersession.Identity
	last        browsersession.AuthorizationRequest
	logouts     int
	exchangeErr error
	logoutErr   error
}

func (f *fakeAtlasOIDC) AuthorizationURL(_ context.Context, in browsersession.AuthorizationRequest) (string, error) {
	f.last = in
	return "https://nexus.example/oauth2/authorize?state=" + in.State, nil
}
func (f *fakeAtlasOIDC) ExchangeAndVerify(_ context.Context, in browsersession.ExchangeRequest) (browsersession.ExchangeResult, error) {
	if f.exchangeErr != nil {
		return browsersession.ExchangeResult{}, f.exchangeErr
	}
	return browsersession.ExchangeResult{Identity: browsersession.Identity{EnterpriseID: f.profile.EnterpriseID, UserID: f.profile.UserID}, AccessToken: "upstream-access-token", ExpiresAt: time.Date(2026, 7, 12, 10, 5, 0, 0, time.UTC)}, nil
}
func (f *fakeAtlasOIDC) Profile(context.Context, string) (browsersession.Identity, error) {
	return f.profile, nil
}
func (f *fakeAtlasOIDC) Logout(context.Context, string) error { f.logouts++; return f.logoutErr }

func TestBrowserAuthErrorsAreStableAndDoNotLeakInternalCause(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	oidc := &fakeAtlasOIDC{profile: browsersession.Identity{EnterpriseID: "ent-1", UserID: "user-1", OrgVersion: 1, OrgUnitIDs: []string{"team"}, Permissions: []string{"suggest"}}, exchangeErr: errors.New("oidc token endpoint returned 502 with internal topology")}
	sessions, err := browsersession.New(browsersession.Config{Issuer: "https://nexus.example", ClientID: "agentatlas", ClientSecret: "secret", RedirectURI: "https://atlas.example/auth/callback"}, browsersession.NewMemoryStore(clock), oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	router := NewAgentRouter(AgentRouterDeps{OrgAuthorization: &allowOrgAuthorization{}, ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"}, ApprovalAuthority: "oa.example", Nexus: adminMock(), BrowserSessions: sessions})
	invalidLogin := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/login?return_to=%2F%2Fevil.example", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, invalidLogin)
	if rr.Code != http.StatusBadRequest || strings.Contains(rr.Body.String(), "unsafe return_to") || !strings.Contains(rr.Body.String(), "login request is invalid") {
		t.Fatalf("invalid login=%d body=%s", rr.Code, rr.Body.String())
	}
	login := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/login", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, login)
	callback := httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/callback?state="+oidc.last.State+"&code=bad", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, callback)
	if rr.Code != http.StatusUnauthorized || strings.Contains(rr.Body.String(), "topology") || !strings.Contains(rr.Body.String(), "login could not be completed") {
		t.Fatalf("callback=%d body=%s", rr.Code, rr.Body.String())
	}

	oidc.exchangeErr = nil
	oidc.logoutErr = errors.New("audit database connection string leaked")
	login = httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/login", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, login)
	callback = httptest.NewRequest(http.MethodGet, "https://atlas.example/auth/callback?state="+oidc.last.State+"&code=ok", nil)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, callback)
	cookie := rr.Result().Cookies()[0]
	logout := httptest.NewRequest(http.MethodPost, "https://atlas.example/auth/logout", nil)
	logout.Header.Set("Origin", "https://atlas.example")
	logout.AddCookie(cookie)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, logout)
	if rr.Code != http.StatusAccepted || strings.Contains(rr.Body.String(), "connection string") || !strings.Contains(rr.Body.String(), "logout is being completed") {
		t.Fatalf("logout=%d body=%s", rr.Code, rr.Body.String())
	}
}
