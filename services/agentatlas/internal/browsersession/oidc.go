package browsersession

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OIDCClient struct {
	http   *http.Client
	issuer string
	now    func() time.Time
}

func NewOIDCClient(client *http.Client, issuer string, now func() time.Time) *OIDCClient {
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	return &OIDCClient{http: client, issuer: strings.TrimRight(issuer, "/"), now: now}
}

type discoveryDocument struct {
	Issuer, AuthorizationEndpoint, TokenEndpoint, JWKSURI string
}

func (d *discoveryDocument) UnmarshalJSON(raw []byte) error {
	var v struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		JWKSURI               string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	d.Issuer, d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI = v.Issuer, v.AuthorizationEndpoint, v.TokenEndpoint, v.JWKSURI
	return nil
}

type tokenResponse struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}
type idClaims struct {
	Issuer           string `json:"iss"`
	Subject          string `json:"sub"`
	Audience         any    `json:"aud"`
	ExpiresAt        int64  `json:"exp"`
	IssuedAt         int64  `json:"iat"`
	Nonce            string `json:"nonce"`
	EnterpriseID     string `json:"enterprise_id"`
	EnterpriseUserID string `json:"enterprise_user_id"`
}

func (c *OIDCClient) discover(ctx context.Context) (discoveryDocument, error) {
	var d discoveryDocument
	if err := c.getJSON(ctx, c.issuer+"/.well-known/openid-configuration", "", &d); err != nil {
		return d, err
	}
	if d.Issuer != c.issuer || !sameOrigin(d.AuthorizationEndpoint, c.issuer) || !sameOrigin(d.TokenEndpoint, c.issuer) || !sameOrigin(d.JWKSURI, c.issuer) {
		return d, errors.New("oidc discovery issuer/endpoint mismatch")
	}
	return d, nil
}

func sameOrigin(endpoint, issuer string) bool {
	e, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	i, err := url.Parse(issuer)
	return err == nil && e.Scheme != "" && e.Scheme == i.Scheme && e.Host == i.Host
}
func (c *OIDCClient) AuthorizationURL(ctx context.Context, in AuthorizationRequest) (string, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{"response_type": {"code"}, "client_id": {in.ClientID}, "redirect_uri": {in.RedirectURI}, "scope": {"openid profile"}, "state": {in.State}, "nonce": {in.Nonce}, "code_challenge": {in.CodeChallenge}, "code_challenge_method": {"S256"}}
	return d.AuthorizationEndpoint + "?" + q.Encode(), nil
}

