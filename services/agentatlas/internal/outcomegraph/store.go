package outcomegraph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Sentinel store errors.
var (
	// ErrGraphUnavailable is returned by every graph-dependent operation when
	// the graph read model is unreachable (e.g. AGE is down). It is EXPLICIT:
	// graph-dependent planning degrades with this clear error, never a silent
	// wrong answer. Authoritative PostgreSQL mutation and already-approved
	// execution are unaffected (they never touch the graph).
	ErrGraphUnavailable = errors.New("outcomegraph: graph read model unavailable")
	// ErrInvalidQuery is returned when a typed query's parameters are malformed
	// (missing tenant, unversioned/invalid anchor, ...).
	ErrInvalidQuery = errors.New("outcomegraph: invalid query")
)

// Direction selects a lineage traversal direction from the anchor: DirOut walks
// the anchor's basis (what it derives from / depends on); DirIn walks its
// dependents (what derives from / is affected by it).
type Direction string

const (
	DirOut Direction = "out"
	DirIn  Direction = "in"
)

// ResultNode is one opaque node in a query result. It exposes ONLY classified
// metadata: label, versioned business id, org scope, permitted summary, opaque
// hash, and the correction flags. No raw content, no graph-provider id.
type ResultNode struct {
	Label      NodeLabel `json:"label"`
	Org        string    `json:"org,omitempty"`
	BusinessID string    `json:"business_id"`
	Revision   uint64    `json:"revision"`
	Summary    string    `json:"summary,omitempty"`
	Hash       string    `json:"hash,omitempty"`
	Revoked    bool      `json:"revoked,omitempty"`
	Superseded bool      `json:"superseded,omitempty"`
}

// QueryResult is a bounded, opaque query answer that ALWAYS carries the freshness
// of the read model it came from: the source revision of the queried anchor and
// the projection watermark (how far the tenant's outbox has been projected). A
// caller therefore always knows the exact version/freshness of the answer.
type QueryResult struct {
	Nodes          []ResultNode `json:"nodes"`
	SourceRevision uint64       `json:"source_revision"`
	Watermark      uint64       `json:"watermark"`
	Truncated      bool         `json:"truncated"`
}

// --- typed query inputs ----------------------------------------------------

// LineageQuery walks lineage from an anchor node in one Direction, bounded by
// Budget, scoped to (Tenant, Org).
type LineageQuery struct {
	Tenant    string
	Org       string
	Anchor    NodeRef
	Direction Direction
	Budget    Budget
}

func (q LineageQuery) validate() error {
	if q.Tenant == "" {
		return fmt.Errorf("%w: tenant required", ErrInvalidQuery)
	}
	if q.Direction != DirOut && q.Direction != DirIn {
		return fmt.Errorf("%w: direction must be in or out", ErrInvalidQuery)
	}
	if err := q.Anchor.Validate(); err != nil {
		return fmt.Errorf("%w: anchor: %v", ErrInvalidQuery, err)
	}
	return nil
}

// RelatedCasesQuery finds other WorkCases related to the anchor WorkCase through
// a shared Goal (anchor -> Outcome -> Goal <- Outcome -> WorkCase).
type RelatedCasesQuery struct {
	Tenant string
	Org    string
	Anchor NodeRef // a WorkCase
	Budget Budget
}

func (q RelatedCasesQuery) validate() error {
	if q.Tenant == "" {
		return fmt.Errorf("%w: tenant required", ErrInvalidQuery)
	}
	if err := q.Anchor.Validate(); err != nil {
		return fmt.Errorf("%w: anchor: %v", ErrInvalidQuery, err)
	}
	return nil
}

// ImpactQuery traces the dependents affected by the anchor (a Blocker or
// Evidence): every node with a path INTO the anchor, bounded by Budget.
type ImpactQuery struct {
	Tenant string
	Org    string
	Anchor NodeRef
	Budget Budget
}

func (q ImpactQuery) validate() error {
	if q.Tenant == "" {
		return fmt.Errorf("%w: tenant required", ErrInvalidQuery)
	}
	if err := q.Anchor.Validate(); err != nil {
		return fmt.Errorf("%w: anchor: %v", ErrInvalidQuery, err)
	}
	return nil
}

// MethodsQuery finds the Contributor (method) nodes that contributed to Outcomes
// concerning the anchor Goal. OnlySatisfied restricts to methods that
// contributed to a satisfied (non-revoked, non-superseded) Outcome.
type MethodsQuery struct {
	Tenant        string
	Org           string
	Goal          NodeRef
	OnlySatisfied bool
	Budget        Budget
}

