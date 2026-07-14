package outcomegraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// age.go is the Apache AGE-backed OutcomeGraphStore — the only GA provider. It
// speaks to a SEPARATELY CONFIGURED graph database (its own DSN), never the
// authoritative WorkCase PostgreSQL. Every Cypher statement is a FIXED template
// (a Go constant, with only closed-vocabulary labels interpolated and a
// validated integer depth) and every caller value is bound as an agtype
// parameter — never string-concatenated — so an injection attempt via any
// id/label parameter is stored as an inert literal and cannot escape the
// template.
//
// The graph is shared across tenants but tenant-isolated by construction: edges
// never cross tenants (validated on append), so a traversal from a tenant's
// anchor can never reach another tenant's component, and every query
// additionally filters on the tenant property (defense in depth). The projector
// watermark lives in this graph database (table outcome_graph_watermark) so it
// resets atomically with a rebuild and advances only after the AGE write commits.

// AGEStore implements OutcomeGraphStore over Apache AGE.
type AGEStore struct {
	pool      *pgxpool.Pool
	graphName string
}

// NewAGEStore opens a pool against the separately-configured graph DSN, loads
// the AGE extension per connection, ensures the graph and watermark table exist,
// and returns the store. The DSN is independent from the authoritative Postgres.
func NewAGEStore(ctx context.Context, dsn, graphName string) (*AGEStore, error) {
	if graphName == "" {
		graphName = DefaultGraphName
	}
	if len(graphName) < 3 {
		return nil, fmt.Errorf("outcomegraph: AGE graph name %q too short (AGE requires >= 3 chars)", graphName)
	}
	if !isSafeIdent(graphName) {
		return nil, fmt.Errorf("outcomegraph: unsafe AGE graph name %q", graphName)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("outcomegraph: age pool config: %w", err)
	}
	// AGE requires LOAD 'age' and the ag_catalog search_path on every connection.
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		if _, err := c.Exec(ctx, `LOAD 'age'`); err != nil {
			return err
		}
		_, err := c.Exec(ctx, `SET search_path = ag_catalog, "$user", public`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("outcomegraph: age pool: %w", err)
	}
	s := &AGEStore{pool: pool, graphName: graphName}
	if err := s.ensureSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *AGEStore) Close() { s.pool.Close() }

