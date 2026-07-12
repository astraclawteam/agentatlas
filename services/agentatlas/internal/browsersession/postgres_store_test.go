package browsersession

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectorEncryptsUpstreamCredentialWithDedicatedKey(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	p, err := NewProtector(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := p.Seal("upstream-opaque-token")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "upstream-opaque-token") {
		t.Fatal("credential stored in plaintext")
	}
	plain, err := p.Open(ciphertext)
	if err != nil || plain != "upstream-opaque-token" {
		t.Fatalf("open=%q err=%v", plain, err)
	}
}

func TestBrowserSessionMigrationStoresOnlyHashesAndCiphertext(t *testing.T) {
	path := filepath.Join("..", "..", "db", "migrations", "000010_browser_sessions_and_change_governance.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(string(raw))
	for _, required := range []string{"atlas_browser_login_attempts", "state_hash", "pkce_verifier_ciphertext", "atlas_browser_sessions", "session_hash", "session_family_id", "generation", "successor_hash", "successor_token_ciphertext", "rotation_due_at", "overlap_expires_at", "atlas_browser_logout_operations", "upstream_access_token_ciphertext", "idle_expires_at", "absolute_expires_at", "revoked_at", "request_hash", "audit_ref_id", "published_resource_pointers", "resource_version"} {
		if !strings.Contains(sql, required) {
			t.Errorf("missing %s", required)
		}
	}
	if !strings.Contains(sql, "update change_reviews") || !strings.Contains(sql, "risk_level='low'") {
		t.Fatal("down migration does not transform low-risk upward reviews before restoring the old constraint")
	}
	for _, forbidden := range []string{"session_token text", "access_token text", "state text", "pkce_verifier text"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("plaintext credential column %s", forbidden)
		}
	}
}