func (q MethodsQuery) validate() error {
	if q.Tenant == "" {
		return fmt.Errorf("%w: tenant required", ErrInvalidQuery)
	}
	if err := q.Goal.Validate(); err != nil {
		return fmt.Errorf("%w: goal: %v", ErrInvalidQuery, err)
	}
	return nil
}

// OutcomeGraphStore is the provider-neutral Outcome-Graph read model. Apache AGE
// is the only GA provider; MemoryGraph is the in-memory implementation unit
// tests run against. Every method is tenant-scoped; no method can traverse
// across a tenant or org boundary. The write side (ApplyBatch/Rebuild) is fed
// ONLY by the authoritative projection outbox, never a direct caller write.
type OutcomeGraphStore interface {
	// ApplyBatch idempotently MERGEs a batch of ordered projection events into
	// the tenant's graph in ONE graph transaction (all-or-nothing), under the
	// per-tenant projection lock. Re-applying the same events is a no-op
	// (deterministic merge keys) — safe under duplicate, out-of-order and
	// redelivered delivery. It does NOT advance the watermark.
	ApplyBatch(ctx context.Context, tenant string, events []ProjectionEvent) error

	// ProjectBatch is the projector's atomic, per-tenant-SERIALIZED unit: under a
	// per-tenant lock held in the graph store, it reads the current watermark,
	// applies only the events whose sequence exceeds it, and advances the
	// watermark — all as ONE unit. Concurrent ProjectBatch calls for the same
	// tenant run strictly serially (an advisory lock in the graph database, so it
	// holds across replicas — not merely an in-process mutex), so the idempotent
	// MERGE can never race into duplicate vertices/edges and the read-apply-advance
	// window is never interleaved. Returns the watermark after applying.
	ProjectBatch(ctx context.Context, tenant string, events []ProjectionEvent) (uint64, error)

	// Watermark returns how far the tenant's outbox has been projected into the
	// graph (0 if nothing yet).
	Watermark(ctx context.Context, tenant string) (uint64, error)

	// AdvanceWatermark moves the tenant's projection watermark forward. A
	// regression is rejected; re-setting the same value is idempotent.
	AdvanceWatermark(ctx context.Context, tenant string, toSeq uint64) error

	// Rebuild drops the tenant's graph and replays events from scratch, then
	// sets the watermark to the last replayed sequence. The result is IDENTICAL
	// to the incrementally-projected graph (deterministic merge keys).
	Rebuild(ctx context.Context, tenant string, events []ProjectionEvent) error

	GetLineage(ctx context.Context, q LineageQuery) (QueryResult, error)
	FindRelatedCases(ctx context.Context, q RelatedCasesQuery) (QueryResult, error)
	TraceImpact(ctx context.Context, q ImpactQuery) (QueryResult, error)
	QueryMethods(ctx context.Context, q MethodsQuery) (QueryResult, error)
}

// ErrWatermarkRegression is returned when AdvanceWatermark is asked to move a
// tenant's watermark backwards.
var ErrWatermarkRegression = errors.New("outcomegraph: projection watermark must not move backwards")

// --- in-memory provider ----------------------------------------------------

type memNode struct {
	node       GraphNode
	revoked    bool
	superseded bool
}

// MemoryGraph is an in-memory OutcomeGraphStore enforcing exactly the GA
// contract: idempotent MERGE on deterministic keys, forward-only watermark,
// drop/rebuild equality, tenant/org isolation, budget enforcement and opaque
// results. Unit tests exercise real projection and query behavior against it;
// the AGE integration leg re-proves the same properties on real Apache AGE.
type MemoryGraph struct {
	mu          sync.Mutex
	nodes       map[string]map[string]memNode   // tenant -> nid -> node
	edges       map[string]map[string]GraphEdge // tenant -> eid -> edge
	watermark   map[string]uint64
	unavailable bool
}

// NewMemoryGraph constructs an empty in-memory graph.
func NewMemoryGraph() *MemoryGraph {
	return &MemoryGraph{
		nodes:     map[string]map[string]memNode{},
		edges:     map[string]map[string]GraphEdge{},
		watermark: map[string]uint64{},
	}
}

// SetUnavailable simulates an AGE outage/recovery for degradation tests.
func (g *MemoryGraph) SetUnavailable(down bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unavailable = down
}

