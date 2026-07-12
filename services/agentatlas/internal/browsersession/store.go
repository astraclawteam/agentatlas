package browsersession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var (
	ErrInvalidState   = errors.New("browser session: invalid or consumed state")
	ErrInvalidSession = errors.New("browser session: invalid, expired, or revoked session")
)

type LoginAttemptInput struct {
	State, Nonce, PKCEVerifier, ReturnTo string
}

type LoginAttempt struct {
	State, Nonce, PKCEVerifier, ReturnTo string
	ExpiresAt                            time.Time
}

type Identity struct {
	EnterpriseID, UserID, DisplayName string
	OrgVersion                        int64
	OrgUnitIDs, Permissions           []string
	AdvancedModeAllowed               bool
}

type Session struct {
	Identity
	UpstreamAccessToken                                     string
	CreatedAt, LastSeenAt, IdleExpiresAt, AbsoluteExpiresAt time.Time
	Revoked                                                 bool
}

type Store interface {
	CreateLoginAttempt(context.Context, LoginAttemptInput) (LoginAttempt, error)
	ConsumeLoginAttempt(context.Context, string) (LoginAttempt, error)
	CreateSession(context.Context, string, Identity, string, time.Duration, time.Duration) error
	GetSession(context.Context, string) (Session, error)
	RevokeSession(context.Context, string) error
}

type MemoryStore struct {
	mu       sync.Mutex
	now      func() time.Time
	logins   map[string]LoginAttempt
	sessions map[string]Session
}

func NewMemoryStore(now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{now: now, logins: map[string]LoginAttempt{}, sessions: map[string]Session{}}
}

func (s *MemoryStore) CreateLoginAttempt(_ context.Context, in LoginAttemptInput) (LoginAttempt, error) {
	if !validOpaque(in.State, 16, 256) || !validOpaque(in.Nonce, 16, 256) || !validOpaque(in.PKCEVerifier, 43, 128) || !SafeReturnTo(in.ReturnTo) {
		return LoginAttempt{}, ErrInvalidState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := LoginAttempt{State: in.State, Nonce: in.Nonce, PKCEVerifier: in.PKCEVerifier, ReturnTo: in.ReturnTo, ExpiresAt: s.now().Add(5 * time.Minute)}
	s.logins[hash(in.State)] = a
	return a, nil
}

func (s *MemoryStore) ConsumeLoginAttempt(_ context.Context, state string) (LoginAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hash(state)
	a, ok := s.logins[key]
	delete(s.logins, key)
	if !ok || !s.now().Before(a.ExpiresAt) {
		return LoginAttempt{}, ErrInvalidState
	}
	return a, nil
}

func (s *MemoryStore) CreateSession(_ context.Context, token string, id Identity, upstreamToken string, idle, absolute time.Duration) error {
	if !validOpaque(token, 32, 256) || !validOpaque(upstreamToken, 16, 4096) || !validIdentity(id) || idle <= 0 || absolute < idle {
		return ErrInvalidSession
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[hash(token)] = Session{Identity: id, UpstreamAccessToken: upstreamToken, CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now.Add(idle), AbsoluteExpiresAt: now.Add(absolute)}
	return nil
}

func (s *MemoryStore) GetSession(_ context.Context, token string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hash(token)
	session, ok := s.sessions[key]
	now := s.now()
	if !ok || session.Revoked || !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
		return Session{}, ErrInvalidSession
	}
	session.LastSeenAt = now
	if next := now.Add(8 * time.Hour); next.Before(session.AbsoluteExpiresAt) {
		session.IdleExpiresAt = next
	} else {
		session.IdleExpiresAt = session.AbsoluteExpiresAt
	}
	s.sessions[key] = session
	return session, nil
}

func (s *MemoryStore) RevokeSession(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hash(token)
	session, ok := s.sessions[key]
	if !ok {
		return nil
	}
	session.Revoked = true
	s.sessions[key] = session
	return nil
}

func hash(v string) string                    { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func validOpaque(v string, min, max int) bool { return len(v) >= min && len(v) <= max }
