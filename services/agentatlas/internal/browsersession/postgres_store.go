package browsersession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
func (s *PostgresStore) CreateSession(ctx context.Context, token string, id Identity, upstream string, idle, absolute time.Duration, rotationArg ...time.Duration) error {
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
	rotation := 15 * time.Minute
	if len(rotationArg) > 0 {
		rotation = rotationArg[0]
	}
	if rotation <= 0 {
		return ErrInvalidSession
	}
	familyID, err := randomOpaque(24)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO atlas_browser_sessions(session_hash,session_family_id,generation,rotation_due_at,enterprise_id,enterprise_user_id,display_name,org_version,org_unit_ids,permissions,advanced_mode_allowed,upstream_access_token_ciphertext,created_at,last_seen_at,idle_expires_at,absolute_expires_at) VALUES($1,$2,1,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12,$13,$14)`, hash(token), familyID, now.Add(rotation), id.EnterpriseID, id.UserID, id.DisplayName, id.OrgVersion, orgs, permissions, id.AdvancedModeAllowed, ciphertext, now, now.Add(idle), now.Add(absolute))
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

func (s *PostgresStore) ResolveSession(ctx context.Context, token string, idle, rotation, overlap time.Duration) (SessionAccess, error) {
	if idle <= 0 || rotation <= 0 || overlap <= 0 {
		return SessionAccess{}, ErrInvalidSession
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return SessionAccess{}, err
	}
	defer tx.Rollback(ctx)
	now := s.now().UTC()
	key := hash(token)
	record, meta, err := s.readSession(ctx, tx, key, now, true)
	if err != nil {
		return SessionAccess{}, err
	}
	if meta.successorHash != "" {
		if !now.Before(meta.overlapExpiresAt) || len(meta.successorCiphertext) == 0 {
			return SessionAccess{}, ErrInvalidSession
		}
		successor, _, err := s.readSession(ctx, tx, meta.successorHash, now, false)
		if err != nil {
			return SessionAccess{}, err
		}
		replacement, err := s.protector.Open(meta.successorCiphertext)
		if err != nil {
			return SessionAccess{}, ErrInvalidSession
		}
		if err := tx.Commit(ctx); err != nil {
			return SessionAccess{}, err
		}
		return SessionAccess{Session: successor, ReplacementToken: replacement}, nil
	}
	if meta.parentHash != "" {
		if _, err := tx.Exec(ctx, `UPDATE atlas_browser_sessions SET overlap_expires_at=$2 WHERE session_hash=$1 AND (overlap_expires_at IS NULL OR overlap_expires_at>$2)`, meta.parentHash, now); err != nil {
			return SessionAccess{}, err
		}
	}
	record.LastSeenAt = now
	record.IdleExpiresAt = minTime(now.Add(idle), record.AbsoluteExpiresAt)
	if now.Before(meta.rotationDueAt) {
		if _, err := tx.Exec(ctx, `UPDATE atlas_browser_sessions SET last_seen_at=$2,idle_expires_at=$3 WHERE session_hash=$1`, key, now, record.IdleExpiresAt); err != nil {
			return SessionAccess{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return SessionAccess{}, err
		}
		return SessionAccess{Session: record}, nil
	}
	successorToken, err := randomOpaque(48)
	if err != nil {
		return SessionAccess{}, err
	}
	successorCiphertext, err := s.protector.Seal(successorToken)
	if err != nil {
		return SessionAccess{}, err
	}
	orgs, _ := json.Marshal(record.OrgUnitIDs)
	permissions, _ := json.Marshal(record.Permissions)
	successorHash := hash(successorToken)
	_, err = tx.Exec(ctx, `INSERT INTO atlas_browser_sessions(session_hash,session_family_id,generation,parent_hash,rotation_due_at,enterprise_id,enterprise_user_id,display_name,org_version,org_unit_ids,permissions,advanced_mode_allowed,upstream_access_token_ciphertext,created_at,last_seen_at,idle_expires_at,absolute_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14,$15,$16)`, successorHash, record.FamilyID, record.Generation+1, key, now.Add(rotation), record.EnterpriseID, record.UserID, record.DisplayName, record.OrgVersion, orgs, permissions, record.AdvancedModeAllowed, meta.upstreamCiphertext, now, record.IdleExpiresAt, record.AbsoluteExpiresAt)
	if err != nil {
		return SessionAccess{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE atlas_browser_sessions SET successor_hash=$2,successor_token_ciphertext=$3,overlap_expires_at=$4,last_seen_at=$5,idle_expires_at=$6 WHERE session_hash=$1`, key, successorHash, successorCiphertext, minTime(now.Add(overlap), record.AbsoluteExpiresAt), now, record.IdleExpiresAt); err != nil {
		return SessionAccess{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionAccess{}, err
	}
	record.Generation++
	record.CreatedAt = now
	return SessionAccess{Session: record, ReplacementToken: successorToken}, nil
}

type sessionMeta struct {
	parentHash, successorHash               string
	rotationDueAt, overlapExpiresAt         time.Time
	successorCiphertext, upstreamCiphertext []byte
}

type rowScanner interface{ Scan(...any) error }