func (g *MemoryGraph) applyEvent(tenant string, ev ProjectionEvent) {
	if g.nodes[tenant] == nil {
		g.nodes[tenant] = map[string]memNode{}
	}
	if g.edges[tenant] == nil {
		g.edges[tenant] = map[string]GraphEdge{}
	}
	switch ev.Kind {
	case KindUpsert:
		for _, n := range ev.Delta.Nodes {
			id := n.NID()
			cur, ok := g.nodes[tenant][id]
			// MERGE: create if absent; preserve correction flags on re-apply so a
			// later upsert can never un-revoke an already-revoked node.
			cur.node = n
			if !ok {
				cur.revoked, cur.superseded = false, false
			}
			g.nodes[tenant][id] = cur
		}
		for _, e := range ev.Delta.Edges {
			g.edges[tenant][e.EID()] = e
			// Ensure both endpoints exist as (at least) minimal nodes, matching
			// the AGE mergeEdgeTemplate. Create-if-absent NEVER clobbers an
			// already-projected full node (so a domain-owned node keeps its org).
			for _, ref := range []NodeRef{e.From, e.To} {
				id := ref.NID(tenant)
				if _, ok := g.nodes[tenant][id]; !ok {
					g.nodes[tenant][id] = memNode{node: GraphNode{Tenant: tenant, Label: ref.Label, BusinessID: ref.BusinessID, Revision: ref.Revision}}
				}
			}
		}
	case KindTombstone, KindReevaluation:
		if ev.Delta.Mark == nil {
			return
		}
		id := ev.Delta.Mark.NID(tenant)
		cur, ok := g.nodes[tenant][id]
		if !ok {
			// The target may not have been projected yet (out-of-order); create a
			// minimal placeholder so the correction is not lost, MERGE-safe.
			cur = memNode{node: GraphNode{Tenant: tenant, Label: ev.Delta.Mark.Label, BusinessID: ev.Delta.Mark.BusinessID, Revision: ev.Delta.Mark.Revision}}
		}
		if ev.Kind == KindTombstone {
			cur.revoked = true
		} else {
			cur.superseded = true
		}
		g.nodes[tenant][id] = cur
	}
}

func (g *MemoryGraph) ApplyBatch(ctx context.Context, tenant string, events []ProjectionEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unavailable {
		return ErrGraphUnavailable
	}
	// Validate the whole batch BEFORE mutating so it is all-or-nothing like a
	// single AGE transaction.
	for i := range events {
		if events[i].Tenant != tenant {
			return fmt.Errorf("%w: event tenant %q != batch tenant %q", ErrCrossTenantEdge, events[i].Tenant, tenant)
		}
		if err := events[i].Validate(); err != nil {
			return err
		}
	}
	for i := range events {
		g.applyEvent(tenant, events[i])
	}
	return nil
}

func (g *MemoryGraph) ProjectBatch(ctx context.Context, tenant string, events []ProjectionEvent) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for i := range events {
		if events[i].Tenant != tenant {
			return 0, fmt.Errorf("%w: event tenant %q != batch tenant %q", ErrCrossTenantEdge, events[i].Tenant, tenant)
		}
		if err := events[i].Validate(); err != nil {
			return 0, err
		}
	}
	// The single mutex serializes per-tenant projection in-process (the AGE
	// provider uses a graph-database advisory lock so serialization also holds
	// across replicas). read-watermark -> apply -> advance is ONE critical section.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unavailable {
		return 0, ErrGraphUnavailable
	}
	wm := g.watermark[tenant]
	last := wm
	for i := range events {
		if events[i].Sequence <= wm { // already applied (self-correcting under concurrency)
			continue
		}
		g.applyEvent(tenant, events[i])
		if events[i].Sequence > last {
			last = events[i].Sequence
		}
	}
	g.watermark[tenant] = last
	return last, nil
}

func (g *MemoryGraph) Watermark(ctx context.Context, tenant string) (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unavailable {
		return 0, ErrGraphUnavailable
	}
	return g.watermark[tenant], nil
}

func (g *MemoryGraph) AdvanceWatermark(ctx context.Context, tenant string, toSeq uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unavailable {
		return ErrGraphUnavailable
	}
	if toSeq < g.watermark[tenant] {
		return ErrWatermarkRegression
	}
	g.watermark[tenant] = toSeq
	return nil
}

func (g *MemoryGraph) Rebuild(ctx context.Context, tenant string, events []ProjectionEvent) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unavailable {
		return ErrGraphUnavailable
	}
	for i := range events {
		if events[i].Tenant != tenant {
			return fmt.Errorf("%w: event tenant %q != rebuild tenant %q", ErrCrossTenantEdge, events[i].Tenant, tenant)
		}
		if err := events[i].Validate(); err != nil {
			return err
		}
	}
	// Drop then replay.
	delete(g.nodes, tenant)
	delete(g.edges, tenant)
	var last uint64
	for i := range events {
		g.applyEvent(tenant, events[i])
		if events[i].Sequence > last {
			last = events[i].Sequence
		}
	}
	g.watermark[tenant] = last
	return nil
}

