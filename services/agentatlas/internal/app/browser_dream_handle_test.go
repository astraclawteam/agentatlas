package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
)

func TestBrowserDreamHandleIsOpaqueRestartSafeAndStrictlyBound(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	protector, err := browsersession.NewProtector(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	issuer := newBrowserDreamHandleCodec(protector, func() time.Time { return now })
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent-secret", UserID: "user-1"}, FamilyID: "family-secret"}
	handle, err := issuer.issue(session, "run", "org-secret", "run-secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ent-secret", "family-secret", "org-secret", "run-secret"} {
		if strings.Contains(handle, secret) {
			t.Fatalf("handle exposes %q: %s", secret, handle)
		}
	}

	// A new codec instance models another process after restart using the same rotated keyring.
	resolver := newBrowserDreamHandleCodec(protector, func() time.Time { return now.Add(time.Minute) })
	claim, err := resolver.resolve(session, "run", handle)
	if err != nil || claim.ResourceID != "run-secret" || claim.OrgUnitID != "org-secret" {
		t.Fatalf("cross-instance resolve claim=%+v err=%v", claim, err)
	}

	otherSession := session
	otherSession.FamilyID = "family-other"
	otherTenant := session
	otherTenant.EnterpriseID = "ent-other"
	tamperAt := len(handle) / 2
	replacement := "A"
	if handle[tamperAt] == 'A' {
		replacement = "B"
	}
	for name, candidate := range map[string]struct {
		session browsersession.Session
		kind    string
		token   string
	}{
		"session":      {otherSession, "run", handle},
		"tenant":       {otherTenant, "run", handle},
		"kind":         {session, "policy", handle},
		"substitution": {session, "run", handle[:tamperAt] + replacement + handle[tamperAt+1:]},
	} {
		if _, err := resolver.resolve(candidate.session, candidate.kind, candidate.token); err == nil {
			t.Fatalf("%s substitution was accepted", name)
		}
	}

	expired := newBrowserDreamHandleCodec(protector, func() time.Time { return now.Add(browserDreamHandleTTL + time.Second) })
	if _, err := expired.resolve(session, "run", handle); err == nil {
		t.Fatal("expired handle was accepted")
	}
}

func TestBrowserDreamHandleFailsClosedWithoutSessionFamily(t *testing.T) {
	protector, _ := browsersession.NewProtector(bytes.Repeat([]byte{8}, 32))
	codec := newBrowserDreamHandleCodec(protector, time.Now)
	session := browsersession.Session{Identity: browsersession.Identity{EnterpriseID: "ent", UserID: "same-user"}}
	if _, err := codec.issue(session, "run", "org", "run"); err == nil {
		t.Fatal("handle issued without a session family")
	}
	withFamily := session
	withFamily.FamilyID = "family"
	handle, err := codec.issue(withFamily, "run", "org", "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.resolve(session, "run", handle); err == nil {
		t.Fatal("handle resolved without a session family")
	}
}