func (s *PostgresStore) readSession(ctx context.Context, tx pgx.Tx, sessionHash string, now time.Time, lock bool) (Session, sessionMeta, error) {
	query := `SELECT enterprise_id,enterprise_user_id,display_name,org_version,org_unit_ids,permissions,advanced_mode_allowed,upstream_access_token_ciphertext,created_at,last_seen_at,idle_expires_at,absolute_expires_at,session_family_id,generation,COALESCE(parent_hash,''),COALESCE(successor_hash,''),successor_token_ciphertext,rotation_due_at,COALESCE(overlap_expires_at,'epoch'::timestamptz) FROM atlas_browser_sessions WHERE session_hash=$1 AND revoked_at IS NULL AND idle_expires_at>$2 AND absolute_expires_at>$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var out Session
	var meta sessionMeta
	var orgs, permissions []byte
	err := rowScanner(tx.QueryRow(ctx, query, sessionHash, now)).Scan(&out.EnterpriseID, &out.UserID, &out.DisplayName, &out.OrgVersion, &orgs, &permissions, &out.AdvancedModeAllowed, &meta.upstreamCiphertext, &out.CreatedAt, &out.LastSeenAt, &out.IdleExpiresAt, &out.AbsoluteExpiresAt, &out.FamilyID, &out.Generation, &meta.parentHash, &meta.successorHash, &meta.successorCiphertext, &meta.rotationDueAt, &meta.overlapExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, sessionMeta{}, ErrInvalidSession
	}
	if err != nil {
		return Session{}, sessionMeta{}, err
	}
	if json.Unmarshal(orgs, &out.OrgUnitIDs) != nil || json.Unmarshal(permissions, &out.Permissions) != nil {
		return Session{}, sessionMeta{}, ErrInvalidSession
	}
	if len(meta.upstreamCiphertext) > 0 {
		out.UpstreamAccessToken, err = s.protector.Open(meta.upstreamCiphertext)
		if err != nil {
			return Session{}, sessionMeta{}, ErrInvalidSession
		}
	}
	return out, meta, nil
}

func (s *PostgresStore) BeginLogout(ctx context.Context, token string) (LogoutOperation, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return LogoutOperation{}, err
	}
	defer tx.Rollback(ctx)
	key := hash(token)
	now := s.now().UTC()
	result, err := tx.Exec(ctx, `INSERT INTO atlas_browser_logout_operations(session_hash,upstream_access_token_ciphertext,created_at) SELECT session_hash,upstream_access_token_ciphertext,$2 FROM atlas_browser_sessions WHERE session_hash=$1 AND upstream_access_token_ciphertext IS NOT NULL ON CONFLICT(session_hash) DO NOTHING`, key, now)
	if err != nil {
		return LogoutOperation{}, err
	}
	var ciphertext []byte
	var attempts int
	err = tx.QueryRow(ctx, `SELECT upstream_access_token_ciphertext,attempts FROM atlas_browser_logout_operations WHERE session_hash=$1 FOR UPDATE`, key).Scan(&ciphertext, &attempts)
	if errors.Is(err, pgx.ErrNoRows) && result.RowsAffected() == 0 {
		return LogoutOperation{}, ErrInvalidSession
	}
	if err != nil {
		return LogoutOperation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE atlas_browser_sessions SET revoked_at=COALESCE(revoked_at,$2) WHERE session_family_id=(SELECT session_family_id FROM atlas_browser_sessions WHERE session_hash=$1)`, key, now); err != nil {
		return LogoutOperation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LogoutOperation{}, err
	}
	access, err := s.protector.Open(ciphertext)
	if err != nil {
		return LogoutOperation{}, err
	}
	return LogoutOperation{SessionHash: key, UpstreamAccessToken: access, Attempts: attempts}, nil
}

func (s *PostgresStore) CompleteLogout(ctx context.Context, sessionHash string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE atlas_browser_sessions SET upstream_access_token_ciphertext=NULL WHERE session_family_id=(SELECT session_family_id FROM atlas_browser_sessions WHERE session_hash=$1)`, sessionHash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM atlas_browser_logout_operations WHERE session_hash=$1`, sessionHash); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) PendingLogouts(ctx context.Context, limit int) ([]LogoutOperation, error) {
	if limit < 1 || limit > 1000 {
		return nil, nil
	}
	now := s.now().UTC()
	rows, err := s.pool.Query(ctx, `WITH pending AS (SELECT session_hash FROM atlas_browser_logout_operations WHERE last_attempt_at IS NULL OR last_attempt_at<($2::timestamptz-interval '10 seconds') ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED), claimed AS (UPDATE atlas_browser_logout_operations o SET attempts=o.attempts+1,last_attempt_at=$2 FROM pending p WHERE o.session_hash=p.session_hash RETURNING o.session_hash,o.upstream_access_token_ciphertext,o.attempts) SELECT session_hash,upstream_access_token_ciphertext,attempts FROM claimed`, limit, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogoutOperation
	var decryptErrs []error
	for rows.Next() {
		var op LogoutOperation
		var ciphertext []byte
		if err := rows.Scan(&op.SessionHash, &ciphertext, &op.Attempts); err != nil {
			return nil, err
		}
		op.UpstreamAccessToken, err = s.protector.Open(ciphertext)
		if err != nil {
			decryptErrs = append(decryptErrs, fmt.Errorf("decrypt pending browser logout %s: %w", op.SessionHash, err))
			continue
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		decryptErrs = append(decryptErrs, err)
	}
	return out, errors.Join(decryptErrs...)
}
