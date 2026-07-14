package outcomelearning

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresCandidateStore is the authoritative append-only CandidateStore backed
// by db/migrations/000018_outcome_learning_candidates.sql. It follows the same
// direct-pgx idiom as internal/outcome/postgres.go: tenant-scoped queries,
// promoted scalar columns for indexing/CHECKs plus JSONB for the opaque
// provenance blobs, and NO in-place mutation (the table's immutability triggers
// reject UPDATE/DELETE; re-evaluations append a new superseding row).
type PostgresCandidateStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresCandidateStore constructs a PostgresCandidateStore over pool.
func NewPostgresCandidateStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresCandidateStore, error) {
	if pool == nil {
		return nil, errors.New("outcomelearning postgres store requires a pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresCandidateStore{pool: pool, now: now}, nil
}

// Append inserts an immutable candidate. It is idempotent on (tenant, id): a
// re-append of the same deterministic id is a no-op (append-only, first wins),
// never an in-place update.
func (s *PostgresCandidateStore) Append(ctx context.Context, c Candidate) error {
	if err := c.Validate(); err != nil {
		return err
	}
	anchor, _ := json.Marshal(c.Anchor)
	sources, _ := json.Marshal(c.Sources)
	coverage, _ := json.Marshal(c.Coverage)
	replay, _ := json.Marshal(c.Replay)
	shadow, _ := json.Marshal(c.Shadow)
	proposal, _ := json.Marshal(c.Proposal)
	created := c.CreatedAt
	if created.IsZero() {
		created = s.now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO outcome_learning_candidates (
			tenant, id, org, kind, summary, watermark, anchor, sources,
			generation_policy, model_version, coverage, replay, shadow, proposal,
			disposition, quarantine_reason, governance_change_id, supersedes_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (tenant, id) DO NOTHING`,
		c.Tenant, c.ID, c.Org, string(c.Kind), c.Summary, int64(c.Watermark), anchor, sources,
		c.GenerationPolicy, c.ModelVersion, coverage, replay, shadow, proposal,
		string(c.Disposition), string(c.QuarantineReason), c.GovernanceChangeID, c.SupersedesID, created,
	)
	return err
}

// Get returns the (tenant, id) candidate, if present.
func (s *PostgresCandidateStore) Get(ctx context.Context, tenant, id string) (Candidate, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT tenant, id, org, kind, summary, watermark, anchor, sources,
		       generation_policy, model_version, coverage, replay, shadow, proposal,
		       disposition, quarantine_reason, governance_change_id, supersedes_id, created_at
		FROM outcome_learning_candidates WHERE tenant = $1 AND id = $2`, tenant, id)
	var (
		c                                                   Candidate
		kind, disposition, reason                           string
		watermark                                           int64
		anchor, sources, coverage, replay, shadow, proposal []byte
	)
	err := row.Scan(&c.Tenant, &c.ID, &c.Org, &kind, &c.Summary, &watermark, &anchor, &sources,
		&c.GenerationPolicy, &c.ModelVersion, &coverage, &replay, &shadow, &proposal,
		&disposition, &reason, &c.GovernanceChangeID, &c.SupersedesID, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Candidate{}, false, nil
	}
	if err != nil {
		return Candidate{}, false, err
	}
	c.Kind = CandidateKind(kind)
	c.Watermark = uint64(watermark)
	c.Disposition = Disposition(disposition)
	c.QuarantineReason = QuarantineReason(reason)
	_ = json.Unmarshal(anchor, &c.Anchor)
	_ = json.Unmarshal(sources, &c.Sources)
	_ = json.Unmarshal(coverage, &c.Coverage)
	_ = json.Unmarshal(replay, &c.Replay)
	_ = json.Unmarshal(shadow, &c.Shadow)
	_ = json.Unmarshal(proposal, &c.Proposal)
	return c, true, nil
}

var _ CandidateStore = (*PostgresCandidateStore)(nil)
