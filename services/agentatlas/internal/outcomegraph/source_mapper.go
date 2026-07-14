package outcomegraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	sdkoperatingmap "github.com/astraclawteam/agentatlas/sdk/go/operatingmap"
	outcomemodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
)

// source_mapper maps a COMMITTED versioned domain fact from each authoritative
// store into ONE canonical ProjectionEvent with a self-contained GraphDelta, and
// appends it to the single shared outbox inside the store's own transaction.
// Nothing here reads back the graph or calls AGE; the mapping is pure and
// deterministic so a full outbox replay rebuilds an identical graph.
//
// Only opaque, classified data enters a delta: versioned business ids, bounded
// permitted summaries and opaque hashes. Every node is checked by
// GraphNode.Validate (which runs the sdk/go/outcome NoRawContentLeak scanner), so
// a connector endpoint, credential or raw query can never be projected.

// boundedSummary clamps s to the permitted-summary length.
func boundedSummary(s string) string {
	if utf8.RuneCountInString(s) <= maxSummary {
		return s
	}
	r := []rune(s)
	return string(r[:maxSummary])
}

func atLeast1(v uint64) uint64 {
	if v < 1 {
		return 1
	}
	return v
}

// --- workcase --------------------------------------------------------------

// MapWorkCase builds the projection event for a committed WorkCase revision: a
// single versioned WorkCase node carrying its org scope and a status summary.
func MapWorkCase(wc sdkworkcase.WorkCase) ProjectionEvent {
	rev := atLeast1(wc.Revision)
	node := GraphNode{
		Tenant: wc.EnterpriseID, Org: wc.OrgScope, Label: LabelWorkCase,
		BusinessID: wc.ID, Revision: rev, Summary: boundedSummary(string(wc.Status)),
	}
	delta := GraphDelta{Nodes: []GraphNode{node}}
	return ProjectionEvent{
		Tenant: wc.EnterpriseID, Org: wc.OrgScope, Source: SourceWorkCase, Kind: KindUpsert,
		SubjectLabel: LabelWorkCase, SubjectID: wc.ID, SubjectRevision: rev,
		Delta: delta, PayloadHash: delta.Hash(),
	}
}

// --- operating map ---------------------------------------------------------

// MapOperatingMap builds the projection event for a published Operating Map
// entry: a single versioned OperatingMap node carrying its org scope. Its method
// refs carry no stable identity, so they are summarized (count) rather than
// projected as nodes.
func MapOperatingMap(e sdkoperatingmap.Entry) ProjectionEvent {
	rev := atLeast1(uint64(e.Version))
	node := GraphNode{
		Tenant: e.EnterpriseID, Org: e.OrgScope, Label: LabelOperatingMap,
		BusinessID: e.IntentKey, Revision: rev,
		Summary: boundedSummary(fmt.Sprintf("methods=%d", len(e.MethodRefs))),
	}
	delta := GraphDelta{Nodes: []GraphNode{node}}
	return ProjectionEvent{
		Tenant: e.EnterpriseID, Org: e.OrgScope, Source: SourceOperatingMap, Kind: KindUpsert,
		SubjectLabel: LabelOperatingMap, SubjectID: e.IntentKey, SubjectRevision: rev,
		Delta: delta, PayloadHash: delta.Hash(),
	}
}

// --- governance ------------------------------------------------------------

// MapGovernanceChange builds the projection event for a published governed
// change. It takes primitives (never an internal governance type) so
// internal/governance can depend on this package without a cycle. resourceType +
// resourceID form the versioned business id; org is the org unit.
func MapGovernanceChange(tenant, org, resourceType, resourceID string, version uint64, auditHash string) ProjectionEvent {
	rev := atLeast1(version)
	bizID := resourceType + ":" + resourceID
	node := GraphNode{
		Tenant: tenant, Org: org, Label: LabelGovernanceChange,
		BusinessID: bizID, Revision: rev, Summary: boundedSummary(resourceType), Hash: auditHash,
	}
	delta := GraphDelta{Nodes: []GraphNode{node}}
	return ProjectionEvent{
		Tenant: tenant, Org: org, Source: SourceGovernance, Kind: KindUpsert,
		SubjectLabel: LabelGovernanceChange, SubjectID: bizID, SubjectRevision: rev,
		Delta: delta, PayloadHash: delta.Hash(),
	}
}

// --- outcome ---------------------------------------------------------------

