// Package outcomegraph builds and serves the Task 0I Outcome Graph: a
// PROJECTED, REBUILDABLE read model over the provable Outcome lineage frozen by
// Tasks 0G/0H. It is NEVER an authoritative store and NEVER receives a write
// that did not originate from the authoritative-PostgreSQL projection outbox.
//
// The architecture is a single transactional outbox with an asynchronous,
// idempotent projector:
//
//   - The four authoritative stores (workcase, operatingmap, governance,
//     outcome) each append a canonical ProjectionEvent into ONE shared outbox
//     table (db/migrations/000017_outcome_graph_outbox.sql) WITHIN THE SAME
//     PostgreSQL transaction as their own domain write. The only synchronous
//     write is to authoritative PostgreSQL (domain row + outbox row, atomically);
//     there is no PostgreSQL/AGE dual write.
//   - The projector (projector.go) consumes the outbox through NATS/outbox,
//     applies the canonical GraphDelta payload to Apache AGE via parameterized
//     Cypher MERGE on DETERMINISTIC merge keys (idempotent under duplicate,
//     out-of-order and redelivered events), and advances a per-tenant watermark
//     ONLY after the AGE write commits.
//   - The query service (query.go, budget.go) exposes ONLY typed, parameterized,
//     tenant/org-scoped, bounded operations. No arbitrary Cypher is ever
//     accepted, and every response carries the source revision and projection
//     watermark of the read model it came from.
//
// Every graph node/edge carries only classified metadata, hashes, opaque
// references and permitted summaries — NEVER raw enterprise content, connector
// endpoints, credentials or full receipts. NoRawContentLeak (reused from
// sdk/go/outcome) enforces that boundary on every projected node.
//
// Apache AGE is the only GA provider; the OutcomeGraphStore interface is
// provider-neutral so unit tests run against an in-memory graph and only the
// integration leg needs real AGE.
package outcomegraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	outcomemodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

// Provider/compatibility constants, consistent with the enterprise
// contracts/ga-manifest.yaml outcome_graph section (Task 1A). They are asserted,
// not re-pinned, here — the manifest remains the single source of truth.
const (
	// ProviderName is the only GA Outcome Graph provider.
	ProviderName = "apache-age"
	// GraphPostgresMajor / AGERelease pin the GA tuple the projector expects.
	GraphPostgresMajor = 17
	AGERelease         = "1.6.0"
	// GraphSchemaVersion / ProjectionProtocolVersion mirror the manifest.
	GraphSchemaVersion        = "1"
	ProjectionProtocolVersion = "1"
	// DefaultGraphName is the AGE graph name. AGE rejects graph names shorter
	// than 3 characters, so a descriptive name is used (never "g").
	DefaultGraphName = "outcomegraph"
)

// --- source domains --------------------------------------------------------

// Source is the authoritative domain that emitted a projection event. It is a
// closed set: the four stores that own governed facts the Outcome Graph
// projects.
type Source string

const (
	SourceWorkCase     Source = "workcase"
	SourceOperatingMap Source = "operatingmap"
	SourceGovernance   Source = "governance"
	SourceOutcome      Source = "outcome"
)

// Valid reports whether s is a known source domain.
func (s Source) Valid() bool {
	switch s {
	case SourceWorkCase, SourceOperatingMap, SourceGovernance, SourceOutcome:
		return true
	}
	return false
}

// --- closed graph vocabulary -----------------------------------------------

// NodeLabel is the closed, versioned Outcome-Graph node vocabulary. It is a
// SUPERSET of the sdk/go/outcome lineage node types (which cover the outcome
// domain) plus the two labels the operating-map and governance domains
// contribute. The AGE graph is a 0I-owned, rebuildable read model, so its label
// vocabulary is owned here; this does NOT mutate any frozen SDK type.
type NodeLabel string