// isSafeIdent guards interpolated identifiers (graph name). Labels come from the
// closed NodeLabel/EdgeLabel vocabularies and are additionally checked by Valid.
func isSafeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func (s *AGEStore) ensureSchema(ctx context.Context) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ag_catalog.ag_graph WHERE name=$1)`, s.graphName).Scan(&exists); err != nil {
		return fmt.Errorf("outcomegraph: check graph: %w", mapAGEErr(err))
	}
	if !exists {
		if _, err := s.pool.Exec(ctx, `SELECT create_graph($1)`, s.graphName); err != nil {
			// Tolerate a concurrent creator.
			if !strings.Contains(err.Error(), "already exists") {
				return fmt.Errorf("outcomegraph: create graph: %w", mapAGEErr(err))
			}
		}
	}
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS outcome_graph_watermark(
		tenant text PRIMARY KEY, last_sequence bigint NOT NULL DEFAULT 0, updated_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("outcomegraph: watermark table: %w", mapAGEErr(err))
	}
	return nil
}

// mapAGEErr maps a lost/refused graph connection to ErrGraphUnavailable so
// graph-dependent callers degrade explicitly.
func mapAGEErr(err error) error {
	if err == nil {
		return nil
	}
	// Deliberately conservative, fail-CLOSED heuristic: it classifies transport /
	// availability failures as ErrGraphUnavailable so graph-dependent callers
	// degrade EXPLICITLY rather than trusting a stale/partial graph. Erring toward
	// "unavailable" is the safe direction (a genuinely-unavailable graph
	// misclassified as a normal error would be the dangerous one); the substrings
	// below are specific connection/shutdown phrases, not bare tokens.
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"connection refused", "connection reset", "server closed the connection",
		"no connection to the server", "failed to connect", "unexpected eof",
		"broken pipe", "the database system is", "closed pool", "conn closed",
		"is shutting down", "cannot connect",
	} {
		if strings.Contains(msg, sig) {
			return fmt.Errorf("%w: %v", ErrGraphUnavailable, err)
		}
	}
	return err
}

// execCypher runs a fixed Cypher template with bound agtype params, either in tx
// (when non-nil) or on the pool. The template is a Go constant with only
// closed-vocabulary labels interpolated; params is a JSON object bound as the
// single agtype argument.
func (s *AGEStore) execCypher(ctx context.Context, tx pgx.Tx, template string, params map[string]any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	stmt := fmt.Sprintf(`SELECT * FROM cypher('%s', $cy$ %s $cy$, $1) AS (v agtype)`, s.graphName, template)
	if tx != nil {
		_, err = tx.Exec(ctx, stmt, string(raw))
	} else {
		_, err = s.pool.Exec(ctx, stmt, string(raw))
	}
	return mapAGEErr(err)
}

// --- write side ------------------------------------------------------------

const mergeNodeTemplate = `MERGE (n:%s {nid: $nid}) SET n.tenant = $tenant, n.org = $org, n.business_id = $bid, n.revision = $rev, n.summary = $summary, n.hash = $hash`

// mergeEdgeTemplate ensures both endpoints exist (minimal MERGE with their own
// versioned identity) then MERGEs the edge — so an edge never fails on a missing
// endpoint and endpoints always carry tenant/business_id/revision.
const mergeEdgeTemplate = `MERGE (a:%s {nid: $from}) SET a.tenant = $tenant, a.business_id = $fbid, a.revision = $frev MERGE (b:%s {nid: $to}) SET b.tenant = $tenant, b.business_id = $tbid, b.revision = $trev MERGE (a)-[e:%s]->(b) SET e.tenant = $tenant`

const markTemplate = `MERGE (n:%s {nid: $nid}) SET n.tenant = $tenant, n.business_id = $bid, n.revision = $rev, n.%s = true`

func (s *AGEStore) applyEventTx(ctx context.Context, tx pgx.Tx, ev ProjectionEvent) error {
	switch ev.Kind {
	case KindUpsert:
		for _, n := range ev.Delta.Nodes {
			tmpl := fmt.Sprintf(mergeNodeTemplate, string(n.Label))
			if err := s.execCypher(ctx, tx, tmpl, map[string]any{
				"nid": n.NID(), "tenant": n.Tenant, "org": n.Org, "bid": n.BusinessID,
				"rev": n.Revision, "summary": n.Summary, "hash": n.Hash,
			}); err != nil {
				return err
			}
		}
		for _, e := range ev.Delta.Edges {
			tmpl := fmt.Sprintf(mergeEdgeTemplate, string(e.From.Label), string(e.To.Label), string(e.Label))
			if err := s.execCypher(ctx, tx, tmpl, map[string]any{
				"from": e.From.NID(e.Tenant), "to": e.To.NID(e.Tenant), "tenant": e.Tenant,
				"fbid": e.From.BusinessID, "frev": e.From.Revision, "tbid": e.To.BusinessID, "trev": e.To.Revision,
			}); err != nil {
				return err
			}
		}
	case KindTombstone, KindReevaluation:
		m := ev.Delta.Mark
		if m == nil {
			return nil
		}
		prop := "revoked"
		if ev.Kind == KindReevaluation {
			prop = "superseded"
		}
		tmpl := fmt.Sprintf(markTemplate, string(m.Label), prop)
		if err := s.execCypher(ctx, tx, tmpl, map[string]any{
			"nid": m.NID(ev.Tenant), "tenant": ev.Tenant, "bid": m.BusinessID, "rev": m.Revision,
		}); err != nil {
			return err
		}
	}
	return nil
}

// lockTenant takes the per-tenant projection advisory lock inside tx. It is
// held for the life of tx (xact-scoped), so every projection/rebuild transaction
// for a tenant runs strictly serially — ACROSS REPLICAS, since the lock lives in
// the graph database itself, not in process memory. This is the load-bearing
// serialization: AGE MERGE is not guaranteed concurrency-safe on the `nid`
// PROPERTY (there is no unique constraint on it, only AGE's internal id), and the
// interface is provider-neutral, so correctness must not depend on any provider's
// internal MERGE locking.
func (s *AGEStore) lockTenant(ctx context.Context, tx pgx.Tx, tenant string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "outcomegraph:"+tenant)
	return mapAGEErr(err)
}

func (s *AGEStore) validateBatch(tenant string, events []ProjectionEvent) error {
	for i := range events {
		if events[i].Tenant != tenant {
			return fmt.Errorf("%w: event tenant %q != batch tenant %q", ErrCrossTenantEdge, events[i].Tenant, tenant)
		}
		if err := events[i].Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s *AGEStore) ApplyBatch(ctx context.Context, tenant string, events []ProjectionEvent) error {
	if err := s.validateBatch(tenant, events); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapAGEErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockTenant(ctx, tx, tenant); err != nil {
		return err
	}
	for i := range events {
		if err := s.applyEventTx(ctx, tx, events[i]); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return mapAGEErr(err)
	}
	return nil
}

func (s *AGEStore) ProjectBatch(ctx context.Context, tenant string, events []ProjectionEvent) (uint64, error) {
	if err := s.validateBatch(tenant, events); err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, mapAGEErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialize the ENTIRE read-watermark -> apply -> advance unit per tenant.
	if err := s.lockTenant(ctx, tx, tenant); err != nil {
		return 0, err
	}
	var wm int64
	switch err := tx.QueryRow(ctx, `SELECT last_sequence FROM outcome_graph_watermark WHERE tenant=$1`, tenant).Scan(&wm); {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		wm = 0
	default:
		return 0, mapAGEErr(err)
	}
	last := uint64(wm)
	for i := range events {
		if events[i].Sequence <= uint64(wm) { // already applied — skip (self-correcting)
			continue
		}
		if err := s.applyEventTx(ctx, tx, events[i]); err != nil {
			return 0, err
		}
		if events[i].Sequence > last {
			last = events[i].Sequence
		}
	}
	// Advance the watermark WITHIN the same locked transaction: it commits atomically
	// with the graph writes it accounts for.
	if _, err := tx.Exec(ctx, `INSERT INTO outcome_graph_watermark(tenant,last_sequence,updated_at) VALUES ($1,$2,now())
		ON CONFLICT (tenant) DO UPDATE SET last_sequence=EXCLUDED.last_sequence, updated_at=now()
		WHERE outcome_graph_watermark.last_sequence <= EXCLUDED.last_sequence`, tenant, int64(last)); err != nil {
		return 0, mapAGEErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, mapAGEErr(err)
	}
	return last, nil
}

func (s *AGEStore) Watermark(ctx context.Context, tenant string) (uint64, error) {
	var last int64
	err := s.pool.QueryRow(ctx, `SELECT last_sequence FROM outcome_graph_watermark WHERE tenant=$1`, tenant).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, mapAGEErr(err)
	}
	return uint64(last), nil
}

func (s *AGEStore) AdvanceWatermark(ctx context.Context, tenant string, toSeq uint64) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO outcome_graph_watermark(tenant,last_sequence,updated_at)
		VALUES ($1,$2,now())
		ON CONFLICT (tenant) DO UPDATE SET last_sequence=EXCLUDED.last_sequence, updated_at=now()
		WHERE outcome_graph_watermark.last_sequence <= EXCLUDED.last_sequence`, tenant, int64(toSeq))
	if err != nil {
		return mapAGEErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrWatermarkRegression
	}
	return nil
}

