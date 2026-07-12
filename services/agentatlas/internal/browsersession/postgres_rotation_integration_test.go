package browsersession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresSessionRotationKeyRolloverAndLogoutOutbox(t *testing.T) {
	dsn := os.Getenv("ATLAS_TASK8_SESSION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ATLAS_TASK8_SESSION_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	enterpriseID := "ent-session-" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Session Integration')`, enterpriseID); err != nil {
		t.Fatal(err)
	}
	oldKey := EncryptionKey{ID: "2026-06", Key: bytes.Repeat([]byte{3}, 32)}
	newKey := EncryptionKey{ID: "2026-07", Key: bytes.Repeat([]byte{4}, 32)}
	oldProtector, _ := NewProtectorKeyring(oldKey)
	oldStore, _ := NewPostgresStore(pool, oldProtector, clock)
	token := "atlas-session-token-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	id := testIdentity()
	id.EnterpriseID = enterpriseID
	if err := oldStore.CreateSession(ctx, token, id, "upstream-access-token-123456", 8*time.Hour, 24*time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
	rotatedProtector, _ := NewProtectorKeyring(newKey, oldKey)
	store, _ := NewPostgresStore(pool, rotatedProtector, clock)
	now = now.Add(time.Minute)
	first, err := store.ResolveSession(ctx, token, 8*time.Hour, time.Minute, 30*time.Second)
	if err != nil || first.ReplacementToken == "" || first.UpstreamAccessToken != "upstream-access-token-123456" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	retry, err := store.ResolveSession(ctx, token, 8*time.Hour, time.Minute, 30*time.Second)
	if err != nil || retry.ReplacementToken != first.ReplacementToken {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	if _, err := store.ResolveSession(ctx, first.ReplacementToken, 8*time.Hour, time.Minute, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveSession(ctx, token, 8*time.Hour, time.Minute, 30*time.Second); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("predecessor replay err=%v", err)
	}
	op, err := store.BeginLogout(ctx, first.ReplacementToken)
	if err != nil || op.UpstreamAccessToken != "upstream-access-token-123456" {
		t.Fatalf("logout op=%+v err=%v", op, err)
	}
	var rawCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM atlas_browser_logout_operations WHERE position('upstream-access-token' in encode(upstream_access_token_ciphertext,'escape'))>0`).Scan(&rawCount); err != nil || rawCount != 0 {
		t.Fatalf("plaintext outbox credentials=%d err=%v", rawCount, err)
	}
	if err := store.CompleteLogout(ctx, op.SessionHash); err != nil {
		t.Fatal(err)
	}
	var pending, credentials int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM atlas_browser_logout_operations`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM atlas_browser_sessions WHERE session_family_id=$1 AND upstream_access_token_ciphertext IS NOT NULL`, first.FamilyID).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || credentials != 0 {
		t.Fatalf("pending=%d retained credentials=%d", pending, credentials)
	}
	unknownKeyToken := "atlas-session-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := oldStore.CreateSession(ctx, unknownKeyToken, id, "upstream-access-token-unknown-key", 8*time.Hour, 24*time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
	newOnlyProtector, _ := NewProtectorKeyring(newKey)
	newOnlyStore, _ := NewPostgresStore(pool, newOnlyProtector, clock)
	if _, err := newOnlyStore.BeginLogout(ctx, unknownKeyToken); err == nil {
		t.Fatal("logout unexpectedly decrypted a credential without its key")
	}
	var revoked bool
	if err := pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM atlas_browser_sessions WHERE session_hash=$1`, hash(unknownKeyToken)).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM atlas_browser_logout_operations WHERE session_hash=$1`, hash(unknownKeyToken)).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !revoked || pending != 1 {
		t.Fatalf("decrypt failure rolled back revoke-wins state: revoked=%v pending=%d", revoked, pending)
	}
	validToken := "atlas-session-token-cccccccccccccccccccccccccccccccc"
	if err := newOnlyStore.CreateSession(ctx, validToken, id, "upstream-access-token-valid-key", 8*time.Hour, 24*time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := newOnlyStore.BeginLogout(ctx, validToken); err != nil {
		t.Fatal(err)
	}
	operations, pendingErr := newOnlyStore.PendingLogouts(ctx, 10)
	if pendingErr == nil || len(operations) != 1 || operations[0].UpstreamAccessToken != "upstream-access-token-valid-key" {
		t.Fatalf("pending operations=%+v err=%v", operations, pendingErr)
	}
	now = now.Add(11 * time.Second)
	oidc := &logoutOIDC{}
	svc, err := New(Config{Issuer: "https://nexus", ClientID: "atlas", ClientSecret: "secret", RedirectURI: "https://atlas/auth/callback"}, newOnlyStore, oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ReconcileLogouts(ctx, 10); err != nil {
		t.Fatalf("quarantined operation remained in retry queue: %v", err)
	}
	if oidc.calls != 1 {
		t.Fatalf("poison logout blocked decryptable operations: calls=%d", oidc.calls)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM atlas_browser_logout_operations WHERE session_hash=$1`, hash(validToken)).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("decryptable logout remained pending=%d", pending)
	}
}