// MapOutcome builds the rich, self-contained graph delta for a committed Outcome
// revision: the Outcome node plus the Goal, WorkCase, Observation, Blocker,
// Contributor, Evidence and ActionReceipt nodes it binds, and the lineage edges
// between them. This is derived ONLY from the authoritative Outcome record, and
// carries only opaque handles/hashes and permitted summaries. org is "" — the
// outcome domain is tenant-scoped (an Outcome carries no org scope); org-scoped
// isolation is enforced on the org-bearing anchors (WorkCase, OperatingMap,
// GovernanceChange).
func MapOutcome(o outcomemodel.Outcome) GraphDelta {
	rev := atLeast1(o.Revision)
	outcomeNode := GraphNode{Tenant: o.Tenant, Label: LabelOutcome, BusinessID: o.OutcomeKey, Revision: rev, Summary: boundedSummary(string(o.Claim.Status))}
	nodes := []GraphNode{outcomeNode}
	var edges []GraphEdge
	outRef := NodeRef{Label: LabelOutcome, BusinessID: o.OutcomeKey, Revision: rev}

	// Goal.
	if o.Claim.Goal.GoalKey != "" {
		gRev := atLeast1(uint64(o.Claim.Goal.GoalVersion))
		nodes = append(nodes, GraphNode{Tenant: o.Tenant, Label: LabelGoal, BusinessID: o.Claim.Goal.GoalKey, Revision: gRev})
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeConcerns, From: outRef, To: NodeRef{Label: LabelGoal, BusinessID: o.Claim.Goal.GoalKey, Revision: gRev}})
	}
	// WorkCase — referenced by an edge only. The WorkCase NODE is OWNED by the
	// workcase domain (which carries its org scope); the outcome domain never
	// projects a WorkCase node, so it can never clobber that org with its own
	// tenant-scoped (org == "") view.
	if o.WorkCaseID != "" {
		wRev := atLeast1(o.WorkCaseRevision)
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeDerivedFrom, From: outRef, To: NodeRef{Label: LabelWorkCase, BusinessID: o.WorkCaseID, Revision: wRev}})
	}
	// Observations.
	for _, obs := range o.Claim.Observations {
		nodes = append(nodes, GraphNode{Tenant: o.Tenant, Label: LabelObservation, BusinessID: obs.Handle, Revision: 1, Summary: boundedSummary(obs.Authority), Hash: obs.ObservationHash})
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeObserves, From: outRef, To: NodeRef{Label: LabelObservation, BusinessID: obs.Handle, Revision: 1}})
	}
	// Evidence.
	for _, ev := range o.Claim.Evidence {
		nodes = append(nodes, GraphNode{Tenant: o.Tenant, Label: LabelEvidence, BusinessID: ev.Handle, Revision: 1, Summary: boundedSummary(ev.Authority), Hash: ev.ContentHash})
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeEvidences, From: outRef, To: NodeRef{Label: LabelEvidence, BusinessID: ev.Handle, Revision: 1}})
	}
	// Blockers.
	for _, b := range o.Claim.Blockers {
		nodes = append(nodes, GraphNode{Tenant: o.Tenant, Label: LabelBlocker, BusinessID: b.Handle, Revision: 1, Summary: boundedSummary(b.Kind)})
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeBlockedBy, From: outRef, To: NodeRef{Label: LabelBlocker, BusinessID: b.Handle, Revision: 1}})
	}
	// Contributors (methods/agents).
	for _, c := range o.Contributions {
		nodes = append(nodes, GraphNode{Tenant: o.Tenant, Label: LabelContributor, BusinessID: c.ContributorID, Revision: 1, Summary: boundedSummary(c.Kind)})
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeContributesTo, From: outRef, To: NodeRef{Label: LabelContributor, BusinessID: c.ContributorID, Revision: 1}})
	}
	// Action receipts (audit lineage only, never satisfaction evidence).
	for _, r := range o.ActionReceipts {
		nodes = append(nodes, GraphNode{Tenant: o.Tenant, Label: LabelActionReceipt, BusinessID: r.Handle, Revision: 1, Hash: r.ReceiptHash})
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeDerivedFrom, From: outRef, To: NodeRef{Label: LabelActionReceipt, BusinessID: r.Handle, Revision: 1}})
	}
	// Supersession lineage.
	if o.Supersedes != nil {
		pRev := atLeast1(o.Supersedes.Revision)
		nodes = append(nodes, GraphNode{Tenant: o.Tenant, Label: LabelOutcome, BusinessID: o.Supersedes.OutcomeKey, Revision: pRev})
		edges = append(edges, GraphEdge{Tenant: o.Tenant, Label: EdgeSupersedes, From: outRef, To: NodeRef{Label: LabelOutcome, BusinessID: o.Supersedes.OutcomeKey, Revision: pRev}})
	}
	return GraphDelta{Nodes: nodes, Edges: edges}
}

// minimalSubjectDelta upserts only the subject node (label derived from the
// source event's subject type) — used when the authoritative Outcome for a
// projection event is not (yet) present (e.g. a raw projection-event append with
// no outcome row).
func minimalSubjectDelta(tenant string, label NodeLabel, subjectID string, subjectRevision uint64, hash string) GraphDelta {
	return GraphDelta{Nodes: []GraphNode{{Tenant: tenant, Label: label, BusinessID: subjectID, Revision: atLeast1(subjectRevision), Hash: hash}}}
}

