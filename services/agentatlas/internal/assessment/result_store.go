package assessment

// result_store.go persists the immutable, versioned WorkAssessment RESULTS
// produced by evaluator.go (Task 18C), backed by
// db/migrations/000021_work_assessments.sql. Unlike the assessment-POLICY store
// (policy_store.go), a WorkAssessment has NO lifecycle transitions — it is a
// frozen record the moment it is written. There is therefore no UpdateRevision at
// all: append-only is the ENTIRE contract, and a re-assessment is a NEW version
// (a new deterministic id), never an in-place edit. Two implementations satisfy
// ResultStore — an in-memory MemoryResultStore (fast, deterministic tests) and a
// PostgresResultStore (authoritative) — and both enforce the same append-only
// immutability so their behavior cannot drift. It holds NO Apache AGE access.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
)

// ResultStore is the append-only persistence port for WorkAssessment results.
type ResultStore interface {
	// AppendAssessment validates wa, assigns its deterministic id and inserts it as
	// a NEW immutable version. A duplicate (tenant, id) — i.e. the identical
	// (subject, policy_revision, version, period) — is ErrRevisionExists.
	AppendAssessment(ctx context.Context, wa model.WorkAssessment) (model.WorkAssessment, error)

	// GetAssessment returns the exact (tenant, id), or ErrNotFound. A cross-tenant
	// read is reported as ErrNotFound.
	GetAssessment(ctx context.Context, tenant, id string) (model.WorkAssessment, error)

	// LatestVersion returns the highest-version assessment for
	// (tenant, subject, policy_key, policy_revision), or ErrNotFound — the head a
	// re-assessment increments from.
	LatestVersion(ctx context.Context, tenant, subject, policyKey string, policyRevision int) (model.WorkAssessment, error)
}

func cloneAssessment(w model.WorkAssessment) model.WorkAssessment {
	raw, _ := json.Marshal(w)
	var out model.WorkAssessment
	_ = json.Unmarshal(raw, &out)
	return out
}

// --- MemoryResultStore ------------------------------------------------------

// MemoryResultStore is an in-memory ResultStore enforcing the same append-only
// immutability contract as PostgresResultStore: a written assessment is never
// mutated in place, and a re-assessment is a new version.
type MemoryResultStore struct {
	mu   sync.Mutex
	now  func() time.Time
	rows map[string]map[string]model.WorkAssessment // tenant -> id -> assessment
}

// NewMemoryResultStore constructs an empty MemoryResultStore. now defaults to
// time.Now.
func NewMemoryResultStore(now func() time.Time) *MemoryResultStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryResultStore{now: now, rows: map[string]map[string]model.WorkAssessment{}}
}

func (s *MemoryResultStore) AppendAssessment(_ context.Context, wa model.WorkAssessment) (model.WorkAssessment, error) {
	stored := wa.Normalize()
	stored.ID = stored.DerivedID()
	if err := stored.Validate(); err != nil {
		return model.WorkAssessment{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := s.rows[stored.Tenant]
	if byID == nil {
		byID = map[string]model.WorkAssessment{}
		s.rows[stored.Tenant] = byID
	}
	if _, ok := byID[stored.ID]; ok {
		return model.WorkAssessment{}, ErrRevisionExists
	}
	byID[stored.ID] = cloneAssessment(stored)
	return cloneAssessment(stored), nil
}

func (s *MemoryResultStore) GetAssessment(_ context.Context, tenant, id string) (model.WorkAssessment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.rows[tenant][id]
	if !ok {
		return model.WorkAssessment{}, ErrNotFound
	}
	return cloneAssessment(w), nil
}

func (s *MemoryResultStore) LatestVersion(_ context.Context, tenant, subject, policyKey string, policyRevision int) (model.WorkAssessment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var matches []model.WorkAssessment
	for _, w := range s.rows[tenant] {
		if w.Subject == subject && w.PolicyKey == policyKey && w.PolicyRevision == policyRevision {
			matches = append(matches, w)
		}
	}
	if len(matches) == 0 {
		return model.WorkAssessment{}, ErrNotFound
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Version > matches[j].Version })
	return cloneAssessment(matches[0]), nil
}

var _ ResultStore = (*MemoryResultStore)(nil)

// --- PostgresResultStore ----------------------------------------------------

// PostgresResultStore is the authoritative ResultStore backed by
// db/migrations/000021_work_assessments.sql. It follows the same direct-pgx idiom
// as internal/outcome/postgres.go and policy_store.go: tenant-scoped queries,
// promoted scalar columns for indexing/CHECKs plus JSONB for the opaque result,
// and append-only immutability enforced by the table's own triggers (there is no
// update path in Go at all). It holds NO Apache AGE access.
type PostgresResultStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresResultStore constructs a PostgresResultStore over pool.
func NewPostgresResultStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresResultStore, error) {
	if pool == nil {
		return nil, errors.New("assessment postgres result store requires a pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresResultStore{pool: pool, now: now}, nil
}

func (s *PostgresResultStore) AppendAssessment(ctx context.Context, wa model.WorkAssessment) (model.WorkAssessment, error) {
	stored := wa.Normalize()
	stored.ID = stored.DerivedID()
	if err := stored.Validate(); err != nil {
		return model.WorkAssessment{}, err
	}
	content, _ := json.Marshal(stored)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO work_assessments (
			tenant, id, subject, level, policy_key, policy_revision, version, org,
			formal, org_version, period_start, period_end, graph_watermark, digest,
			content, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		stored.Tenant, stored.ID, stored.Subject, string(stored.Level), stored.PolicyKey, stored.PolicyRevision, stored.Version, nullString(stored.Org),
		stored.Formal, stored.OrgVersion, stored.Period.Start.UTC(), stored.Period.End.UTC(), int64(stored.Graph.Watermark), stored.Digest(),
		content, stored.CreatedAt.UTC()); err != nil {
		if isUniqueViolation(err) {
			return model.WorkAssessment{}, ErrRevisionExists
		}
		return model.WorkAssessment{}, err
	}
	return cloneAssessment(stored), nil
}

func (s *PostgresResultStore) GetAssessment(ctx context.Context, tenant, id string) (model.WorkAssessment, error) {
	return scanAssessmentRow(s.pool.QueryRow(ctx, `SELECT content FROM work_assessments WHERE tenant=$1 AND id=$2`, tenant, id))
}

func (s *PostgresResultStore) LatestVersion(ctx context.Context, tenant, subject, policyKey string, policyRevision int) (model.WorkAssessment, error) {
	return scanAssessmentRow(s.pool.QueryRow(ctx, `
		SELECT content FROM work_assessments
		WHERE tenant=$1 AND subject=$2 AND policy_key=$3 AND policy_revision=$4
		ORDER BY version DESC LIMIT 1`, tenant, subject, policyKey, policyRevision))
}

func scanAssessmentRow(row interface{ Scan(...any) error }) (model.WorkAssessment, error) {
	var content []byte
	if err := row.Scan(&content); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.WorkAssessment{}, ErrNotFound
		}
		return model.WorkAssessment{}, err
	}
	var w model.WorkAssessment
	if err := json.Unmarshal(content, &w); err != nil {
		return model.WorkAssessment{}, err
	}
	return w, nil
}

var _ ResultStore = (*PostgresResultStore)(nil)
