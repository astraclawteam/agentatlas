// Package assessment persists and governs the B2 employee work-assessment
// POLICY lifecycle frozen by Task 18B (github.com/astraclawteam/agentatlas/sdk/
// go/assessment). It is deliberately FAIR and GOVERNED: a policy draft is
// evidence-grounded and graph-bound, forbidden dimensions are hard-rejected, a
// pre-cycle-1 or shadow assessment is never a formal score, and publication
// flows only through the shadow-cycle gate plus the existing 0C governance
// review path — a policy never self-publishes.
//
// Two implementations satisfy PolicyStore: an in-memory MemoryPolicyStore (for
// fast, deterministic lifecycle tests) and a PostgresPolicyStore (the
// authoritative store, db/migrations/000020_assessment_policies.sql). Both
// enforce the SAME structural immutability contract so their behavior cannot
// drift: a policy revision's RULE SET is immutable once created (a rule edit is a
// NEW revision), a published revision may only make the single terminal
// published -> retired transition, and a retired revision is fully frozen. This
// mirrors the append-only-immutable + down-guard discipline of the Outcome
// stores (Tasks 0G/0I, migrations 000016/000017/000018).
package assessment

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
)

// Sentinel store errors. Callers compare with errors.Is.
var (
	// ErrNotFound is returned when no policy revision exists for the given
	// (tenant, policy_key, revision) or (tenant, policy_key). A record under a
	// different tenant is indistinguishable from one that does not exist.
	ErrNotFound = errors.New("assessment: policy revision not found")
	// ErrRevisionExists is returned when a (tenant, policy_key, revision) that
	// already exists is created again — history is immutable, so a change is a
	// NEW, higher revision, never an overwrite.
	ErrRevisionExists = errors.New("assessment: policy revision already exists (history is immutable; a rule edit is a new revision)")
	// ErrFrozenRevision is returned when a published/retired revision is mutated
	// beyond the single permitted published -> retired transition.
	ErrFrozenRevision = errors.New("assessment: a published/retired policy revision is immutable")
	// ErrRulesImmutable is returned when a revision's rule set is changed in
	// place; a rule edit must create a new revision (restarting the shadow count).
	ErrRulesImmutable = errors.New("assessment: policy rules are immutable within a revision; a rule edit must create a new revision")
)

// PolicyStore is the persistence port for the assessment-policy lifecycle. A
// revision is created once (CreateRevision) and then advances through its
// lifecycle via UpdateRevision, which enforces the immutability contract.
type PolicyStore interface {
	// CreateRevision validates p, assigns its deterministic id and inserts it as
	// a NEW revision. A duplicate (tenant, policy_key, revision) is
	// ErrRevisionExists.
	CreateRevision(ctx context.Context, p model.Policy) (model.Policy, error)

	// UpdateRevision persists a lifecycle transition (status, shadow cycles,
	// governance link) for an existing revision. It rejects any change to the
	// rule set (ErrRulesImmutable) and freezes a published revision except for the
	// single terminal published -> retired transition (ErrFrozenRevision).
	UpdateRevision(ctx context.Context, p model.Policy) (model.Policy, error)

	// GetRevision returns the exact (tenant, policy_key, revision), or ErrNotFound.
	GetRevision(ctx context.Context, tenant, policyKey string, revision int) (model.Policy, error)

	// Head returns the highest revision for (tenant, policy_key), or ErrNotFound.
	Head(ctx context.Context, tenant, policyKey string) (model.Policy, error)
}

// --- immutability guard (shared contract) ----------------------------------