func TestPostgresLogoutReconcilerQuarantinesFullPoisonBatchAndReachesNewerOperation(t *testing.T) {
	dsn := os.Getenv("ATLAS_TASK8_SESSION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("ATLAS_TASK8_SESSION_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	enterpriseID := "ent-logout-quarantine-" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Logout Quarantine Integration')`, enterpriseID); err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	identity.EnterpriseID = enterpriseID
	tokenRunID := strings.TrimPrefix(enterpriseID, "ent-logout-quarantine-")
	retiredProtector, _ := NewProtectorKeyring(EncryptionKey{ID: "retired", Key: bytes.Repeat([]byte{7}, 32)})
	currentProtector, _ := NewProtectorKeyring(EncryptionKey{ID: "current", Key: bytes.Repeat([]byte{8}, 32)})
	retiredStore, _ := NewPostgresStore(pool, retiredProtector, clock)
	currentStore, _ := NewPostgresStore(pool, currentProtector, clock)

	for i := 0; i < 100; i++ {
		token := fmt.Sprintf("atlas-session-poison-%s-%03d-aaaaaaaaaaaaaaaa", tokenRunID, i)
		if err := retiredStore.CreateSession(ctx, token, identity, "retired-upstream-token", 8*time.Hour, 24*time.Hour, time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, err := currentStore.BeginLogout(ctx, token); err == nil {
			t.Fatalf("poison logout %d unexpectedly decrypted", i)
		}
	}
	now = now.Add(time.Second)
	validToken := "atlas-session-newer-valid-" + tokenRunID + "-dddddddddddddddd"
	if err := currentStore.CreateSession(ctx, validToken, identity, "valid-upstream-token", 8*time.Hour, 24*time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := currentStore.BeginLogout(ctx, validToken); err != nil {
		t.Fatal(err)
	}

	oidc := &logoutOIDC{}
	svc, err := New(Config{Issuer: "https://nexus", ClientID: "atlas", ClientSecret: "secret", RedirectURI: "https://atlas/auth/callback"}, currentStore, oidc, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ReconcileLogouts(ctx, 100); err == nil {
		t.Fatal("poison quarantine errors were hidden")
	}
	foundValid := false
	for _, token := range oidc.tokens {
		if token == "valid-upstream-token" {
			foundValid = true
			break
		}
	}
	if !foundValid {
		t.Fatalf("full poison batch starved newer valid logout: tokens=%v", oidc.tokens)
	}

	var quarantined, retainedCiphertexts, revokedPoison, validPending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM atlas_browser_logout_operations o JOIN atlas_browser_sessions s USING(session_hash) WHERE s.enterprise_id=$1 AND o.quarantine_reason='credential_decrypt_failed' AND o.quarantined_at IS NOT NULL`, enterpriseID).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM atlas_browser_logout_operations o JOIN atlas_browser_sessions s USING(session_hash) WHERE s.enterprise_id=$1 AND o.quarantine_reason='credential_decrypt_failed' AND o.upstream_access_token_ciphertext IS NOT NULL`, enterpriseID).Scan(&retainedCiphertexts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM atlas_browser_sessions WHERE enterprise_id=$1 AND revoked_at IS NOT NULL`, enterpriseID).Scan(&revokedPoison); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM atlas_browser_logout_operations WHERE session_hash=$1`, hash(validToken)).Scan(&validPending); err != nil {
		t.Fatal(err)
	}
	if quarantined != 100 || retainedCiphertexts != 100 || revokedPoison != 101 || validPending != 0 {
		t.Fatalf("quarantined=%d retained=%d revoked=%d valid_pending=%d", quarantined, retainedCiphertexts, revokedPoison, validPending)
	}
}