// Digest returns a deterministic content digest of the tenant's graph (sorted
// nodes + edges + correction flags). Two graphs with the same digest are
// identical — the rebuild-equality proof.
func (g *MemoryGraph) Digest(tenant string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return graphDigest(g.nodes[tenant], g.edges[tenant])
}

// --- traversal -------------------------------------------------------------

// visibleInOrg reports whether a node is visible to a query scoped to org. A
// tenant-scoped query (org == "") sees every node in the tenant. An org-scoped
// query sees org-matching nodes and tenant-global (org == "") nodes, but NEVER
// a node belonging to a different org — the org-isolation boundary.
func visibleInOrg(n GraphNode, org string) bool {
	if org == "" {
		return true
	}
	return n.Org == "" || n.Org == org
}

func toResult(n memNode) ResultNode {
	return ResultNode{
		Label: n.node.Label, Org: n.node.Org, BusinessID: n.node.BusinessID,
		Revision: n.node.Revision, Summary: n.node.Summary, Hash: n.node.Hash,
		Revoked: n.revoked, Superseded: n.superseded,
	}
}

// neighbors returns the neighbor node ids reachable from nid in the given
// direction, staying within the org boundary.
func (g *MemoryGraph) neighbors(tenant, nid string, dir Direction, org string) []string {
	var out []string
	for _, e := range g.edges[tenant] {
		var from, to string
		from, to = e.From.NID(tenant), e.To.NID(tenant)
		var next string
		switch dir {
		case DirOut:
			if from == nid {
				next = to
			}
		case DirIn:
			if to == nid {
				next = from
			}
		}
		if next == "" {
			continue
		}
		if n, ok := g.nodes[tenant][next]; ok && visibleInOrg(n.node, org) {
			out = append(out, next)
		}
	}
	return out
}

// bfs walks up to maxDepth hops from startNID in dir, returning the reachable
// nodes (excluding the anchor), bounded by maxRows. Deterministic order.
func (g *MemoryGraph) bfs(tenant, startNID string, dir Direction, org string, maxDepth, maxRows int) ([]memNode, bool) {
	seen := map[string]bool{startNID: true}
	frontier := []string{startNID}
	var collected []string
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, cur := range frontier {
			for _, nb := range g.neighbors(tenant, cur, dir, org) {
				if seen[nb] {
					continue
				}
				seen[nb] = true
				collected = append(collected, nb)
				next = append(next, nb)
			}
		}
		frontier = next
	}
	sort.Strings(collected)
	truncated := false
	if len(collected) > maxRows {
		collected = collected[:maxRows]
		truncated = true
	}
	out := make([]memNode, 0, len(collected))
	for _, id := range collected {
		out = append(out, g.nodes[tenant][id])
	}
	return out, truncated
}

func (g *MemoryGraph) preflight(ctx context.Context, tenant string, b Budget) (Budget, error) {
	if err := ctx.Err(); err != nil {
		return Budget{}, err
	}
	rb, err := b.Resolve()
	if err != nil {
		return Budget{}, err
	}
	if g.unavailable {
		return Budget{}, ErrGraphUnavailable
	}
	return rb, nil
}

func (g *MemoryGraph) GetLineage(ctx context.Context, q LineageQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rb, err := g.preflight(ctx, q.Tenant, q.Budget)
	if err != nil {
		return QueryResult{}, err
	}
	start := q.Anchor.NID(q.Tenant)
	if an, ok := g.nodes[q.Tenant][start]; !ok || !visibleInOrg(an.node, q.Org) {
		return QueryResult{Watermark: g.watermark[q.Tenant], SourceRevision: q.Anchor.Revision}, nil
	}
	nodes, truncated := g.bfs(q.Tenant, start, q.Direction, q.Org, rb.MaxDepth, rb.MaxRows)
	return g.envelope(q.Tenant, q.Anchor.Revision, nodes, truncated), nil
}