// checkTransition enforces the structural immutability contract when a stored
// revision (old) is updated to a new state. It is the single source of truth
// both stores share so their behavior cannot drift.
//
// LAYERING (deliberate): the STORE/DB enforce only the two structural invariants
// that must hold regardless of caller — IMMUTABILITY (a frozen published/retired
// terminal state, and an immutable rule set within a revision) and, in Postgres,
// NO-SILENT-PUBLICATION (the governed_publication CHECK). The LEGALITY of the
// non-terminal lifecycle transitions — which operation is valid from which status
// (draft/shadow/reviewing) — is enforced AUTHORITATIVELY in the SERVICE layer
// (BeginShadowCycle/CompleteShadowCycle/ConfirmPublication/Retire preconditions,
// returning ErrInvalidState; covered by the service precondition tests). We
// deliberately do NOT duplicate that full transition matrix into checkTransition
// or the DB: two independent copies of the matrix (Memory and Postgres) would be
// a drift hazard, whereas the structural invariants above are simple enough to
// keep in lockstep.
func checkTransition(old, updated model.Policy) error {
	switch old.Status {
	case model.StatusRetired:
		// Fully frozen.
		return ErrFrozenRevision
	case model.StatusPublished:
		// A published revision is immutable except for the single terminal
		// transition to retired, which changes ONLY the status.
		if updated.Status != model.StatusRetired || !sameExceptStatus(old, updated) {
			return ErrFrozenRevision
		}
		return nil
	default:
		// Non-terminal (draft/shadow/reviewing): the RULE SET is immutable within
		// a revision; only status, shadow cycles and the governance link advance.
		if old.RuleDigest() != updated.RuleDigest() {
			return ErrRulesImmutable
		}
		return nil
	}
}

// sameExceptStatus reports whether two policies are identical once Status and
// UpdatedAt are normalized away, so a published -> retired transition can change
// nothing but the status.
func sameExceptStatus(a, b model.Policy) bool {
	a.Status, b.Status = "", ""
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// clonePolicy deep-copies a policy so a stored value can never be mutated in
// place by a caller holding a returned copy.
func clonePolicy(p model.Policy) model.Policy {
	raw, _ := json.Marshal(p)
	var out model.Policy
	_ = json.Unmarshal(raw, &out)
	return out
}

// --- MemoryPolicyStore ------------------------------------------------------

// MemoryPolicyStore is an in-memory PolicyStore enforcing exactly the same
// immutability contract as PostgresPolicyStore. Lifecycle tests run against it
// to exercise real behavior without a database.
type MemoryPolicyStore struct {
	mu   sync.Mutex
	now  func() time.Time
	rows map[string]map[int]model.Policy // tenant\x00policy_key -> revision -> policy
}

// NewMemoryPolicyStore constructs an empty MemoryPolicyStore. now defaults to
// time.Now.
func NewMemoryPolicyStore(now func() time.Time) *MemoryPolicyStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryPolicyStore{now: now, rows: map[string]map[int]model.Policy{}}
}

func policyChainKey(tenant, policyKey string) string { return tenant + "\x00" + policyKey }

// CreateRevision inserts a new immutable revision.
func (s *MemoryPolicyStore) CreateRevision(_ context.Context, p model.Policy) (model.Policy, error) {
	if err := p.Validate(); err != nil {
		return model.Policy{}, err
	}
	p.ID = p.DerivedID()
	s.mu.Lock()
	defer s.mu.Unlock()
	k := policyChainKey(p.Tenant, p.PolicyKey)
	chain := s.rows[k]
	if chain == nil {
		chain = map[int]model.Policy{}
		s.rows[k] = chain
	}
	if _, ok := chain[p.Revision]; ok {
		return model.Policy{}, ErrRevisionExists
	}
	chain[p.Revision] = clonePolicy(p)
	return clonePolicy(p), nil
}

// UpdateRevision persists a lifecycle transition under the immutability guard.
func (s *MemoryPolicyStore) UpdateRevision(_ context.Context, p model.Policy) (model.Policy, error) {
	if err := p.Validate(); err != nil {
		return model.Policy{}, err
	}
	p.ID = p.DerivedID()
	s.mu.Lock()
	defer s.mu.Unlock()
	chain := s.rows[policyChainKey(p.Tenant, p.PolicyKey)]
	old, ok := chain[p.Revision]
	if !ok {
		return model.Policy{}, ErrNotFound
	}
	if err := checkTransition(old, p); err != nil {
		return model.Policy{}, err
	}
	chain[p.Revision] = clonePolicy(p)
	return clonePolicy(p), nil
}