const (
	LabelOutcome          NodeLabel = "Outcome"
	LabelGoal             NodeLabel = "Goal"
	LabelWorkCase         NodeLabel = "WorkCase"
	LabelObservation      NodeLabel = "Observation"
	LabelActionReceipt    NodeLabel = "ActionReceipt"
	LabelBlocker          NodeLabel = "Blocker"
	LabelContributor      NodeLabel = "Contributor"
	LabelEvidence         NodeLabel = "Evidence"
	LabelOperatingMap     NodeLabel = "OperatingMap"
	LabelGovernanceChange NodeLabel = "GovernanceChange"
)

// nodeLabels is the closed set; also the exact set of AGE vertex labels the
// projector may create.
var nodeLabels = map[NodeLabel]bool{
	LabelOutcome: true, LabelGoal: true, LabelWorkCase: true, LabelObservation: true,
	LabelActionReceipt: true, LabelBlocker: true, LabelContributor: true, LabelEvidence: true,
	LabelOperatingMap: true, LabelGovernanceChange: true,
}

// Valid reports whether l is a known node label.
func (l NodeLabel) Valid() bool { return nodeLabels[l] }

// fromOutcomeNodeType maps a frozen sdk/go/outcome.NodeType onto the graph
// label vocabulary (a 1:1 translation for the lineage node types).
func fromOutcomeNodeType(t outcomemodel.NodeType) (NodeLabel, bool) {
	switch t {
	case outcomemodel.NodeOutcome:
		return LabelOutcome, true
	case outcomemodel.NodeGoal:
		return LabelGoal, true
	case outcomemodel.NodeWorkCase:
		return LabelWorkCase, true
	case outcomemodel.NodeObservation:
		return LabelObservation, true
	case outcomemodel.NodeActionReceipt:
		return LabelActionReceipt, true
	case outcomemodel.NodeBlocker:
		return LabelBlocker, true
	case outcomemodel.NodeContributor:
		return LabelContributor, true
	case outcomemodel.NodeEvidence:
		return LabelEvidence, true
	}
	return "", false
}

// EdgeLabel is the closed edge vocabulary (the sdk/go/outcome lineage edge
// types plus the two domain edges).
type EdgeLabel string

const (
	EdgeConcerns       EdgeLabel = "CONCERNS"
	EdgeSupersedes     EdgeLabel = "SUPERSEDES"
	EdgeCorrects       EdgeLabel = "CORRECTS"
	EdgeContributesTo  EdgeLabel = "CONTRIBUTES_TO"
	EdgeEvidences      EdgeLabel = "EVIDENCES"
	EdgeObserves       EdgeLabel = "OBSERVES"
	EdgeBlockedBy      EdgeLabel = "BLOCKED_BY"
	EdgeDerivedFrom    EdgeLabel = "DERIVED_FROM"
	EdgeGovernedBy     EdgeLabel = "GOVERNED_BY"
	EdgePublishedUnder EdgeLabel = "PUBLISHED_UNDER"
)

var edgeLabels = map[EdgeLabel]bool{
	EdgeConcerns: true, EdgeSupersedes: true, EdgeCorrects: true, EdgeContributesTo: true,
	EdgeEvidences: true, EdgeObserves: true, EdgeBlockedBy: true, EdgeDerivedFrom: true,
	EdgeGovernedBy: true, EdgePublishedUnder: true,
}

// Valid reports whether l is a known edge label.
func (l EdgeLabel) Valid() bool { return edgeLabels[l] }

func fromOutcomeEdgeType(t outcomemodel.EdgeType) (EdgeLabel, bool) {
	switch t {
	case outcomemodel.EdgeConcerns:
		return EdgeConcerns, true
	case outcomemodel.EdgeSupersedes:
		return EdgeSupersedes, true
	case outcomemodel.EdgeCorrects:
		return EdgeCorrects, true
	case outcomemodel.EdgeContributesTo:
		return EdgeContributesTo, true
	case outcomemodel.EdgeEvidences:
		return EdgeEvidences, true
	case outcomemodel.EdgeObserves:
		return EdgeObserves, true
	case outcomemodel.EdgeBlockedBy:
		return EdgeBlockedBy, true
	case outcomemodel.EdgeDerivedFrom:
		return EdgeDerivedFrom, true
	}
	return "", false
}

