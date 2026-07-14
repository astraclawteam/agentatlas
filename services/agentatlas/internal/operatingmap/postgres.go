package operatingmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	model "github.com/astraclawteam/agentatlas/sdk/go/operatingmap"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// PostgresStore is a direct-pgx implementation of Store (see service.go),
// following the same idiom as internal/browsersession/postgres_store.go and
// internal/governance/postgres_store.go: no sqlc, tenant-scoped queries. It
// backs operating_map_entries (migration 000015), an append-only table
// whose UPDATE/DELETE are additionally rejected by a database trigger —
// Publish only ever INSERTs.
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresStore builds a PostgresStore. now defaults to time.Now.
func NewPostgresStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresStore, error) {
	if pool == nil {
		return nil, errors.New("operatingmap postgres store requires a pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresStore{pool: pool, now: now}, nil
}

// epochSentinel marks a NULL effective_to column (an open-ended entry),
// following internal/browsersession/postgres_store.go's
// COALESCE(...,'epoch'::timestamptz) idiom for nullable timestamptz columns
// scanned through pgx without pulling in the pgtype package. A real
// effective_to can never legitimately equal the Unix epoch.
var epochSentinel = time.Unix(0, 0).UTC()

// farFuture stands in for "+infinity" ONLY as a query-parameter value when
// comparing against a candidate's possibly-open-ended EffectiveTo (Publish's
// overlap check). It is never written to a column: the INSERT below binds a
// genuine SQL NULL for an open-ended effective_to.
var farFuture = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

// entryContent is the shape of operating_map_entries.content: every field
// of model.Entry that is not already its own scalar column.
type entryContent struct {
	IntentPhrases        []string                   `json:"intent_phrases"`
	DataNeeds            []model.DataNeed           `json:"data_needs"`
	MethodRefs           []model.MethodRef          `json:"method_refs"`
	BusinessCapabilities []model.BusinessCapability `json:"business_capabilities"`
	CorrelationRules     []model.CorrelationRule    `json:"correlation_rules"`
	AuthorityRules       []model.AuthorityRule      `json:"authority_rules"`
	FreshnessRules       []model.FreshnessRule      `json:"freshness_rules"`
	ConflictRules        []model.ConflictRule       `json:"conflict_rules"`
	MemoryRefs           []model.MemoryRef          `json:"memory_refs"`
}

const entrySelectColumns = `SELECT id,enterprise_id,org_scope,org_version,intent_key,version,effective_from,COALESCE(effective_to,'epoch'::timestamptz),source_policy_revision,governance_review_ref,content,created_at `

// Publish implements Store. See the Store interface doc comment in
// service.go for the exact contract; this implementation makes version
// assignment, the conflict check and the insert atomic per
// (enterprise_id, org_scope) with a transaction-scoped advisory lock,
// mirroring internal/governance/postgres_store.go's FinalizePublish.
func (s *PostgresStore) Publish(ctx context.Context, candidate model.Entry, check func([]model.Entry) error) (model.Entry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.Entry{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, candidate.EnterpriseID+"|"+candidate.OrgScope); err != nil {
		return model.Entry{}, err
	}

	candidateTo := candidate.EffectiveTo
	if candidateTo.IsZero() {
		candidateTo = farFuture
	}
	// ORDER BY makes the overlap set's feed order deterministic. The
	// conflict verdict itself no longer depends on row order
	// (detectRuleConflicts deduplicates to the latest version per intent
	// key and compares in sorted key order), but a stable feed is cheap
	// insurance for any future check logic.
	rows, err := tx.Query(ctx, entrySelectColumns+`
		FROM operating_map_entries
		WHERE enterprise_id=$1 AND org_scope=$2
		  AND effective_from < $4::timestamptz
		  AND COALESCE(effective_to, 'infinity'::timestamptz) > $3::timestamptz
		ORDER BY intent_key, version`,
		candidate.EnterpriseID, candidate.OrgScope, candidate.EffectiveFrom, candidateTo)
	if err != nil {
		return model.Entry{}, err
	}
	overlapping, err := scanEntries(rows)
	if err != nil {
		return model.Entry{}, err
	}
	if check != nil {
		if err := check(overlapping); err != nil {
			return model.Entry{}, err
		}
	}

	var nextVersion int32
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM operating_map_entries WHERE enterprise_id=$1 AND org_scope=$2 AND intent_key=$3`,
		candidate.EnterpriseID, candidate.OrgScope, candidate.IntentKey).Scan(&nextVersion); err != nil {
		return model.Entry{}, err
	}
	candidate.Version = nextVersion
	candidate.ID = fmt.Sprintf("opm_%s", stableHash(candidate.EnterpriseID, candidate.OrgScope, candidate.IntentKey, fmt.Sprint(candidate.Version)))
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = s.now().UTC()
	}

	content, err := json.Marshal(entryContent{
		IntentPhrases: candidate.IntentPhrases, DataNeeds: candidate.DataNeeds, MethodRefs: candidate.MethodRefs,
		BusinessCapabilities: candidate.BusinessCapabilities, CorrelationRules: candidate.CorrelationRules,
		AuthorityRules: candidate.AuthorityRules, FreshnessRules: candidate.FreshnessRules,
		ConflictRules: candidate.ConflictRules, MemoryRefs: candidate.MemoryRefs,
	})
	if err != nil {
		return model.Entry{}, err
	}

	var effectiveToArg any
	if !candidate.EffectiveTo.IsZero() {
		effectiveToArg = candidate.EffectiveTo
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operating_map_entries
		(id,enterprise_id,org_scope,org_version,intent_key,version,effective_from,effective_to,source_policy_revision,governance_review_ref,content,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		candidate.ID, candidate.EnterpriseID, candidate.OrgScope, candidate.OrgVersion, candidate.IntentKey, candidate.Version,
		candidate.EffectiveFrom, effectiveToArg, candidate.SourcePolicyRevision, candidate.GovernanceReviewRef, content, candidate.CreatedAt); err != nil {
		return model.Entry{}, err
	}

	// Task 0I transactional outbox: append the published Operating Map entry as a
	// canonical projection event to the shared Outcome-Graph outbox, in THIS
	// transaction (no PostgreSQL/AGE dual write).
	if _, err := outcomegraph.AppendOutboxTx(ctx, tx, outcomegraph.MapOperatingMap(candidate), s.now()); err != nil {
		return model.Entry{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return model.Entry{}, err
	}
	return candidate, nil
}

// ActiveEntries implements Store: an exact (enterpriseID, orgScope) match —
// never a prefix or hierarchy walk, so tenant isolation and exact-scope
// isolation both fail closed to "no rows" — filtered to entries whose
// half-open effective interval covers asOf.
func (s *PostgresStore) ActiveEntries(ctx context.Context, enterpriseID, orgScope string, asOf time.Time) ([]model.Entry, error) {
	rows, err := s.pool.Query(ctx, entrySelectColumns+`
		FROM operating_map_entries
		WHERE enterprise_id=$1 AND org_scope=$2
		  AND effective_from <= $3::timestamptz
		  AND COALESCE(effective_to, 'infinity'::timestamptz) > $3::timestamptz`,
		enterpriseID, orgScope, asOf.UTC())
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

func scanEntries(rows pgx.Rows) ([]model.Entry, error) {
	defer rows.Close()
	var out []model.Entry
	for rows.Next() {
		var e model.Entry
		var effectiveTo time.Time
		var content []byte
		if err := rows.Scan(&e.ID, &e.EnterpriseID, &e.OrgScope, &e.OrgVersion, &e.IntentKey, &e.Version,
			&e.EffectiveFrom, &effectiveTo, &e.SourcePolicyRevision, &e.GovernanceReviewRef, &content, &e.CreatedAt); err != nil {
			return nil, err
		}
		if !effectiveTo.Equal(epochSentinel) {
			e.EffectiveTo = effectiveTo
		}
		var c entryContent
		if err := json.Unmarshal(content, &c); err != nil {
			return nil, err
		}
		e.IntentPhrases = c.IntentPhrases
		e.DataNeeds = c.DataNeeds
		e.MethodRefs = c.MethodRefs
		e.BusinessCapabilities = c.BusinessCapabilities
		e.CorrelationRules = c.CorrelationRules
		e.AuthorityRules = c.AuthorityRules
		e.FreshnessRules = c.FreshnessRules
		e.ConflictRules = c.ConflictRules
		e.MemoryRefs = c.MemoryRefs
		out = append(out, e)
	}
	return out, rows.Err()
}

func stableHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