func (c *OIDCClient) ExchangeAndVerify(ctx context.Context, in ExchangeRequest) (ExchangeResult, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return ExchangeResult{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {in.Code}, "code_verifier": {in.Verifier}, "redirect_uri": {in.RedirectURI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return ExchangeResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(in.ClientID, in.ClientSecret)
	resp, err := c.http.Do(req)
	if err != nil {
		return ExchangeResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ExchangeResult{}, fmt.Errorf("oidc token status %d", resp.StatusCode)
	}
	var token tokenResponse
	if err := decodeBounded(resp.Body, &token); err != nil {
		return ExchangeResult{}, err
	}
	if token.TokenType != "Bearer" || len(token.AccessToken) < 16 || len(token.AccessToken) > 4096 || token.ExpiresIn <= 0 || token.ExpiresIn > 86400 {
		return ExchangeResult{}, errors.New("oidc invalid token response")
	}
	claims, err := c.verifyIDToken(ctx, d.JWKSURI, token.IDToken, in.ClientID, in.Nonce)
	if err != nil {
		return ExchangeResult{}, err
	}
	return ExchangeResult{Identity: Identity{EnterpriseID: claims.EnterpriseID, UserID: claims.EnterpriseUserID}, AccessToken: token.AccessToken, ExpiresAt: c.now().Add(time.Duration(token.ExpiresIn) * time.Second)}, nil
}

func (c *OIDCClient) verifyIDToken(ctx context.Context, jwksURI, raw, audience, nonce string) (idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idClaims{}, errors.New("oidc malformed id token")
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return idClaims{}, err
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(headerRaw, &header) != nil || header.Alg != "RS256" || header.Kid == "" {
		return idClaims{}, errors.New("oidc unsupported id token header")
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := c.getJSON(ctx, jwksURI, "", &set); err != nil {
		return idClaims{}, err
	}
	if len(set.Keys) == 0 || len(set.Keys) > 10 {
		return idClaims{}, errors.New("oidc invalid JWKS size")
	}
	var pub *rsa.PublicKey
	for _, k := range set.Keys {
		if k.Kid != header.Kid || k.Kty != "RSA" || k.Alg != "RS256" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, nerr := base64.RawURLEncoding.DecodeString(k.N)
		e, eerr := base64.RawURLEncoding.DecodeString(k.E)
		if nerr != nil || eerr != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		ev := 0
		for _, b := range e {
			ev = ev<<8 | int(b)
		}
		if ev >= 3 {
			pub = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ev}
			break
		}
	}
	if pub == nil {
		return idClaims{}, errors.New("oidc signing key unavailable")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return idClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) != nil {
		return idClaims{}, errors.New("oidc invalid signature")
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return idClaims{}, err
	}
	var claims idClaims
	if json.Unmarshal(claimsRaw, &claims) != nil {
		return idClaims{}, errors.New("oidc invalid claims")
	}
	now := c.now().Unix()
	if claims.Issuer != c.issuer || claims.Subject == "" || claims.Subject != claims.EnterpriseUserID || claims.EnterpriseID == "" || claims.Nonce != nonce || claims.ExpiresAt <= now || claims.ExpiresAt > now+600 || claims.IssuedAt > now+60 || claims.IssuedAt < now-600 || !audienceContains(claims.Audience, audience) {
		return idClaims{}, errors.New("oidc rejected claims")
	}
	return claims, nil
}

func audienceContains(v any, want string) bool {
	switch a := v.(type) {
	case string:
		return a == want
	case []any:
		for _, item := range a {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
func (c *OIDCClient) Profile(ctx context.Context, access string) (Identity, error) {
	var p struct {
		Authenticated       bool     `json:"authenticated"`
		EnterpriseID        string   `json:"enterprise_id"`
		EnterpriseUserID    string   `json:"enterprise_user_id"`
		DisplayName         string   `json:"display_name"`
		OrgVersion          int64    `json:"org_version"`
		OrgUnitIDs          []string `json:"org_unit_ids"`
		Permissions         []string `json:"permissions"`
		AdvancedModeAllowed bool     `json:"advanced_mode_allowed"`
	}
	if err := c.getJSON(ctx, c.issuer+"/v1/browser-sessions/me", access, &p); err != nil {
		return Identity{}, err
	}
	if !p.Authenticated || !validIdentity(Identity{EnterpriseID: p.EnterpriseID, UserID: p.EnterpriseUserID, DisplayName: p.DisplayName, OrgVersion: p.OrgVersion, OrgUnitIDs: p.OrgUnitIDs, Permissions: p.Permissions}) {
		return Identity{}, errors.New("oidc invalid browser profile")
	}
	return Identity{EnterpriseID: p.EnterpriseID, UserID: p.EnterpriseUserID, DisplayName: p.DisplayName, OrgVersion: p.OrgVersion, OrgUnitIDs: p.OrgUnitIDs, Permissions: p.Permissions, AdvancedModeAllowed: p.AdvancedModeAllowed}, nil
}

func validIdentity(id Identity) bool {
	if id.EnterpriseID == "" || id.UserID == "" || id.OrgVersion < 1 || len(id.OrgUnitIDs) > 1000 || len(id.Permissions) > 100 {
		return false
	}
	for _, values := range [][]string{id.OrgUnitIDs, id.Permissions} {
		seen := map[string]bool{}
		for _, v := range values {
			if v == "" || len(v) > 256 || seen[v] {
				return false
			}
			seen[v] = true
		}
	}
	return true
}
func (c *OIDCClient) Logout(ctx context.Context, access string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer+"/v1/browser-sessions/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("oidc logout status %d", resp.StatusCode)
	}
	return nil
}
func (c *OIDCClient) getJSON(ctx context.Context, endpoint, access string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("oidc get status %d", resp.StatusCode)
	}
	return decodeBounded(resp.Body, out)
}
func decodeBounded(r io.Reader, out any) error {
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(out)
}