// --- event kinds -----------------------------------------------------------

// EventKind is the closed set of projection-outbox event kinds. There is no
// in-place-edit kind by design: a source revocation/supersession APPENDS a
// tombstone or reevaluation event, never edits an existing event.
type EventKind string

const (
	// KindUpsert MERGEs the delta's nodes and edges into the graph.
	KindUpsert EventKind = "upsert"
	// KindTombstone marks the delta's target node revoked (e.g. evidence
	// revocation) — the node stays for lineage but is flagged revoked.
	KindTombstone EventKind = "tombstone"
	// KindReevaluation marks the delta's target node superseded/reevaluated
	// (e.g. an Outcome correction).
	KindReevaluation EventKind = "reevaluation"
)

func (k EventKind) Valid() bool {
	switch k {
	case KindUpsert, KindTombstone, KindReevaluation:
		return true
	}
	return false
}

// fromOutcomeEventKind maps a frozen sdk/go/outcome.ProjectionEventKind onto the
// graph event kind. outcome_revision and lineage_fact both project as upserts;
// tombstone and reevaluation carry their correction semantics.
func fromOutcomeEventKind(k outcomemodel.ProjectionEventKind) EventKind {
	switch k {
	case outcomemodel.ProjectionTombstone:
		return KindTombstone
	case outcomemodel.ProjectionReevaluation:
		return KindReevaluation
	default:
		return KindUpsert
	}
}

// --- graph nodes and edges -------------------------------------------------

const maxSummary = 512

// GraphNode is one versioned Outcome-Graph node. It carries ONLY classified
// metadata: tenant/org scope, its label, a versioned business id, a permitted
// bounded summary and an opaque content hash. NEVER raw enterprise content,
// connector endpoints, credentials or full receipts — NoRawContentLeak enforces
// this on Validate.
type GraphNode struct {
	Tenant     string    `json:"tenant"`
	Org        string    `json:"org,omitempty"`
	Label      NodeLabel `json:"label"`
	BusinessID string    `json:"business_id"`
	Revision   uint64    `json:"revision"`
	Summary    string    `json:"summary,omitempty"`
	Hash       string    `json:"hash,omitempty"`
}

// NID is the deterministic, provider-neutral merge key derived ONLY from
// (tenant, label, business id, revision). Same inputs yield the same id, so a
// parameterized Cypher MERGE on NID is idempotent under duplicate/replayed
// events. A graph-provider's internal id never enters this computation.
func (n GraphNode) NID() string {
	return nid(n.Tenant, n.Label, n.BusinessID, n.Revision)
}