func (s *AGEStore) Rebuild(ctx context.Context, tenant string, events []ProjectionEvent) error {
	if err := s.validateBatch(tenant, events); err != nil {
		return err
	}
	// Wipe + replay + watermark reset are ONE transactional unit under the
	// per-tenant lock: a crash mid-rebuild rolls back to the prior graph (never an
	// empty graph with a stale-high watermark the incremental projector would not
	// refill), and it can never interleave with a concurrent projection. The
	// per-tenant DETACH DELETE is isolated because edges never cross tenants.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapAGEErr(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockTenant(ctx, tx, tenant); err != nil {
		return err
	}
	if err := s.execCypher(ctx, tx, `MATCH (n) WHERE n.tenant = $tenant DETACH DELETE n`, map[string]any{"tenant": tenant}); err != nil {
		return err
	}
	var last uint64
	for i := range events {
		if err := s.applyEventTx(ctx, tx, events[i]); err != nil {
			return err
		}
		if events[i].Sequence > last {
			last = events[i].Sequence
		}
	}
	// Reset the watermark to the last replayed sequence (force, not forward-only),
	// in the SAME transaction as the wipe+replay.
	if _, err := tx.Exec(ctx, `INSERT INTO outcome_graph_watermark(tenant,last_sequence,updated_at) VALUES ($1,$2,now())
		ON CONFLICT (tenant) DO UPDATE SET last_sequence=EXCLUDED.last_sequence, updated_at=now()`, tenant, int64(last)); err != nil {
		return mapAGEErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mapAGEErr(err)
	}
	return nil
}