// GetRevision returns the exact revision.
func (s *MemoryPolicyStore) GetRevision(_ context.Context, tenant, policyKey string, revision int) (model.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[policyChainKey(tenant, policyKey)][revision]
	if !ok {
		return model.Policy{}, ErrNotFound
	}
	return clonePolicy(p), nil
}

// Head returns the highest revision for (tenant, policy_key).
func (s *MemoryPolicyStore) Head(_ context.Context, tenant, policyKey string) (model.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chain := s.rows[policyChainKey(tenant, policyKey)]
	if len(chain) == 0 {
		return model.Policy{}, ErrNotFound
	}
	var head int
	for rev := range chain {
		if rev > head {
			head = rev
		}
	}
	return clonePolicy(chain[head]), nil
}

var _ PolicyStore = (*MemoryPolicyStore)(nil)

// --- PostgresPolicyStore ----------------------------------------------------

// pgUniqueViolation is the PostgreSQL SQLSTATE for a unique/primary-key
// violation. See db/migrations/000020_assessment_policies.sql.
const pgUniqueViolation = "23505"

// PostgresPolicyStore is the authoritative PolicyStore backed by
// db/migrations/000020_assessment_policies.sql. It follows the same direct-pgx
// idiom as internal/outcome/postgres.go and internal/outcomelearning/postgres.go:
// tenant-scoped queries, promoted scalar columns for indexing/CHECKs plus JSONB
// for the opaque rubric, and the immutability contract enforced BOTH in Go
// (checkTransition, for clean errors) and by the table's own triggers (the
// database backstop). It holds NO Apache AGE access.
type PostgresPolicyStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresPolicyStore constructs a PostgresPolicyStore over pool. now defaults
// to time.Now.
func NewPostgresPolicyStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresPolicyStore, error) {
	if pool == nil {
		return nil, errors.New("assessment postgres store requires a pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresPolicyStore{pool: pool, now: now}, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// CreateRevision inserts a new immutable revision.
func (s *PostgresPolicyStore) CreateRevision(ctx context.Context, p model.Policy) (model.Policy, error) {
	if err := p.Validate(); err != nil {
		return model.Policy{}, err
	}
	p.ID = p.DerivedID()
	sources, _ := json.Marshal(p.Sources)
	dims, _ := json.Marshal(p.Dimensions)
	evidence, _ := json.Marshal(p.EvidenceRules)
	attribution, _ := json.Marshal(p.AttributionRules)
	confidence, _ := json.Marshal(p.ConfidenceRules)
	cycles, _ := json.Marshal(nonNilCycles(p.ShadowCycles))
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO assessment_policies (
			tenant, id, policy_key, revision, org, status, sources, watermark,
			dimensions, evidence_rules, attribution_rules, confidence_rules,
			shadow_cycles, rule_digest, governance_change_id, supersedes_id,
			created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		p.Tenant, p.ID, p.PolicyKey, p.Revision, nullString(p.Org), string(p.Status), sources, int64(p.Watermark),
		dims, evidence, attribution, confidence,
		cycles, p.RuleDigest(), p.GovernanceChangeID, p.SupersedesID,
		p.CreatedAt.UTC(), p.UpdatedAt.UTC()); err != nil {
		if isUniqueViolation(err) {
			return model.Policy{}, ErrRevisionExists
		}
		return model.Policy{}, err
	}
	return clonePolicy(p), nil
}

// UpdateRevision persists a lifecycle transition under the immutability guard.
// The Go guard (checkTransition) yields clean sentinels; the table triggers are
// the authoritative backstop.
func (s *PostgresPolicyStore) UpdateRevision(ctx context.Context, p model.Policy) (model.Policy, error) {
	if err := p.Validate(); err != nil {
		return model.Policy{}, err
	}
	p.ID = p.DerivedID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Policy{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := scanPolicyRow(tx.QueryRow(ctx, selectPolicySQL+` WHERE tenant=$1 AND policy_key=$2 AND revision=$3 FOR UPDATE`, p.Tenant, p.PolicyKey, p.Revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Policy{}, ErrNotFound
	}
	if err != nil {
		return model.Policy{}, err
	}
	if err := checkTransition(old, p); err != nil {
		return model.Policy{}, err
	}
	cycles, _ := json.Marshal(nonNilCycles(p.ShadowCycles))
	if _, err := tx.Exec(ctx, `
		UPDATE assessment_policies
		SET status=$4, shadow_cycles=$5, governance_change_id=$6, updated_at=$7
		WHERE tenant=$1 AND policy_key=$2 AND revision=$3`,
		p.Tenant, p.PolicyKey, p.Revision, string(p.Status), cycles, p.GovernanceChangeID, p.UpdatedAt.UTC()); err != nil {
		return model.Policy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Policy{}, err
	}
	return clonePolicy(p), nil
}

// GetRevision returns the exact revision.
func (s *PostgresPolicyStore) GetRevision(ctx context.Context, tenant, policyKey string, revision int) (model.Policy, error) {
	p, err := scanPolicyRow(s.pool.QueryRow(ctx, selectPolicySQL+` WHERE tenant=$1 AND policy_key=$2 AND revision=$3`, tenant, policyKey, revision))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Policy{}, ErrNotFound
	}
	return p, err
}

// Head returns the highest revision for (tenant, policy_key).
func (s *PostgresPolicyStore) Head(ctx context.Context, tenant, policyKey string) (model.Policy, error) {
	p, err := scanPolicyRow(s.pool.QueryRow(ctx, selectPolicySQL+` WHERE tenant=$1 AND policy_key=$2 ORDER BY revision DESC LIMIT 1`, tenant, policyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Policy{}, ErrNotFound
	}
	return p, err
}

const selectPolicySQL = `
	SELECT tenant, id, policy_key, revision, org, status, sources, watermark,
	       dimensions, evidence_rules, attribution_rules, confidence_rules,
	       shadow_cycles, governance_change_id, supersedes_id, created_at, updated_at
	FROM assessment_policies`

func scanPolicyRow(row interface{ Scan(...any) error }) (model.Policy, error) {
	var (
		p                                                        model.Policy
		org                                                      *string
		status                                                   string
		watermark                                                int64
		sources, dims, evidence, attribution, confidence, cycles []byte
	)
	if err := row.Scan(&p.Tenant, &p.ID, &p.PolicyKey, &p.Revision, &org, &status, &sources, &watermark,
		&dims, &evidence, &attribution, &confidence, &cycles, &p.GovernanceChangeID, &p.SupersedesID,
		&p.CreatedAt, &p.UpdatedAt); err != nil {
		return model.Policy{}, err
	}
	if org != nil {
		p.Org = *org
	}
	p.Status = model.Status(status)
	p.Watermark = uint64(watermark)
	_ = json.Unmarshal(sources, &p.Sources)
	_ = json.Unmarshal(dims, &p.Dimensions)
	_ = json.Unmarshal(evidence, &p.EvidenceRules)
	_ = json.Unmarshal(attribution, &p.AttributionRules)
	_ = json.Unmarshal(confidence, &p.ConfidenceRules)
	_ = json.Unmarshal(cycles, &p.ShadowCycles)
	return p, nil
}

func nonNilCycles(c []model.ShadowCycle) []model.ShadowCycle {
	if c == nil {
		return []model.ShadowCycle{}
	}
	return c
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var _ PolicyStore = (*PostgresPolicyStore)(nil)