func nid(tenant string, label NodeLabel, businessID string, revision uint64) string {
	h := sha256.New()
	for _, p := range []string{tenant, string(label), businessID, fmt.Sprintf("%d", revision)} {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "gn_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// Validate enforces the GraphNode rules and the opaque-content boundary.
func (n GraphNode) Validate() error {
	if n.Tenant == "" {
		return errors.New("outcomegraph: node tenant is required")
	}
	if !n.Label.Valid() {
		return fmt.Errorf("outcomegraph: node label %q is not valid", n.Label)
	}
	if n.BusinessID == "" {
		return errors.New("outcomegraph: node business_id is required")
	}
	if n.Revision < 1 {
		return errors.New("outcomegraph: node revision must be >= 1 (nodes are versioned)")
	}
	if utf8.RuneCountInString(n.Summary) > maxSummary {
		return fmt.Errorf("outcomegraph: node summary must be a bounded permitted summary (<=%d chars)", maxSummary)
	}
	// The opaque-content guard: reuse the exact sdk/go/outcome scanner so a
	// connector endpoint, credential or raw query can never enter the graph.
	return outcomemodel.NoRawContentLeak(n)
}

// NodeRef is a versioned reference to a node (its endpoint), without the
// node's own metadata. Used for edges and tombstone/reevaluation targets.
type NodeRef struct {
	Label      NodeLabel `json:"label"`
	BusinessID string    `json:"business_id"`
	Revision   uint64    `json:"revision"`
}

// NID returns the deterministic node id this ref points at, in tenant.
func (r NodeRef) NID(tenant string) string { return nid(tenant, r.Label, r.BusinessID, r.Revision) }

func (r NodeRef) Validate() error {
	if !r.Label.Valid() {
		return fmt.Errorf("outcomegraph: ref label %q is not valid", r.Label)
	}
	if r.BusinessID == "" || r.Revision < 1 {
		return errors.New("outcomegraph: node ref must be versioned (business_id and revision >= 1)")
	}
	return nil
}

// GraphEdge is one versioned edge between two versioned endpoints in the SAME
// tenant. A cross-tenant edge is rejected.
type GraphEdge struct {
	Tenant string    `json:"tenant"`
	Label  EdgeLabel `json:"label"`
	From   NodeRef   `json:"from"`
	To     NodeRef   `json:"to"`
}

// EID is the deterministic edge merge key derived from (tenant, edge label,
// from node id, to node id).
func (e GraphEdge) EID() string {
	h := sha256.New()
	for _, p := range []string{e.Tenant, string(e.Label), e.From.NID(e.Tenant), e.To.NID(e.Tenant)} {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "ge_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// ErrCrossTenantEdge rejects an edge whose endpoints are not both in its tenant.
var ErrCrossTenantEdge = errors.New("outcomegraph: an edge must not cross tenants")

func (e GraphEdge) Validate() error {
	if e.Tenant == "" {
		return errors.New("outcomegraph: edge tenant is required")
	}
	if !e.Label.Valid() {
		return fmt.Errorf("outcomegraph: edge label %q is not valid", e.Label)
	}
	if err := e.From.Validate(); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := e.To.Validate(); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	return nil
}

// --- canonical delta -------------------------------------------------------

// OutcomeSubjectRef points at an authoritative Outcome revision. It is retained
// as documentation of an event's subject; the delta itself always carries the
// inline nodes/edges to project, so the projector never re-reads authoritative
// tables at projection time (self-contained, decoupled, rebuildable).
type OutcomeSubjectRef struct {
	OutcomeKey string `json:"outcome_key"`
	Revision   uint64 `json:"revision"`
}

// GraphDelta is the canonical, self-contained payload of a projection event:
// the exact set of nodes and edges to MERGE (KindUpsert) or the target node to
// mark revoked/superseded (KindTombstone/KindReevaluation). It carries only
// opaque, classified data (each node passes NoRawContentLeak). Because it is
// self-contained, a full rebuild replays deltas to an IDENTICAL graph.
type GraphDelta struct {
	Nodes []GraphNode `json:"nodes,omitempty"`
	Edges []GraphEdge `json:"edges,omitempty"`
	// Mark is the target of a tombstone/reevaluation (the node to flag).
	Mark *NodeRef `json:"mark,omitempty"`
}

// Validate enforces the delta rules for its kind.
func (d GraphDelta) Validate(kind EventKind) error {
	for i, n := range d.Nodes {
		if err := n.Validate(); err != nil {
			return fmt.Errorf("nodes[%d]: %w", i, err)
		}
	}
	for i, e := range d.Edges {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("edges[%d]: %w", i, err)
		}
	}
	switch kind {
	case KindUpsert:
		if len(d.Nodes) == 0 && len(d.Edges) == 0 {
			return errors.New("outcomegraph: an upsert delta must carry at least one node or edge")
		}
	case KindTombstone, KindReevaluation:
		if d.Mark == nil {
			return errors.New("outcomegraph: a tombstone/reevaluation delta must name a mark target")
		}
		if err := d.Mark.Validate(); err != nil {
			return fmt.Errorf("mark: %w", err)
		}
	default:
		return fmt.Errorf("outcomegraph: unknown event kind %q", kind)
	}
	return nil
}

// canonicalJSON marshals the delta deterministically (sorted nodes/edges) so
// the payload hash is stable regardless of input ordering.
func (d GraphDelta) canonicalJSON() []byte {
	c := GraphDelta{Mark: d.Mark}
	c.Nodes = append([]GraphNode(nil), d.Nodes...)
	c.Edges = append([]GraphEdge(nil), d.Edges...)
	sort.Slice(c.Nodes, func(i, j int) bool { return c.Nodes[i].NID() < c.Nodes[j].NID() })
	sort.Slice(c.Edges, func(i, j int) bool { return c.Edges[i].EID() < c.Edges[j].EID() })
	raw, _ := json.Marshal(c)
	return raw
}

// Hash returns the deterministic content hash of the delta.
func (d GraphDelta) Hash() string {
	sum := sha256.Sum256(d.canonicalJSON())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// --- projection event ------------------------------------------------------

// ProjectionEvent is one immutable, canonical row of the ONE shared projection
// outbox (db/migrations/000017_outcome_graph_outbox.sql). All four authoritative
// stores append it transactionally with their own domain write. Sequence is
// per-tenant monotonic and assigned by the store; the projector consumes events
// in sequence order and advances a per-tenant watermark only after the AGE write
// commits.
type ProjectionEvent struct {
	Tenant             string     `json:"tenant"`
	Sequence           uint64     `json:"sequence"`
	Org                string     `json:"org,omitempty"`
	Source             Source     `json:"source"`
	Kind               EventKind  `json:"kind"`
	SubjectLabel       NodeLabel  `json:"subject_label"`
	SubjectID          string     `json:"subject_id"`
	SubjectRevision    uint64     `json:"subject_revision"`
	Delta              GraphDelta `json:"delta"`
	PayloadHash        string     `json:"payload_hash"`
	SupersedesSequence uint64     `json:"supersedes_sequence,omitempty"`
	RecordedAt         time.Time  `json:"recorded_at"`
}

// Validate enforces the ProjectionEvent rules. Sequence is not required here
// (the store assigns it) but when present must be >= 1.
func (e ProjectionEvent) Validate() error {
	if e.Tenant == "" {
		return errors.New("outcomegraph: event tenant is required")
	}
	if !e.Source.Valid() {
		return fmt.Errorf("outcomegraph: event source %q is not valid", e.Source)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("outcomegraph: event kind %q is not valid", e.Kind)
	}
	if !e.SubjectLabel.Valid() {
		return fmt.Errorf("outcomegraph: event subject_label %q is not valid", e.SubjectLabel)
	}
	if e.SubjectID == "" {
		return errors.New("outcomegraph: event subject_id is required")
	}
	if e.SubjectRevision < 1 {
		return errors.New("outcomegraph: event subject_revision must be >= 1")
	}
	if e.RecordedAt.IsZero() {
		return errors.New("outcomegraph: event recorded_at is required")
	}
	if err := e.Delta.Validate(e.Kind); err != nil {
		return err
	}
	// Every node/edge in the delta must bind the event's own tenant: a delta
	// can never smuggle a cross-tenant node/edge into another tenant's graph.
	for i, n := range e.Delta.Nodes {
		if n.Tenant != e.Tenant {
			return fmt.Errorf("nodes[%d]: %w (node tenant %q != event tenant %q)", i, ErrCrossTenantEdge, n.Tenant, e.Tenant)
		}
	}
	for i, ed := range e.Delta.Edges {
		if ed.Tenant != e.Tenant {
			return fmt.Errorf("edges[%d]: %w", i, ErrCrossTenantEdge)
		}
	}
	if e.PayloadHash != "" && e.PayloadHash != e.Delta.Hash() {
		return errors.New("outcomegraph: event payload_hash does not match the delta")
	}
	return nil
}

// WithHash returns a copy of e with PayloadHash set from its delta.
func (e ProjectionEvent) WithHash() ProjectionEvent {
	e.PayloadHash = e.Delta.Hash()
	return e
}