// --- read side -------------------------------------------------------------

// orgClause returns the org-isolation WHERE fragment and whether an org param is
// needed. An org-scoped query sees org-matching and tenant-global (null/empty)
// nodes, never another org's — the org boundary. Tenant-scoped (org == "") adds
// no org fragment. The fragment references only the bound $org param.
func orgClause(varName, org string) string {
	if org == "" {
		return ""
	}
	return fmt.Sprintf(" AND (%[1]s.org = $org OR %[1]s.org IS NULL OR %[1]s.org = '')", varName)
}

func (s *AGEStore) runNodeQuery(ctx context.Context, template string, params map[string]any, maxRows int) ([]ResultNode, bool, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, false, err
	}
	stmt := fmt.Sprintf(`SELECT * FROM cypher('%s', $cy$ %s $cy$, $1) AS (label agtype, org agtype, business_id agtype, revision agtype, summary agtype, hash agtype, revoked agtype, superseded agtype)`, s.graphName, template)
	rows, err := s.pool.Query(ctx, stmt, string(raw))
	if err != nil {
		return nil, false, mapAGEErr(err)
	}
	defer rows.Close()
	var out []ResultNode
	truncated := false
	for rows.Next() {
		if len(out) >= maxRows {
			truncated = true
			break
		}
		var label, org, bid, rev, summary, hash, revoked, superseded *string
		if err := rows.Scan(&label, &org, &bid, &rev, &summary, &hash, &revoked, &superseded); err != nil {
			return nil, false, err
		}
		out = append(out, ResultNode{
			Label: NodeLabel(agString(ds(label))), Org: agString(ds(org)), BusinessID: agString(ds(bid)),
			Revision: agUint(ds(rev)), Summary: agString(ds(summary)), Hash: agString(ds(hash)),
			Revoked: agBool(ds(revoked)), Superseded: agBool(ds(superseded)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapAGEErr(err)
	}
	// Deterministic order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		if out[i].BusinessID != out[j].BusinessID {
			return out[i].BusinessID < out[j].BusinessID
		}
		return out[i].Revision < out[j].Revision
	})
	return out, truncated, nil
}

func (s *AGEStore) envelope(ctx context.Context, tenant string, srcRev uint64, nodes []ResultNode, truncated bool) (QueryResult, error) {
	wm, err := s.Watermark(ctx, tenant)
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{Nodes: nodes, SourceRevision: srcRev, Watermark: wm, Truncated: truncated}, nil
}