// MapOutcomeMarker builds the projection event for a frozen sdk/go/outcome
// ProjectionEvent, re-pointed onto the shared outbox. A rich delta is supplied
// when the authoritative Outcome is available (outcome_revision/lineage_fact);
// tombstone/reevaluation carry a correction mark on the subject Outcome node so
// evidence revocation and Outcome supersession propagate into the graph.
func MapOutcomeMarker(ev outcomemodel.ProjectionEvent, rich *GraphDelta) ProjectionEvent {
	kind := fromOutcomeEventKind(ev.Kind)
	label, ok := fromOutcomeNodeType(ev.SubjectType)
	if !ok {
		label = LabelOutcome
	}
	var delta GraphDelta
	switch kind {
	case KindUpsert:
		if rich != nil {
			delta = *rich
		} else {
			delta = minimalSubjectDelta(ev.Tenant, label, ev.SubjectID, ev.SubjectRevision, ev.PayloadHash)
		}
	case KindTombstone, KindReevaluation:
		delta = GraphDelta{Mark: &NodeRef{Label: label, BusinessID: ev.SubjectID, Revision: atLeast1(ev.SubjectRevision)}}
	}
	// NOTE: the 0G ev.SupersedesSequence references the retired
	// outcome_projection_events sequence space and is NOT meaningful in the shared
	// outbox (which has its own per-tenant sequence), so it is deliberately dropped
	// here. Correction ordering in the graph is carried by the tombstone/
	// reevaluation MARK on the subject node, not by a cross-outbox sequence pointer.
	out := ProjectionEvent{
		Tenant: ev.Tenant, Org: "", Source: SourceOutcome, Kind: kind,
		SubjectLabel: label, SubjectID: ev.SubjectID, SubjectRevision: atLeast1(ev.SubjectRevision),
		Delta: delta, RecordedAt: ev.RecordedAt,
	}
	out.PayloadHash = delta.Hash()
	return out
}

// --- transactional outbox append -------------------------------------------

// AppendOutboxTx appends ev to the single shared projection outbox
// (outcome_graph_outbox) WITHIN the caller's transaction. It assigns the next
// per-tenant sequence under a transaction-scoped advisory lock, stamps
// RecordedAt/PayloadHash if unset, and returns the assigned sequence. The caller
// commits its domain write and this outbox row together — the only synchronous
// write is to authoritative PostgreSQL; AGE is updated asynchronously by the
// projector. There is NO PostgreSQL/AGE dual write.
func AppendOutboxTx(ctx context.Context, tx pgx.Tx, ev ProjectionEvent, now time.Time) (uint64, error) {
	if ev.RecordedAt.IsZero() {
		ev.RecordedAt = now.UTC()
	}
	if ev.PayloadHash == "" {
		ev.PayloadHash = ev.Delta.Hash()
	}
	if err := ev.Validate(); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(ev.Delta)
	if err != nil {
		return 0, err
	}
	// Serialize per-tenant sequence assignment; the PRIMARY KEY (tenant,sequence)
	// is the real backstop.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "outcome_graph_outbox:"+ev.Tenant); err != nil {
		return 0, err
	}
	var next int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM outcome_graph_outbox WHERE tenant=$1`, ev.Tenant).Scan(&next); err != nil {
		return 0, err
	}
	var supersedes any
	if ev.SupersedesSequence != 0 {
		supersedes = int64(ev.SupersedesSequence)
	}
	var org any
	if ev.Org != "" {
		org = ev.Org
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outcome_graph_outbox
		(tenant,sequence,org,source,kind,subject_label,subject_id,subject_revision,payload,payload_hash,supersedes_sequence,recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		ev.Tenant, next, org, string(ev.Source), string(ev.Kind), string(ev.SubjectLabel),
		ev.SubjectID, int64(ev.SubjectRevision), payload, ev.PayloadHash, supersedes, ev.RecordedAt.UTC()); err != nil {
		return 0, err
	}
	return uint64(next), nil
}

// --- digest ----------------------------------------------------------------

// graphDigest computes a deterministic content digest of a tenant's in-memory
// graph (sorted nodes with their correction flags, then sorted edges). Two
// graphs with the same digest are identical — the rebuild-equality proof.
func graphDigest(nodes map[string]memNode, edges map[string]GraphEdge) string {
	type nd struct {
		NID        string
		Label      NodeLabel
		Org        string
		BusinessID string
		Revision   uint64
		Summary    string
		Hash       string
		Revoked    bool
		Superseded bool
	}
	nl := make([]nd, 0, len(nodes))
	for id, n := range nodes {
		nl = append(nl, nd{id, n.node.Label, n.node.Org, n.node.BusinessID, n.node.Revision, n.node.Summary, n.node.Hash, n.revoked, n.superseded})
	}
	sort.Slice(nl, func(i, j int) bool { return nl[i].NID < nl[j].NID })
	el := make([]string, 0, len(edges))
	for id, e := range edges {
		el = append(el, id+"|"+string(e.Label))
	}
	sort.Strings(el)
	h := sha256.New()
	blob, _ := json.Marshal(struct {
		Nodes []nd
		Edges []string
	}{nl, el})
	h.Write(blob)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
