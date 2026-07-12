package browsersession

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionRotationReturnsOneSuccessorAndInvalidatesReplayAfterSuccessorUse(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := NewMemoryStore(clock)
	oidc := &logoutOIDC{}
	svc, err := New(Config{Issuer: "https://nexus", ClientID: "atlas", ClientSecret: "secret", RedirectURI: "https://atlas/auth/callback", RotationInterval: time.Minute, RotationOverlap: 30 * time.Second}, store, oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	old := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := store.CreateSession(context.Background(), old, testIdentity(), "upstream-access-token", 8*time.Hour, 24*time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	first, err := svc.Session(context.Background(), old)
	if err != nil || first.ReplacementToken == "" || first.ReplacementToken == old {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	retry, err := svc.Session(context.Background(), old)
	if err != nil || retry.ReplacementToken != first.ReplacementToken {
		t.Fatalf("lost-response retry=%+v err=%v", retry, err)
	}
	if _, err := svc.Session(context.Background(), first.ReplacementToken); err != nil {
		t.Fatalf("successor rejected: %v", err)
	}
	if _, err := svc.Session(context.Background(), old); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("predecessor replay err=%v", err)
	}
}

func TestLogoutOutboxRetriesAfterCookieAndLocalSessionAreRevoked(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := NewMemoryStore(clock)
	oidc := &logoutOIDC{failures: 1}
	svc, err := New(Config{Issuer: "https://nexus", ClientID: "atlas", ClientSecret: "secret", RedirectURI: "https://atlas/auth/callback"}, store, oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	token := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := store.CreateSession(context.Background(), token, testIdentity(), "upstream-access-token", 8*time.Hour, 24*time.Hour, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(context.Background(), token); err == nil {
		t.Fatal("upstream failure was hidden")
	}
	if _, err := svc.Session(context.Background(), token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("local session not revoked: %v", err)
	}
	if got := store.PendingLogoutCount(); got != 1 {
		t.Fatalf("pending logouts=%d", got)
	}
	if err := svc.ReconcileLogouts(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := store.PendingLogoutCount(); got != 0 || oidc.calls != 2 {
		t.Fatalf("pending=%d calls=%d", got, oidc.calls)
	}
}

func testIdentity() Identity {
	return Identity{EnterpriseID: "ent-1", UserID: "user-1", OrgVersion: 1, OrgUnitIDs: []string{"team"}, Permissions: []string{"suggest"}}
}

type logoutOIDC struct{ failures, calls int }

func (*logoutOIDC) AuthorizationURL(context.Context, AuthorizationRequest) (string, error) {
	return "", nil
}
func (*logoutOIDC) ExchangeAndVerify(context.Context, ExchangeRequest) (ExchangeResult, error) {
	return ExchangeResult{}, nil
}
func (*logoutOIDC) Profile(context.Context, string) (Identity, error) { return Identity{}, nil }
func (o *logoutOIDC) Logout(context.Context, string) error {
	o.calls++
	if o.failures > 0 {
		o.failures--
		return errors.New("upstream audit unavailable")
	}
	return nil
}