func (s *AGEStore) GetLineage(ctx context.Context, q LineageQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	b, err := q.Budget.Resolve()
	if err != nil {
		return QueryResult{}, err
	}
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	arrow := "-[*1..%d]->"
	if q.Direction == DirIn {
		arrow = "<-[*1..%d]-"
	}
	rel := fmt.Sprintf(arrow, b.MaxDepth)
	tmpl := fmt.Sprintf(`MATCH (a:%s {nid: $anchor}) MATCH (a)%s(b) WHERE a.tenant = $tenant%s AND b.tenant = $tenant%s RETURN DISTINCT label(b), b.org, b.business_id, b.revision, b.summary, b.hash, b.revoked, b.superseded ORDER BY b.business_id, b.revision LIMIT %d`,
		string(q.Anchor.Label), rel, orgClause("a", q.Org), orgClause("b", q.Org), b.MaxRows+1)
	params := map[string]any{"anchor": q.Anchor.NID(q.Tenant), "tenant": q.Tenant}
	if q.Org != "" {
		params["org"] = q.Org
	}
	nodes, truncated, err := s.runNodeQuery(ctx, tmpl, params, b.MaxRows)
	if err != nil {
		return QueryResult{}, mapDeadline(ctx, err)
	}
	return s.envelope(ctx, q.Tenant, q.Anchor.Revision, nodes, truncated)
}

func (s *AGEStore) TraceImpact(ctx context.Context, q ImpactQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	b, err := q.Budget.Resolve()
	if err != nil {
		return QueryResult{}, err
	}
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	rel := fmt.Sprintf("<-[*1..%d]-", b.MaxDepth)
	tmpl := fmt.Sprintf(`MATCH (a:%s {nid: $anchor}) MATCH (a)%s(b) WHERE a.tenant = $tenant%s AND b.tenant = $tenant%s RETURN DISTINCT label(b), b.org, b.business_id, b.revision, b.summary, b.hash, b.revoked, b.superseded ORDER BY b.business_id, b.revision LIMIT %d`,
		string(q.Anchor.Label), rel, orgClause("a", q.Org), orgClause("b", q.Org), b.MaxRows+1)
	params := map[string]any{"anchor": q.Anchor.NID(q.Tenant), "tenant": q.Tenant}
	if q.Org != "" {
		params["org"] = q.Org
	}
	nodes, truncated, err := s.runNodeQuery(ctx, tmpl, params, b.MaxRows)
	if err != nil {
		return QueryResult{}, mapDeadline(ctx, err)
	}
	return s.envelope(ctx, q.Tenant, q.Anchor.Revision, nodes, truncated)
}

func (s *AGEStore) FindRelatedCases(ctx context.Context, q RelatedCasesQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	b, err := q.Budget.Resolve()
	if err != nil {
		return QueryResult{}, err
	}
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	// case <- Outcome -CONCERNS-> Goal <-CONCERNS- Outcome -DERIVED_FROM-> other case.
	// The ANCHOR (a) is tenant/org-filtered too, so an org-scoped query can never
	// traverse FROM a foreign-org anchor.
	tmpl := fmt.Sprintf(`MATCH (a:WorkCase {nid: $anchor})<-[:DERIVED_FROM]-(:Outcome)-[:CONCERNS]->(g:Goal)<-[:CONCERNS]-(:Outcome)-[:DERIVED_FROM]->(b:WorkCase) WHERE a.tenant = $tenant%s AND b.tenant = $tenant AND b.nid <> $anchor%s RETURN DISTINCT label(b), b.org, b.business_id, b.revision, b.summary, b.hash, b.revoked, b.superseded ORDER BY b.business_id, b.revision LIMIT %d`,
		orgClause("a", q.Org), orgClause("b", q.Org), b.MaxRows+1)
	params := map[string]any{"anchor": q.Anchor.NID(q.Tenant), "tenant": q.Tenant}
	if q.Org != "" {
		params["org"] = q.Org
	}
	nodes, truncated, err := s.runNodeQuery(ctx, tmpl, params, b.MaxRows)
	if err != nil {
		return QueryResult{}, mapDeadline(ctx, err)
	}
	return s.envelope(ctx, q.Tenant, q.Anchor.Revision, nodes, truncated)
}

