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
	FamilyID                                                string
	Generation                                              int64
	Revoked                                                 bool
}

type SessionAccess struct {
	Session
	ReplacementToken string
}

type LogoutOperation struct {
	SessionHash         string
	UpstreamAccessToken string
	Attempts            int
}

type Store interface {
	CreateLoginAttempt(context.Context, LoginAttemptInput) (LoginAttempt, error)
	ConsumeLoginAttempt(context.Context, string) (LoginAttempt, error)
	CreateSession(context.Context, string, Identity, string, time.Duration, time.Duration, ...time.Duration) error
	GetSession(context.Context, string) (Session, error)
	ResolveSession(context.Context, string, time.Duration, time.Duration, time.Duration) (SessionAccess, error)
	RevokeSession(context.Context, string) error
	BeginLogout(context.Context, string) (LogoutOperation, error)
	CompleteLogout(context.Context, string) error
	PendingLogouts(context.Context, int) ([]LogoutOperation, error)
}

type memorySession struct {
	Session
	ParentHash, SuccessorHash, SuccessorToken string
	RotationDueAt, OverlapExpiresAt           time.Time
}

type MemoryStore struct {
	mu       sync.Mutex
	now      func() time.Time
	logins   map[string]LoginAttempt
	sessions map[string]memorySession
	logouts  map[string]LogoutOperation
}

func NewMemoryStore(now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{now: now, logins: map[string]LoginAttempt{}, sessions: map[string]memorySession{}, logouts: map[string]LogoutOperation{}}
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

func (s *MemoryStore) CreateSession(_ context.Context, token string, id Identity, upstreamToken string, idle, absolute time.Duration, rotationArg ...time.Duration) error {
	if !validOpaque(token, 32, 256) || !validOpaque(upstreamToken, 16, 4096) || !validIdentity(id) || idle <= 0 || absolute < idle {
		return ErrInvalidSession
	}
	now := s.now()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[hash(token)] = memorySession{Session: Session{Identity: id, UpstreamAccessToken: upstreamToken, CreatedAt: now, LastSeenAt: now, IdleExpiresAt: now.Add(idle), AbsoluteExpiresAt: now.Add(absolute), FamilyID: familyID, Generation: 1}, RotationDueAt: now.Add(rotation)}
	return nil
}

func (s *MemoryStore) GetSession(_ context.Context, token string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hash(token)
	record, ok := s.sessions[key]
	now := s.now()
	if !ok || record.Revoked || !now.Before(record.IdleExpiresAt) || !now.Before(record.AbsoluteExpiresAt) {
		return Session{}, ErrInvalidSession
	}
	record.LastSeenAt = now
	if next := now.Add(8 * time.Hour); next.Before(record.AbsoluteExpiresAt) {
		record.IdleExpiresAt = next
	} else {
		record.IdleExpiresAt = record.AbsoluteExpiresAt
	}
	s.sessions[key] = record
	return record.Session, nil
}

func (s *MemoryStore) ResolveSession(_ context.Context, token string, idle, rotation, overlap time.Duration) (SessionAccess, error) {
	if idle <= 0 || rotation <= 0 || overlap <= 0 {
		return SessionAccess{}, ErrInvalidSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hash(token)
	record, ok := s.sessions[key]
	now := s.now()
	if !ok || record.Revoked || !now.Before(record.IdleExpiresAt) || !now.Before(record.AbsoluteExpiresAt) {
		return SessionAccess{}, ErrInvalidSession
	}
	if record.SuccessorHash != "" {
		if !now.Before(record.OverlapExpiresAt) {
			return SessionAccess{}, ErrInvalidSession
		}
		successor := s.sessions[record.SuccessorHash]
		return SessionAccess{Session: successor.Session, ReplacementToken: record.SuccessorToken}, nil
	}
	if record.ParentHash != "" {
		parent := s.sessions[record.ParentHash]
		parent.OverlapExpiresAt = now
		s.sessions[record.ParentHash] = parent
	}
	record.LastSeenAt = now
	record.IdleExpiresAt = minTime(now.Add(idle), record.AbsoluteExpiresAt)
	if now.Before(record.RotationDueAt) {
		s.sessions[key] = record
		return SessionAccess{Session: record.Session}, nil
	}
	successorToken, err := randomOpaque(48)
	if err != nil {
		return SessionAccess{}, err
	}
	successorHash := hash(successorToken)
	successor := memorySession{Session: record.Session, ParentHash: key, RotationDueAt: now.Add(rotation)}
	successor.Generation++
	successor.CreatedAt = now
	successor.LastSeenAt = now
	successor.IdleExpiresAt = minTime(now.Add(idle), successor.AbsoluteExpiresAt)
	record.SuccessorHash = successorHash
	record.SuccessorToken = successorToken
	record.OverlapExpiresAt = minTime(now.Add(overlap), record.AbsoluteExpiresAt)
	s.sessions[key] = record
	s.sessions[successorHash] = successor
	return SessionAccess{Session: successor.Session, ReplacementToken: successorToken}, nil
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

func (s *MemoryStore) BeginLogout(_ context.Context, token string) (LogoutOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hash(token)
	if existing, ok := s.logouts[key]; ok {
		return existing, nil
	}
	record, ok := s.sessions[key]
	if !ok {
		return LogoutOperation{}, ErrInvalidSession
	}
	for sessionHash, familyRecord := range s.sessions {
		if familyRecord.FamilyID == record.FamilyID {
			familyRecord.Revoked = true
			s.sessions[sessionHash] = familyRecord
		}
	}
	op := LogoutOperation{SessionHash: key, UpstreamAccessToken: record.UpstreamAccessToken}
	s.logouts[key] = op
	return op, nil
}

func (s *MemoryStore) CompleteLogout(_ context.Context, sessionHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.logouts, sessionHash)
	if record, ok := s.sessions[sessionHash]; ok {
		for key, familyRecord := range s.sessions {
			if familyRecord.FamilyID == record.FamilyID {
				familyRecord.UpstreamAccessToken = ""
				s.sessions[key] = familyRecord
			}
		}
	}
	return nil
}

func (s *MemoryStore) PendingLogouts(_ context.Context, limit int) ([]LogoutOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 {
		return nil, nil
	}
	out := make([]LogoutOperation, 0, min(limit, len(s.logouts)))
	for key, op := range s.logouts {
		op.Attempts++
		s.logouts[key] = op
		out = append(out, op)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) PendingLogoutCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.logouts)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func hash(v string) string                    { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func validOpaque(v string, min, max int) bool { return len(v) >= min && len(v) <= max }