func (g *MemoryGraph) TraceImpact(ctx context.Context, q ImpactQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rb, err := g.preflight(ctx, q.Tenant, q.Budget)
	if err != nil {
		return QueryResult{}, err
	}
	start := q.Anchor.NID(q.Tenant)
	if an, ok := g.nodes[q.Tenant][start]; !ok || !visibleInOrg(an.node, q.Org) {
		return QueryResult{Watermark: g.watermark[q.Tenant], SourceRevision: q.Anchor.Revision}, nil
	}
	// Impact = dependents = nodes with a path INTO the anchor.
	nodes, truncated := g.bfs(q.Tenant, start, DirIn, q.Org, rb.MaxDepth, rb.MaxRows)
	return g.envelope(q.Tenant, q.Anchor.Revision, nodes, truncated), nil
}

func (g *MemoryGraph) FindRelatedCases(ctx context.Context, q RelatedCasesQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rb, err := g.preflight(ctx, q.Tenant, q.Budget)
	if err != nil {
		return QueryResult{}, err
	}
	start := q.Anchor.NID(q.Tenant)
	if an, ok := g.nodes[q.Tenant][start]; !ok || !visibleInOrg(an.node, q.Org) {
		return QueryResult{Watermark: g.watermark[q.Tenant], SourceRevision: q.Anchor.Revision}, nil
	}
	// anchor WorkCase <- Outcome(derived_from) -> Goal(concerns); then Goal <-
	// Outcome(concerns) -> WorkCase(derived_from). Collect the sibling WorkCases.
	related := map[string]memNode{}
	for _, outID := range g.neighbors(q.Tenant, start, DirIn, q.Org) { // outcomes derived_from this case
		out := g.nodes[q.Tenant][outID]
		if out.node.Label != LabelOutcome {
			continue
		}
		for _, goalID := range g.neighbors(q.Tenant, outID, DirOut, q.Org) {
			goal := g.nodes[q.Tenant][goalID]
			if goal.node.Label != LabelGoal {
				continue
			}
			for _, sibOutID := range g.neighbors(q.Tenant, goalID, DirIn, q.Org) {
				sibOut := g.nodes[q.Tenant][sibOutID]
				if sibOut.node.Label != LabelOutcome {
					continue
				}
				for _, caseID := range g.neighbors(q.Tenant, sibOutID, DirOut, q.Org) {
					c := g.nodes[q.Tenant][caseID]
					if c.node.Label == LabelWorkCase && caseID != start {
						related[caseID] = c
					}
				}
			}
		}
	}
	return g.envelopeMap(q.Tenant, q.Anchor.Revision, related, rb.MaxRows), nil
}

func (g *MemoryGraph) QueryMethods(ctx context.Context, q MethodsQuery) (QueryResult, error) {
	if err := q.validate(); err != nil {
		return QueryResult{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	rb, err := g.preflight(ctx, q.Tenant, q.Budget)
	if err != nil {
		return QueryResult{}, err
	}
	start := q.Goal.NID(q.Tenant)
	if an, ok := g.nodes[q.Tenant][start]; !ok || !visibleInOrg(an.node, q.Org) {
		return QueryResult{Watermark: g.watermark[q.Tenant], SourceRevision: q.Goal.Revision}, nil
	}
	methods := map[string]memNode{}
	for _, outID := range g.neighbors(q.Tenant, start, DirIn, q.Org) { // outcomes concerning the goal
		out := g.nodes[q.Tenant][outID]
		if out.node.Label != LabelOutcome {
			continue
		}
		if q.OnlySatisfied && (out.revoked || out.superseded) {
			continue
		}
		for _, cID := range g.neighbors(q.Tenant, outID, DirOut, q.Org) {
			c := g.nodes[q.Tenant][cID]
			if c.node.Label == LabelContributor {
				methods[cID] = c
			}
		}
	}
	return g.envelopeMap(q.Tenant, q.Goal.Revision, methods, rb.MaxRows), nil
}

func (g *MemoryGraph) envelope(tenant string, srcRev uint64, nodes []memNode, truncated bool) QueryResult {
	res := QueryResult{Watermark: g.watermark[tenant], SourceRevision: srcRev, Truncated: truncated}
	for _, n := range nodes {
		res.Nodes = append(res.Nodes, toResult(n))
	}
	return res
}

func (g *MemoryGraph) envelopeMap(tenant string, srcRev uint64, m map[string]memNode, maxRows int) QueryResult {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	truncated := false
	if len(ids) > maxRows {
		ids = ids[:maxRows]
		truncated = true
	}
	res := QueryResult{Watermark: g.watermark[tenant], SourceRevision: srcRev, Truncated: truncated}
	for _, id := range ids {
		res.Nodes = append(res.Nodes, toResult(m[id]))
	}
	return res
}

var _ OutcomeGraphStore = (*MemoryGraph)(nil)