func (s *AGEStore) QueryMethods(ctx context.Context, q MethodsQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	b, err := q.Budget.Resolve()
	if err != nil {
		return QueryResult{}, err
	}
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	satisfied := ""
	if q.OnlySatisfied {
		satisfied = " AND (o.revoked IS NULL OR o.revoked = false) AND (o.superseded IS NULL OR o.superseded = false)"
	}
	tmpl := fmt.Sprintf(`MATCH (g:Goal {nid: $anchor})<-[:CONCERNS]-(o:Outcome)-[:CONTRIBUTES_TO]->(b:Contributor) WHERE g.tenant = $tenant%s AND b.tenant = $tenant%s%s RETURN DISTINCT label(b), b.org, b.business_id, b.revision, b.summary, b.hash, b.revoked, b.superseded ORDER BY b.business_id, b.revision LIMIT %d`,
		orgClause("g", q.Org), satisfied, orgClause("b", q.Org), b.MaxRows+1)
	params := map[string]any{"anchor": q.Goal.NID(q.Tenant), "tenant": q.Tenant}
	if q.Org != "" {
		params["org"] = q.Org
	}
	nodes, truncated, err := s.runNodeQuery(ctx, tmpl, params, b.MaxRows)
	if err != nil {
		return QueryResult{}, mapDeadline(ctx, err)
	}
	return s.envelope(ctx, q.Tenant, q.Goal.Revision, nodes, truncated)
}

// mapDeadline converts a context-deadline error into the explicit query-budget
// cancellation error.
func mapDeadline(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrQueryDeadline, err)
	}
	return err
}

// MaxDuplication returns the maximum number of vertices sharing any single nid,
// and the maximum number of edges sharing any single (edge label, from-nid,
// to-nid) key, for a tenant. Both are 1 in a correctly-projected graph; a value
// > 1 means concurrent, un-serialized projection created duplicate
// vertices/edges (AGE MERGE is not concurrency-safe without a unique constraint).
// Used by the concurrency test and as an ops integrity probe.
func (s *AGEStore) MaxDuplication(ctx context.Context, tenant string) (int, int, error) {
	raw, _ := json.Marshal(map[string]any{"tenant": tenant})
	maxOf := func(stmt string, cols int) (int, error) {
		rows, err := s.pool.Query(ctx, stmt, string(raw))
		if err != nil {
			return 0, mapAGEErr(err)
		}
		defer rows.Close()
		best := 0
		dest := make([]any, cols)
		holders := make([]*string, cols)
		for i := range holders {
			dest[i] = &holders[i]
		}
		for rows.Next() {
			if err := rows.Scan(dest...); err != nil {
				return 0, err
			}
			if c := int(agUint(ds(holders[cols-1]))); c > best {
				best = c
			}
		}
		return best, rows.Err()
	}
	vstmt := fmt.Sprintf(`SELECT * FROM cypher('%s', $cy$ MATCH (n) WHERE n.tenant = $tenant RETURN n.nid, count(*) $cy$, $1) AS (nid agtype, c agtype)`, s.graphName)
	maxV, err := maxOf(vstmt, 2)
	if err != nil {
		return 0, 0, err
	}
	estmt := fmt.Sprintf(`SELECT * FROM cypher('%s', $cy$ MATCH (a)-[e]->(b) WHERE a.tenant = $tenant RETURN label(e), a.nid, b.nid, count(*) $cy$, $1) AS (t agtype, f agtype, tn agtype, c agtype)`, s.graphName)
	maxE, err := maxOf(estmt, 4)
	if err != nil {
		return 0, 0, err
	}
	return maxV, maxE, nil
}

