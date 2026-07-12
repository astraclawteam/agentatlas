package governance

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

type MemoryStore struct {
	mu         sync.Mutex
	now        func() time.Time
	records    map[string]Record
	operations map[string]PublishOperation
}

func NewMemoryStore(now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{now: now, records: map[string]Record{}, operations: map[string]PublishOperation{}}
}
func key(ent, id string) string { return ent + "\x00" + id }
func (s *MemoryStore) Create(_ context.Context, r Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(r.Draft.EnterpriseID, r.Draft.ChangeID)
	if _, ok := s.records[k]; ok {
		return Record{}, ErrConflict
	}
	s.records[k] = r
	return r, nil
}
func (s *MemoryStore) Get(_ context.Context, ent, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[key(ent, id)]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}
func (s *MemoryStore) List(_ context.Context, ent, org string, limit int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Record{}
	for _, r := range s.records {
		if r.Draft.EnterpriseID == ent && r.Draft.OrgUnitID == org {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (s *MemoryStore) Update(_ context.Context, ent, id string, revision int32, content json.RawMessage) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(ent, id)
	r, ok := s.records[k]
	if !ok {
		return Record{}, ErrNotFound
	}
	if r.Draft.Revision != revision {
		return Record{}, &ConflictError{CurrentRevision: r.Draft.Revision, Diff: makeDiff(r.Content, content)}
	}
	r.Draft.Revision++
	r.Draft.UpdatedAt = s.now().UTC()
	r.Content = clone(content)
	_ = jsonInto(content, &r.Draft.ProposedContent)
	s.records[k] = r
	return r, nil
}
func (s *MemoryStore) SaveReview(_ context.Context, ent string, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(ent, r.Draft.ChangeID)
	if _, ok := s.records[k]; !ok {
		return ErrNotFound
	}
	s.records[k] = r
	return nil
}
func (s *MemoryStore) BeginPublish(_ context.Context, ent, idem, id string, rev int32, payload string) (PublishOperation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(ent, idem)
	if op, ok := s.operations[k]; ok {
		return op, true, nil
	}
	op := PublishOperation{ChangeID: id, Revision: rev, PayloadHash: payload}
	s.operations[k] = op
	return op, false, nil
}
func (s *MemoryStore) FinalizePublish(_ context.Context, ent, idem string, _ Actor, rec Record, auditRef string) (PublishedVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(ent, idem)
	op, ok := s.operations[k]
	if !ok {
		return PublishedVersion{}, errors.New("missing publish operation")
	}
	result := PublishedVersion{ChangeID: op.ChangeID, ResourceID: rec.Draft.ResourceID, Version: 1, AuditRefID: auditRef}
	op.Result = result
	op.Complete = true
	s.operations[k] = op
	r := s.records[key(ent, op.ChangeID)]
	r.Draft.State = "published"
	s.records[key(ent, op.ChangeID)] = r
	return result, nil
}

func jsonInto(raw []byte, dst any) error { return json.Unmarshal(raw, dst) }

type MemoryAuditAppender struct {
	mu   sync.Mutex
	refs map[string]string
}

func (a *MemoryAuditAppender) Append(_ context.Context, actor Actor, _ Record, key string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.refs == nil {
		a.refs = map[string]string{}
	}
	scope := actor.EnterpriseID + "|" + key
	if ref, ok := a.refs[scope]; ok {
		return ref, nil
	}
	ref := stableID("audit", actor.EnterpriseID, key)
	a.refs[scope] = ref
	return ref, nil
}
func (a *MemoryAuditAppender) Count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.refs) }

type MemoryPublisher struct {
	mu sync.Mutex
	n  int
}

func NewMemoryPublisher() *MemoryPublisher { return &MemoryPublisher{} }
func (p *MemoryPublisher) Publish(_ context.Context, _ Actor, _ Record) (int32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return int32(p.n), nil
}
func (p *MemoryPublisher) Count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.n }
