package browsersession

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool      *pgxpool.Pool
	protector *Protector
	now       func() time.Time
}

func NewPostgresStore(pool *pgxpool.Pool, protector *Protector, now func() time.Time) (*PostgresStore, error) {
	if pool == nil || protector == nil {
		return nil, errors.New("browser session postgres store requires pool and protector")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresStore{pool: pool, protector: protector, now: now}, nil
}
func (s *PostgresStore) CreateLoginAttempt(ctx context.Context, in LoginAttemptInput) (LoginAttempt, error) {
	if !validOpaque(in.State, 16, 256) || !validOpaque(in.Nonce, 16, 256) || !validOpaque(in.PKCEVerifier, 43, 128) || !SafeReturnTo(in.ReturnTo) {
		return LoginAttempt{}, ErrInvalidState
	}
	ciphertext, err := s.protector.Seal(in.PKCEVerifier)
	if err != nil {
		return LoginAttempt{}, err
	}
	now := s.now().UTC()
	expires := now.Add(5 * time.Minute)
	_, err = s.pool.Exec(ctx, `INSERT INTO atlas_browser_login_attempts(state_hash,nonce,pkce_verifier_ciphertext,return_to,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, hash(in.State), in.Nonce, ciphertext, in.ReturnTo, now, expires)
	if err != nil {
		return LoginAttempt{}, err
	}
	return LoginAttempt{State: in.State, Nonce: in.Nonce, PKCEVerifier: in.PKCEVerifier, ReturnTo: in.ReturnTo, ExpiresAt: expires}, nil
}
func (s *PostgresStore) ConsumeLoginAttempt(ctx context.Context, state string) (LoginAttempt, error) {
	var nonce, returnTo string
	var ciphertext []byte
	var expires time.Time
	err := s.pool.QueryRow(ctx, `DELETE FROM atlas_browser_login_attempts WHERE state_hash=$1 AND expires_at>$2 RETURNING nonce,pkce_verifier_ciphertext,return_to,expires_at`, hash(state), s.now().UTC()).Scan(&nonce, &ciphertext, &returnTo, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginAttempt{}, ErrInvalidState
	}
	if err != nil {
		return LoginAttempt{}, err
	}
	verifier, err := s.protector.Open(ciphertext)
	if err != nil {
		return LoginAttempt{}, err
	}
	return LoginAttempt{State: state, Nonce: nonce, PKCEVerifier: verifier, ReturnTo: returnTo, ExpiresAt: expires}, nil
}
func (s *PostgresStore) CreateSession(ctx context.Context, token string, id Identity, upstream string, idle, absolute time.Duration) error {
	if !validOpaque(token, 32, 256) || !validOpaque(upstream, 16, 4096) || !validIdentity(id) || idle <= 0 || absolute < idle {
		return ErrInvalidSession
	}
	orgs, err := json.Marshal(id.OrgUnitIDs)
	if err != nil {
		return err
	}
	permissions, err := json.Marshal(id.Permissions)
	if err != nil {
		return err
	}
	ciphertext, err := s.protector.Seal(upstream)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	_, err = s.pool.Exec(ctx, `INSERT INTO atlas_browser_sessions(session_hash,enterprise_id,enterprise_user_id,display_name,org_version,org_unit_ids,permissions,advanced_mode_allowed,upstream_access_token_ciphertext,created_at,last_seen_at,idle_expires_at,absolute_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10,$11,$12)`, hash(token), id.EnterpriseID, id.UserID, id.DisplayName, id.OrgVersion, orgs, permissions, id.AdvancedModeAllowed, ciphertext, now, now.Add(idle), now.Add(absolute))
	return err
}
func (s *PostgresStore) GetSession(ctx context.Context, token string) (Session, error) {
	now := s.now().UTC()
	var out Session
	var orgs, permissions, ciphertext []byte
	err := s.pool.QueryRow(ctx, `UPDATE atlas_browser_sessions SET last_seen_at=$2,idle_expires_at=LEAST($2+interval '8 hours',absolute_expires_at) WHERE session_hash=$1 AND revoked_at IS NULL AND idle_expires_at>$2 AND absolute_expires_at>$2 RETURNING enterprise_id,enterprise_user_id,display_name,org_version,org_unit_ids,permissions,advanced_mode_allowed,upstream_access_token_ciphertext,created_at,last_seen_at,idle_expires_at,absolute_expires_at`, hash(token), now).Scan(&out.EnterpriseID, &out.UserID, &out.DisplayName, &out.OrgVersion, &orgs, &permissions, &out.AdvancedModeAllowed, &ciphertext, &out.CreatedAt, &out.LastSeenAt, &out.IdleExpiresAt, &out.AbsoluteExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrInvalidSession
	}
	if err != nil {
		return Session{}, err
	}
	if json.Unmarshal(orgs, &out.OrgUnitIDs) != nil || json.Unmarshal(permissions, &out.Permissions) != nil || len(out.OrgUnitIDs) > 1000 || len(out.Permissions) > 100 {
		return Session{}, ErrInvalidSession
	}
	out.UpstreamAccessToken, err = s.protector.Open(ciphertext)
	if err != nil {
		return Session{}, ErrInvalidSession
	}
	return out, nil
}
func (s *PostgresStore) RevokeSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `UPDATE atlas_browser_sessions SET revoked_at=COALESCE(revoked_at,$2) WHERE session_hash=$1`, hash(token), s.now().UTC())
	return err
}