// Digest returns a deterministic content digest of the tenant's graph as stored
// in AGE. It is used ONLY for AGE-to-AGE comparison (incremental vs. rebuilt);
// it is NOT comparable to MemoryGraph.Digest (different field/edge encodings).
// DISTINCT collapses accidental exact-duplicate rows so the digest reflects the
// logical graph; use MaxDuplication to detect physical duplicates.
func (s *AGEStore) Digest(ctx context.Context, tenant string) (string, error) {
	// Nodes.
	nstmt := fmt.Sprintf(`SELECT * FROM cypher('%s', $cy$ MATCH (n) WHERE n.tenant = $tenant RETURN DISTINCT label(n), n.org, n.business_id, n.revision, n.summary, n.hash, n.revoked, n.superseded $cy$, $1) AS (label agtype, org agtype, business_id agtype, revision agtype, summary agtype, hash agtype, revoked agtype, superseded agtype)`, s.graphName)
	raw, _ := json.Marshal(map[string]any{"tenant": tenant})
	rows, err := s.pool.Query(ctx, nstmt, string(raw))
	if err != nil {
		return "", mapAGEErr(err)
	}
	type nd struct {
		NID, Label, Org, BID, Summary, Hash string
		Rev                                 uint64
		Revoked, Superseded                 bool
	}
	var nl []nd
	for rows.Next() {
		var label, org, bid, rev, summary, hash, revoked, superseded *string
		if err := rows.Scan(&label, &org, &bid, &rev, &summary, &hash, &revoked, &superseded); err != nil {
			rows.Close()
			return "", err
		}
		lbl := NodeLabel(agString(ds(label)))
		n := nd{NID: nid(tenant, lbl, agString(ds(bid)), agUint(ds(rev))), Label: string(lbl), Org: agString(ds(org)), BID: agString(ds(bid)), Summary: agString(ds(summary)), Hash: agString(ds(hash)), Rev: agUint(ds(rev)), Revoked: agBool(ds(revoked)), Superseded: agBool(ds(superseded))}
		nl = append(nl, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", mapAGEErr(err)
	}
	// Edges.
	estmt := fmt.Sprintf(`SELECT * FROM cypher('%s', $cy$ MATCH (a)-[e]->(b) WHERE a.tenant = $tenant RETURN DISTINCT label(e), a.business_id, a.revision, b.business_id, b.revision $cy$, $1) AS (etype agtype, abid agtype, arev agtype, bbid agtype, brev agtype)`, s.graphName)
	erows, err := s.pool.Query(ctx, estmt, string(raw))
	if err != nil {
		return "", mapAGEErr(err)
	}
	var el []string
	for erows.Next() {
		var etype, abid, arev, bbid, brev *string
		if err := erows.Scan(&etype, &abid, &arev, &bbid, &brev); err != nil {
			erows.Close()
			return "", err
		}
		el = append(el, fmt.Sprintf("%s|%s@%d->%s@%d", agString(ds(etype)), agString(ds(abid)), agUint(ds(arev)), agString(ds(bbid)), agUint(ds(brev))))
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		return "", mapAGEErr(err)
	}
	sort.Slice(nl, func(i, j int) bool { return nl[i].NID < nl[j].NID })
	sort.Strings(el)
	h := sha256.New()
	blob, _ := json.Marshal(struct {
		Nodes []nd
		Edges []string
	}{nl, el})
	h.Write(blob)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// ds safely dereferences a nullable scanned agtype column.
func ds(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// agtype scalar parsing.
func agString(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return ""
	}
	var out string
	if err := json.Unmarshal([]byte(s), &out); err == nil {
		return out
	}
	return strings.Trim(s, `"`)
}

func agBool(s string) bool { return strings.TrimSpace(s) == "true" }

func agUint(s string) uint64 {
	var n uint64
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &n); err == nil {
		return n
	}
	return 0
}

var _ OutcomeGraphStore = (*AGEStore)(nil)
