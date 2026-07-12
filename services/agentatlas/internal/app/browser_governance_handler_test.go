package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

func TestBrowserSessionRoutesUseSecureCookieAndCSRF(t *testing.T) {
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	oidc := &fakeAtlasOIDC{profile: browsersession.Identity{EnterpriseID: "ent-1", UserID: "user-1", DisplayName: "User One", OrgVersion: 7, OrgUnitIDs: []string{"team"}, Permissions: []string{"suggest"}}}
	sessions, err := browsersession.New(browsersession.Config{Issuer: "https://nexus.example", ClientID: "agentatlas", ClientSecret: "secret", RedirectURI: "https://atlas.example/auth/callback"}, browsersession.NewMemoryStore(clock), oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	changes := governance.NewService(governance.NewMemoryStore(clock), governance.StaticRouteResolver{ReviewerUserID: "manager", OrgPath: []string{"team", "department"}}, &governance.MemoryAuditAppender{}, governance.NewMemoryPublisher(), clock)
	router := NewAgentRouter(AgentRouterDeps{Nexus: adminMock(), BrowserSessions: sessions, Changes: changes})
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

type fakeAtlasOIDC struct {
	profile browsersession.Identity
	last    browsersession.AuthorizationRequest
	logouts int
}

func (f *fakeAtlasOIDC) AuthorizationURL(_ context.Context, in browsersession.AuthorizationRequest) (string, error) {
	f.last = in
	return "https://nexus.example/oauth2/authorize?state=" + in.State, nil
}
func (f *fakeAtlasOIDC) ExchangeAndVerify(_ context.Context, in browsersession.ExchangeRequest) (browsersession.ExchangeResult, error) {
	return browsersession.ExchangeResult{Identity: browsersession.Identity{EnterpriseID: f.profile.EnterpriseID, UserID: f.profile.UserID}, AccessToken: "upstream-access-token", ExpiresAt: time.Date(2026, 7, 12, 10, 5, 0, 0, time.UTC)}, nil
}
func (f *fakeAtlasOIDC) Profile(context.Context, string) (browsersession.Identity, error) {
	return f.profile, nil
}
func (f *fakeAtlasOIDC) Logout(context.Context, string) error { f.logouts++; return nil }
