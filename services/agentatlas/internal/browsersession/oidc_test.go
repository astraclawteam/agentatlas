package browsersession

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOIDCExchangeVerifiesJWKSClaimsAndReturnsServerCredential(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/oauth2/authorize", "token_endpoint": issuer + "/oauth2/token", "jwks_uri": issuer + "/oauth2/jwks"})
	})
	mux.HandleFunc("/oauth2/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		client, secret, ok := r.BasicAuth()
		_ = r.ParseForm()
		if !ok || client != "agentatlas" || secret != "secret" || r.Form.Get("code_verifier") != "verifier" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		claims := map[string]any{"iss": issuer, "sub": "user-1", "aud": "agentatlas", "exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(), "nonce": "nonce", "enterprise_id": "ent-1", "enterprise_user_id": "user-1"}
		writeTestJSON(w, map[string]any{"id_token": signTestJWT(t, key, "k1", claims), "access_token": "upstream-opaque-token", "token_type": "Bearer", "expires_in": 300})
	})
	mux.HandleFunc("/v1/browser-sessions/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-opaque-token" {
			http.Error(w, "denied", 401)
			return
		}
		writeTestJSON(w, map[string]any{"authenticated": true, "enterprise_id": "ent-1", "enterprise_user_id": "user-1", "display_name": "User One", "org_version": 7, "org_unit_ids": []string{"team"}, "permissions": []string{"suggest"}})
	})
	mux.HandleFunc("/v1/browser-sessions/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-opaque-token" {
			http.Error(w, "denied", 401)
			return
		}
		w.WriteHeader(204)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	issuer = server.URL
	client := NewOIDCClient(server.Client(), issuer, func() time.Time { return now })
	result, err := client.ExchangeAndVerify(context.Background(), ExchangeRequest{Code: "one-use", Verifier: "verifier", Nonce: "nonce", RedirectURI: "https://atlas.example/auth/callback", ClientID: "agentatlas", ClientSecret: "secret"})
	if err != nil || result.Identity.EnterpriseID != "ent-1" || result.AccessToken != "upstream-opaque-token" {
		t.Fatalf("exchange result=%+v err=%v", result, err)
	}
	profile, err := client.Profile(context.Background(), result.AccessToken)
	if err != nil || profile.DisplayName != "User One" {
		t.Fatalf("profile=%+v err=%v", profile, err)
	}
	if err := client.Logout(context.Background(), result.AccessToken); err != nil {
		t.Fatal(err)
	}
}

func TestSafeReturnToRejectsOriginsAndBackslashes(t *testing.T) {
	for _, v := range []string{"https://evil.example/x", "//evil.example/x", "/\\evil", "/ok\r\nX: y", ""} {
		if SafeReturnTo(v) {
			t.Errorf("accepted %q", v)
		}
	}
	for _, v := range []string{"/", "/changes/chg-1?tab=diff"} {
		if !SafeReturnTo(v) {
			t.Errorf("rejected %q", v)
		}
	}
}

func signTestJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}
func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
