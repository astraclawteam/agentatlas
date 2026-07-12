package browsersession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	Issuer, ClientID, ClientSecret, RedirectURI string
	IdleTimeout, AbsoluteTimeout                time.Duration
	RotationInterval, RotationOverlap           time.Duration
}

type OIDC interface {
	AuthorizationURL(context.Context, AuthorizationRequest) (string, error)
	ExchangeAndVerify(context.Context, ExchangeRequest) (ExchangeResult, error)
	Profile(context.Context, string) (Identity, error)
	Logout(context.Context, string) error
}

type AuthorizationRequest struct{ State, Nonce, CodeChallenge, RedirectURI, ClientID string }
type ExchangeRequest struct{ Code, Verifier, Nonce, RedirectURI, ClientID, ClientSecret string }
type ExchangeResult struct {
	Identity    Identity
	AccessToken string
	ExpiresAt   time.Time
}

type Service struct {
	cfg   Config
	store Store
	oidc  OIDC
	now   func() time.Time
}

func New(cfg Config, store Store, oidc OIDC, now func() time.Time) (*Service, error) {
	if store == nil || oidc == nil || cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURI == "" {
		return nil, errors.New("browser session: incomplete configuration")
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 8 * time.Hour
	}
	if cfg.AbsoluteTimeout == 0 {
		cfg.AbsoluteTimeout = 24 * time.Hour
	}
	if cfg.RotationInterval == 0 {
		cfg.RotationInterval = 15 * time.Minute
	}
	if cfg.RotationOverlap == 0 {
		cfg.RotationOverlap = time.Minute
	}
	if cfg.IdleTimeout <= 0 || cfg.AbsoluteTimeout < cfg.IdleTimeout {
		return nil, errors.New("browser session: invalid timeouts")
	}
	if cfg.RotationInterval <= 0 || cfg.RotationOverlap <= 0 || cfg.RotationOverlap >= cfg.RotationInterval {
		return nil, errors.New("browser session: invalid rotation timing")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, store: store, oidc: oidc, now: now}, nil
}

func (s *Service) BeginLogin(ctx context.Context, returnTo string) (string, error) {
	if returnTo == "" {
		returnTo = "/"
	}
	if !SafeReturnTo(returnTo) {
		return "", fmt.Errorf("unsafe return_to")
	}
	state, err := randomOpaque(32)
	if err != nil {
		return "", err
	}
	nonce, err := randomOpaque(32)
	if err != nil {
		return "", err
	}
	verifier, err := randomOpaque(64)
	if err != nil {
		return "", err
	}
	if _, err := s.store.CreateLoginAttempt(ctx, LoginAttemptInput{State: state, Nonce: nonce, PKCEVerifier: verifier, ReturnTo: returnTo}); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	return s.oidc.AuthorizationURL(ctx, AuthorizationRequest{State: state, Nonce: nonce, CodeChallenge: challenge, RedirectURI: s.cfg.RedirectURI, ClientID: s.cfg.ClientID})
}

func (s *Service) CompleteLogin(ctx context.Context, state, code, _ string) (token, returnTo string, err error) {
	attempt, err := s.store.ConsumeLoginAttempt(ctx, state)
	if err != nil {
		return "", "", err
	}
	exchanged, err := s.oidc.ExchangeAndVerify(ctx, ExchangeRequest{Code: code, Verifier: attempt.PKCEVerifier, Nonce: attempt.Nonce, RedirectURI: s.cfg.RedirectURI, ClientID: s.cfg.ClientID, ClientSecret: s.cfg.ClientSecret})
	if err != nil {
		return "", "", err
	}
	profile, err := s.oidc.Profile(ctx, exchanged.AccessToken)
	if err != nil {
		return "", "", err
	}
	if profile.EnterpriseID != exchanged.Identity.EnterpriseID || profile.UserID != exchanged.Identity.UserID {
		return "", "", errors.New("browser session: profile/token identity mismatch")
	}
	token, err = randomOpaque(48)
	if err != nil {
		return "", "", err
	}
	absolute := s.cfg.AbsoluteTimeout
	if remaining := exchanged.ExpiresAt.Sub(s.now()); remaining < absolute {
		absolute = remaining
	}
	idle := s.cfg.IdleTimeout
	if absolute < idle {
		idle = absolute
	}
	if err := s.store.CreateSession(ctx, token, profile, exchanged.AccessToken, idle, absolute, s.cfg.RotationInterval); err != nil {
		return "", "", err
	}
	return token, attempt.ReturnTo, nil
}

func (s *Service) Session(ctx context.Context, token string) (SessionAccess, error) {
	return s.store.ResolveSession(ctx, token, s.cfg.IdleTimeout, s.cfg.RotationInterval, s.cfg.RotationOverlap)
}
func (s *Service) RevokeLocal(ctx context.Context, token string) error {
	return s.store.RevokeSession(ctx, token)
}
func (s *Service) Logout(ctx context.Context, token string) error {
	op, err := s.store.BeginLogout(ctx, token)
	if err != nil {
		return err
	}
	if err := s.oidc.Logout(ctx, op.UpstreamAccessToken); err != nil {
		return err
	}
	return s.store.CompleteLogout(ctx, op.SessionHash)
}

func (s *Service) ReconcileLogouts(ctx context.Context, limit int) error {
	operations, err := s.store.PendingLogouts(ctx, limit)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, op := range operations {
		if err := s.oidc.Logout(ctx, op.UpstreamAccessToken); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.store.CompleteLogout(ctx, op.SessionHash); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func SafeReturnTo(v string) bool {
	if v == "" || len(v) > 2048 || !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsAny(v, "\r\n\\") {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && !u.IsAbs() && u.Host == ""
}

func randomOpaque(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
