// Package outcome mirrors schemas/outcome/outcome.schema.json and
// schemas/outcome/lineage.schema.json. It freezes the provable result-lineage
// vocabulary every Outcome-verification, projection and learning task
// (0G-0J) builds against and no other: a versioned, immutable Outcome that
// binds exactly which governed-record versions and which signed, opaque
// evidence/observation/receipt references justify a business conclusion, plus
// the append-only lineage facts (nodes, edges) and authoritative projection
// events that a later Apache AGE read model (Task 0I) rebuilds from.
//
// Three boundaries are load-bearing and enforced here:
//
//   - OutcomeStatus is a CLOSED enum of business conclusions
//     {unverified, satisfied, unsatisfied, disputed, blocked, unknown}. Action
//     success (a nexus.ActionStatus such as "succeeded") is NOT an Outcome
//     status, and a technical ActionReceipt can never be coerced into a
//     satisfied Outcome — only an authoritative, signed ObservationRef can.
//   - Every reference is OPAQUE: a handle + hash (+ signing key id / authority)
//     and at most a bounded, permitted summary — never a connector endpoint,
//     credential, raw enterprise content or a full receipt body. NoRawContentLeak
//     enforces that boundary on every Outcome.
//   - Identity is DETERMINISTIC and provider-neutral: a stable id derives only
//     from (tenant, node type, business id, revision). A graph-provider's
//     internal id (AGE/Cypher) never appears in this contract.
//
// This package is self-contained (stdlib only) so any consumer can import it
// without pulling in service internals. Authoritative persistence lives in
// services/agentatlas/internal/outcome; authoritative validation happens
// server-side against the JSON Schemas.
package outcome

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"
)

// --- OutcomeStatus (closed business-conclusion enum) -----------------------

// OutcomeStatus is the business conclusion of an Outcome. It is a CLOSED enum:
// an unknown/invalid value is rejected at both this SDK layer and the JSON
// Schema layer. Action success is deliberately absent — a technical
// ActionReceipt proving execution is never an Outcome status.
type OutcomeStatus string

const (
	OutcomeUnverified  OutcomeStatus = "unverified"
	OutcomeSatisfied   OutcomeStatus = "satisfied"
	OutcomeUnsatisfied OutcomeStatus = "unsatisfied"
	OutcomeDisputed    OutcomeStatus = "disputed"
	OutcomeBlocked     OutcomeStatus = "blocked"
	OutcomeUnknown     OutcomeStatus = "unknown"
)

// OutcomeStatuses returns the closed set in a stable order.
func OutcomeStatuses() []OutcomeStatus {
	return []OutcomeStatus{
		OutcomeUnverified, OutcomeSatisfied, OutcomeUnsatisfied, OutcomeDisputed, OutcomeBlocked, OutcomeUnknown,
	}
}

// Valid reports whether s is one of the closed statuses.
func (s OutcomeStatus) Valid() bool {
	switch s {
	case OutcomeUnverified, OutcomeSatisfied, OutcomeUnsatisfied, OutcomeDisputed, OutcomeBlocked, OutcomeUnknown:
		return true
	}
	return false
}

// IsTerminal reports whether s is a definitive verified conclusion (satisfied
// or unsatisfied). unverified/disputed/blocked/unknown retain an explicit
// forward path (reconciliation, human decision, replanning) and are not
// terminal. A terminal Outcome revision is never mutated in place; changing a
// terminal conclusion requires an explicit correction (a NEW revision).
func (s OutcomeStatus) IsTerminal() bool {
	return s == OutcomeSatisfied || s == OutcomeUnsatisfied
}

// --- closed lineage node/edge/projection enums -----------------------------

// NodeType is the closed, versioned set of lineage node kinds.
type NodeType string

const (
	NodeOutcome       NodeType = "outcome"
	NodeGoal          NodeType = "goal"
	NodeWorkCase      NodeType = "work_case"
	NodeObservation   NodeType = "observation"
	NodeActionReceipt NodeType = "action_receipt"
	NodeBlocker       NodeType = "blocker"
	NodeContributor   NodeType = "contributor"
	NodeEvidence      NodeType = "evidence"
)

// NodeTypes returns the closed set in a stable order.
func NodeTypes() []NodeType {
	return []NodeType{
		NodeOutcome, NodeGoal, NodeWorkCase, NodeObservation, NodeActionReceipt, NodeBlocker, NodeContributor, NodeEvidence,
	}
}

// Valid reports whether t is a known node type.
func (t NodeType) Valid() bool {
	switch t {
	case NodeOutcome, NodeGoal, NodeWorkCase, NodeObservation, NodeActionReceipt, NodeBlocker, NodeContributor, NodeEvidence:
		return true
	}
	return false
}

// EdgeType is the closed, versioned set of lineage edge kinds.
type EdgeType string

const (
	EdgeConcerns      EdgeType = "concerns"
	EdgeSupersedes    EdgeType = "supersedes"
	EdgeCorrects      EdgeType = "corrects"
	EdgeContributesTo EdgeType = "contributes_to"
	EdgeEvidences     EdgeType = "evidences"
	EdgeObserves      EdgeType = "observes"
	EdgeBlockedBy     EdgeType = "blocked_by"
	EdgeDerivedFrom   EdgeType = "derived_from"
)

// EdgeTypes returns the closed set in a stable order.
func EdgeTypes() []EdgeType {
	return []EdgeType{
		EdgeConcerns, EdgeSupersedes, EdgeCorrects, EdgeContributesTo, EdgeEvidences, EdgeObserves, EdgeBlockedBy, EdgeDerivedFrom,
	}
}

// Valid reports whether t is a known edge type.
func (t EdgeType) Valid() bool {
	switch t {
	case EdgeConcerns, EdgeSupersedes, EdgeCorrects, EdgeContributesTo, EdgeEvidences, EdgeObserves, EdgeBlockedBy, EdgeDerivedFrom:
		return true
	}
	return false
}

// ProjectionEventKind is the closed set of authoritative projection-outbox
// event kinds. There is no in-place-edit kind by design: a source
// deletion/revocation APPENDS a tombstone (+ a bounded reevaluation), never
// edits an existing event.
type ProjectionEventKind string

const (
	ProjectionOutcomeRevision ProjectionEventKind = "outcome_revision"
	ProjectionLineageFact     ProjectionEventKind = "lineage_fact"
	ProjectionTombstone       ProjectionEventKind = "tombstone"
	ProjectionReevaluation    ProjectionEventKind = "reevaluation"
)

// ProjectionEventKinds returns the closed set in a stable order.
func ProjectionEventKinds() []ProjectionEventKind {
	return []ProjectionEventKind{
		ProjectionOutcomeRevision, ProjectionLineageFact, ProjectionTombstone, ProjectionReevaluation,
	}
}

// Valid reports whether k is a known projection event kind.
func (k ProjectionEventKind) Valid() bool {
	switch k {
	case ProjectionOutcomeRevision, ProjectionLineageFact, ProjectionTombstone, ProjectionReevaluation:
		return true
	}
	return false
}

// --- sentinel errors -------------------------------------------------------

var (
	// ErrUnknownOutcomeStatus rejects a status outside the closed enum —
	// including a nexus.ActionStatus value such as "succeeded".
	ErrUnknownOutcomeStatus = errors.New("outcome: status is not a valid OutcomeStatus (action success is not an outcome)")
	// ErrActionReceiptNotOutcomeEvidence rejects a satisfied Outcome whose
	// only justification is an ActionReceipt (technical execution proof). An
	// ActionReceipt can never be coerced into a satisfied Outcome; only an
	// authoritative signed ObservationRef can justify satisfaction.
	ErrActionReceiptNotOutcomeEvidence = errors.New("outcome: an ActionReceipt proves technical execution only; it can never justify a satisfied Outcome")
	// ErrSatisfiedNeedsObservation rejects a satisfied Outcome with no bound
	// authoritative ObservationRef.
	ErrSatisfiedNeedsObservation = errors.New("outcome: a satisfied Outcome requires at least one authoritative, signed ObservationRef")
	// ErrMissingGoalVersion rejects an Outcome whose GoalRef carries no
	// version.
	ErrMissingGoalVersion = errors.New("outcome: goal version is required (an Outcome binds an exact Goal version)")
	// ErrMissingRuleVersion rejects an Outcome that names no rule version.
	ErrMissingRuleVersion = errors.New("outcome: rule version is required (an Outcome binds the exact evaluation-rule version)")
	// ErrDuplicateContribution rejects crediting the same contributor for the
	// same contribution kind twice on one Outcome.
	ErrDuplicateContribution = errors.New("outcome: duplicate contribution (same contributor credited twice for the same kind)")
	// ErrRawContentLeak rejects any reference/summary that matches a
	// connector, credential or raw-query shape — see NoRawContentLeak.
	ErrRawContentLeak = errors.New("outcome: content matches a forbidden connector, credential or raw-query shape; an Outcome carries opaque handles, hashes and permitted summaries only")
	// ErrUnversionedEndpoint rejects a lineage node/endpoint without a
	// business id or a revision >= 1.
	ErrUnversionedEndpoint = errors.New("outcome: a lineage endpoint must carry a business id and a revision >= 1 (endpoints are always versioned)")
	// ErrCrossTenantEdge rejects a lineage edge whose endpoints are not both
	// in the edge's own tenant.
	ErrCrossTenantEdge = errors.New("outcome: a lineage edge must not cross tenants (both endpoints bind the edge's own tenant)")
	// ErrMissingSupersession rejects a correction revision (>1) that does not
	// name the revision it supersedes.
	ErrMissingSupersession = errors.New("outcome: an Outcome revision greater than 1 must name the prior revision it supersedes")
	// ErrUnexpectedSupersession rejects a revision-1 Outcome that carries a
	// supersession pointer (nothing precedes it).
	ErrUnexpectedSupersession = errors.New("outcome: a revision-1 Outcome must not carry a supersession pointer")
)

// --- opaque-content guard (mirrors operatingmap.NoConnectorLeak) -----------

// connectorShapePattern is the same forbidden-shape scanner
// sdk/go/operatingmap uses, re-declared here so this package stays
// self-contained: URL schemes, jdbc: URLs, SQL SELECT...FROM, api/ path
// segments, connection-string credential keys, bearer authorization values
// and backslash share paths. An Outcome carries opaque handles/hashes and
// bounded permitted summaries only, so none of these may appear anywhere in
// its marshaled form.
var connectorShapePattern = regexp.MustCompile(`(?i)` +
	`[a-z][a-z0-9+.-]*://` +
	`|jdbc:` +
	`|(?:\b|\\[nrt])select\b[\s\S]{0,300}?(?:\b|\\[nrt])from\b` +
	`|select\*from` +
	`|(?:^|[^a-z0-9_])api/` +
	`|password\s*=` +
	`|\b(?:user id|uid|pwd)\s*=` +
	`|\b(?:data source|initial catalog|integrated security)\s*=` +
	`|\\\\[^\\]+\\\\` +
	`|authorization:\s*bearer`)

// NoRawContentLeak marshals v to JSON and reports ErrRawContentLeak if the
// result contains anything shaped like a connector endpoint, credential or
// raw query. Every Outcome (via Validate) must pass this check.
func NoRawContentLeak(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	loc := connectorShapePattern.FindIndex(raw)
	if loc == nil {
		return nil
	}
	end := loc[1] + 24
	if end > len(raw) {
		end = len(raw)
	}
	return fmt.Errorf("%w: near %q", ErrRawContentLeak, raw[loc[0]:end])
}

// --- deterministic identity ------------------------------------------------

// StableID derives a deterministic, provider-neutral id from (tenant, node
// type, business id, revision) — nothing else. Same inputs yield the same id;
// a different tenant, type, business id or revision yields a different id. A
// graph provider's internal id (AGE/Cypher) never enters this computation or
// the public contract.
func StableID(tenant string, t NodeType, businessID string, revision uint64) string {
	h := sha256.New()
	for _, p := range []string{tenant, string(t), businessID, strconv.FormatUint(revision, 10)} {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "ol_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// --- opaque reference types ------------------------------------------------

// GoalRef is a versioned reference to a Goal in a tenant. It binds an exact
// GoalVersion: an Outcome without a goal version is not admissible.
type GoalRef struct {
	Tenant      string `json:"tenant"`
	GoalKey     string `json:"goal_key"`
	GoalVersion int64  `json:"goal_version"`
}

// Validate enforces the GoalRef rules.
func (g GoalRef) Validate() error {
	if g.Tenant == "" || utf8.RuneCountInString(g.Tenant) > 128 {
		return fmt.Errorf("goal tenant must contain 1..128 characters")
	}
	if g.GoalKey == "" || utf8.RuneCountInString(g.GoalKey) > 256 {
		return fmt.Errorf("goal_key must contain 1..256 characters")
	}
	if g.GoalVersion < 1 {
		return ErrMissingGoalVersion
	}
	return nil
}

// EvidenceRef is an opaque, content-addressed pointer to evidence grounding an
// Outcome — a handle and hash only, never raw content. Mirrors
// sdk/go/workcase.EvidenceRef.
type EvidenceRef struct {
	Handle      string `json:"handle"`
	ContentHash string `json:"content_hash"`
	Authority   string `json:"authority"`
}

// Validate enforces the EvidenceRef rules.
func (e EvidenceRef) Validate() error {
	if e.Handle == "" || e.ContentHash == "" || e.Authority == "" {
		return fmt.Errorf("evidence_ref requires handle, content_hash and authority")
	}
	return nil
}

// ObservationRef is an opaque, content-addressed pointer to a signed
// nexus.ObservationReceipt: a handle + normalized-observation hash + the
// authoritative source + the signing key id + the observed-at time. It is a
// HANDLE, never the receipt body. 0G stores refs only; it never re-verifies
// signatures (Task 0H evaluates). IsAuthoritative reports whether this ref is
// admissible as satisfaction evidence.
type ObservationRef struct {
	Handle          string    `json:"handle"`
	ObservationHash string    `json:"observation_hash"`
	Authority       string    `json:"authority"`
	SignatureKeyID  string    `json:"signature_key_id"`
	ObservedAt      time.Time `json:"observed_at"`
}

// IsAuthoritative reports whether the observation is bound to a named
// authoritative source AND a signing key — the structural precondition for
// justifying a satisfied Outcome. It does NOT verify the signature (0G stores
// refs; Task 0H verifies).
func (o ObservationRef) IsAuthoritative() bool {
	return o.Authority != "" && o.SignatureKeyID != ""
}

// Validate enforces the ObservationRef rules. Authority and signature_key_id
// are both required (matching the schema's required set): an ObservationRef
// always names its authoritative source and the signing key of the receipt it
// references. Whether that combination is admissible as satisfaction evidence
// is IsAuthoritative.
func (o ObservationRef) Validate() error {
	if o.Handle == "" || o.ObservationHash == "" {
		return fmt.Errorf("observation_ref requires handle and observation_hash")
	}
	if o.Authority == "" {
		return fmt.Errorf("observation_ref requires an authority")
	}
	if o.SignatureKeyID == "" {
		return fmt.Errorf("observation_ref requires a signature_key_id (it references a SIGNED receipt)")
	}
	if o.ObservedAt.IsZero() {
		return fmt.Errorf("observation_ref requires observed_at")
	}
	return nil
}

// ReceiptRef is an opaque, content-addressed pointer to a signed
// nexus.ActionReceipt (technical execution proof): a handle + receipt hash +
// signing key id, never the receipt body. Mirrors sdk/go/workcase.ReceiptRef.
// An ActionReceipt is bound to an Outcome for audit lineage only; it can never
// justify a satisfied conclusion.
type ReceiptRef struct {
	Handle         string `json:"handle"`
	ReceiptHash    string `json:"receipt_hash"`
	SignatureKeyID string `json:"signature_key_id"`
}

// Validate enforces the ReceiptRef rules.
func (r ReceiptRef) Validate() error {
	if r.Handle == "" || r.ReceiptHash == "" || r.SignatureKeyID == "" {
		return fmt.Errorf("receipt_ref requires handle, receipt_hash and signature_key_id")
	}
	return nil
}

// BlockerRef is an opaque reference to an external blocker that prevented (or
// disputes) an Outcome: a handle, a business kind, the authority that raised
// it and an optional bounded, permitted summary. Never raw enterprise content.
type BlockerRef struct {
	Handle    string `json:"handle"`
	Kind      string `json:"kind"`
	Authority string `json:"authority"`
	Summary   string `json:"summary,omitempty"`
}

// Validate enforces the BlockerRef rules.
func (b BlockerRef) Validate() error {
	if b.Handle == "" || b.Kind == "" || b.Authority == "" {
		return fmt.Errorf("blocker_ref requires handle, kind and authority")
	}
	if utf8.RuneCountInString(b.Summary) > 512 {
		return fmt.Errorf("blocker_ref summary must be a bounded permitted summary (<=512 chars)")
	}
	return nil
}

// ContributionRef credits one contributor (an actor, agent or method,
// identified by an opaque business id) with one kind of contribution to an
// Outcome. Crediting the same contributor for the same kind twice on one
// Outcome is a duplicate and is rejected.
type ContributionRef struct {
	ContributorID string  `json:"contributor_id"`
	Kind          string  `json:"kind"`
	Weight        float64 `json:"weight,omitempty"`
}

// Validate enforces the ContributionRef rules.
func (c ContributionRef) Validate() error {
	if c.ContributorID == "" || c.Kind == "" {
		return fmt.Errorf("contribution_ref requires contributor_id and kind")
	}
	if c.Weight < 0 || c.Weight > 1 {
		return fmt.Errorf("contribution_ref weight must be in [0,1]")
	}
	return nil
}

// ConfidenceComponent is one named, scored component of an Outcome's
// confidence (for example authority, freshness, coverage, consistency,
// attribution). Task 0H computes and interprets these; 0G only carries them.
type ConfidenceComponent struct {
	Kind  string  `json:"kind"`
	Score float64 `json:"score"`
}

// Validate enforces the ConfidenceComponent rules.
func (c ConfidenceComponent) Validate() error {
	if c.Kind == "" || utf8.RuneCountInString(c.Kind) > 64 {
		return fmt.Errorf("confidence component kind must contain 1..64 characters")
	}
	if c.Score < 0 || c.Score > 1 {
		return fmt.Errorf("confidence component score must be in [0,1]")
	}
	return nil
}

// OutcomeRevisionRef points from a correction revision to the exact prior
// Outcome revision it supersedes. It carries the prior revision number, the
// prior revision's deterministic id and a reason — never a mutable pointer
// that could rewrite history.
type OutcomeRevisionRef struct {
	OutcomeKey string `json:"outcome_key"`
	Revision   uint64 `json:"revision"`
	OutcomeID  string `json:"outcome_id"`
	Reason     string `json:"reason"`
}

// Validate enforces the OutcomeRevisionRef rules.
func (r OutcomeRevisionRef) Validate() error {
	if r.OutcomeKey == "" {
		return fmt.Errorf("supersedes.outcome_key is required")
	}
	if r.Revision < 1 {
		return fmt.Errorf("supersedes.revision must be >= 1")
	}
	if r.OutcomeID == "" {
		return fmt.Errorf("supersedes.outcome_id is required")
	}
	if r.Reason == "" {
		return fmt.Errorf("supersedes.reason is required")
	}
	return nil
}

// --- OutcomeClaim ----------------------------------------------------------

// OutcomeClaim is the asserted conclusion at the heart of an Outcome: that a
// specific versioned Goal reached a specific OutcomeStatus under a named
// evaluation-rule version, justified by opaque evidence/observation refs and
// qualified by opaque blocker refs and confidence components. A satisfied
// claim REQUIRES at least one authoritative, signed ObservationRef.
type OutcomeClaim struct {
	Goal         GoalRef               `json:"goal"`
	Status       OutcomeStatus         `json:"status"`
	RuleVersion  string                `json:"rule_version"`
	Evidence     []EvidenceRef         `json:"evidence,omitempty"`
	Observations []ObservationRef      `json:"observations,omitempty"`
	Blockers     []BlockerRef          `json:"blockers,omitempty"`
	Confidence   []ConfidenceComponent `json:"confidence,omitempty"`
}

// Validate enforces the OutcomeClaim rules, including the satisfied-requires-
// authoritative-observation invariant.
func (c OutcomeClaim) Validate() error {
	if err := c.Goal.Validate(); err != nil {
		return err
	}
	if !c.Status.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownOutcomeStatus, c.Status)
	}
	if c.RuleVersion == "" || utf8.RuneCountInString(c.RuleVersion) > 128 {
		return ErrMissingRuleVersion
	}
	for i, e := range c.Evidence {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("evidence[%d]: %w", i, err)
		}
	}
	seenObs := map[string]bool{}
	for i, o := range c.Observations {
		if err := o.Validate(); err != nil {
			return fmt.Errorf("observations[%d]: %w", i, err)
		}
		if seenObs[o.Handle] {
			return fmt.Errorf("observations[%d]: duplicate observation handle %q", i, o.Handle)
		}
		seenObs[o.Handle] = true
	}
	for i, b := range c.Blockers {
		if err := b.Validate(); err != nil {
			return fmt.Errorf("blockers[%d]: %w", i, err)
		}
	}
	seenConf := map[string]bool{}
	for i, cc := range c.Confidence {
		if err := cc.Validate(); err != nil {
			return fmt.Errorf("confidence[%d]: %w", i, err)
		}
		if seenConf[cc.Kind] {
			return fmt.Errorf("confidence[%d]: duplicate component kind %q", i, cc.Kind)
		}
		seenConf[cc.Kind] = true
	}
	// NOTE: the satisfied-requires-authoritative-observation invariant is
	// enforced by Outcome.Validate, not here: only the Outcome sees the
	// ActionReceipts, and distinguishing "no observation at all, only a
	// technical receipt" (ErrActionReceiptNotOutcomeEvidence) from "no
	// authoritative observation" (ErrSatisfiedNeedsObservation) needs that
	// Outcome-level context.
	return nil
}

// --- Outcome ---------------------------------------------------------------

// Outcome is one immutable, versioned governed result record: a business
// OutcomeClaim bound to a tenant, a stable OutcomeKey, a monotonic Revision,
// the exact governed-record versions it was decided against (WorkCase,
// WorkPlan, Operating Map, org), the technical ActionReceipts that executed
// the underlying work (audit lineage only — never satisfaction evidence), the
// contribution credits, the decision time and, for a correction, an explicit
// supersession pointer to the prior revision. An Outcome is never mutated in
// place; a changed conclusion is a NEW revision superseding the prior one, and
// the prior remains readable.
//
// ID is assigned by the persisting store (DerivedID); Validate does not check
// it, mirroring how sdk/go/operatingmap.Entry.Validate ignores service-assigned
// ID/Version.
type Outcome struct {
	ID                  string              `json:"id"`
	Tenant              string              `json:"tenant"`
	OutcomeKey          string              `json:"outcome_key"`
	Revision            uint64              `json:"revision"`
	Claim               OutcomeClaim        `json:"claim"`
	WorkCaseID          string              `json:"work_case_id"`
	WorkCaseRevision    uint64              `json:"work_case_revision"`
	WorkPlanRevision    uint64              `json:"work_plan_revision"`
	OperatingMapVersion int64               `json:"operating_map_version"`
	OrgVersion          int64               `json:"org_version"`
	ActionReceipts      []ReceiptRef        `json:"action_receipts,omitempty"`
	Contributions       []ContributionRef   `json:"contributions,omitempty"`
	DecidedAt           time.Time           `json:"decided_at"`
	Supersedes          *OutcomeRevisionRef `json:"supersedes,omitempty"`
}

// DerivedID returns the deterministic id for this Outcome revision, derived
// only from (tenant, "outcome", outcome_key, revision).
func (o Outcome) DerivedID() string {
	return StableID(o.Tenant, NodeOutcome, o.OutcomeKey, o.Revision)
}

// Validate enforces the full Outcome contract: tenant/key/revision presence,
// exact version binding, a valid claim, no duplicate credit, the action-
// receipt-is-not-outcome-evidence rule, coherent supersession lineage and the
// opaque-content boundary.
func (o Outcome) Validate() error {
	if o.Tenant == "" || utf8.RuneCountInString(o.Tenant) > 128 {
		return fmt.Errorf("tenant must contain 1..128 characters")
	}
	if o.OutcomeKey == "" || utf8.RuneCountInString(o.OutcomeKey) > 256 {
		return fmt.Errorf("outcome_key must contain 1..256 characters")
	}
	if o.Revision < 1 {
		return fmt.Errorf("revision must be >= 1")
	}
	if o.Claim.Goal.Tenant != o.Tenant {
		return fmt.Errorf("claim.goal.tenant %q must match the outcome tenant %q", o.Claim.Goal.Tenant, o.Tenant)
	}
	if err := o.Claim.Validate(); err != nil {
		return err
	}
	if o.WorkCaseID == "" {
		return fmt.Errorf("work_case_id is required")
	}
	if o.WorkCaseRevision < 1 {
		return fmt.Errorf("work_case_revision must be >= 1 (an Outcome binds an exact WorkCase revision)")
	}
	if o.WorkPlanRevision < 1 {
		return fmt.Errorf("work_plan_revision must be >= 1 (an Outcome binds an exact WorkPlan revision)")
	}
	if o.OperatingMapVersion < 1 {
		return fmt.Errorf("operating_map_version must be >= 1 (an Outcome binds an exact Operating Map version)")
	}
	if o.OrgVersion < 0 {
		return fmt.Errorf("org_version must be non-negative")
	}
	for i, r := range o.ActionReceipts {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("action_receipts[%d]: %w", i, err)
		}
	}
	// satisfied requires at least one AUTHORITATIVE, signed ObservationRef.
	// An ActionReceipt is technical execution proof only and can never justify
	// satisfaction: a satisfied Outcome whose ONLY justification is an
	// ActionReceipt (no observation at all) is the precise "action success is
	// not an outcome" error; any other missing/insufficient observation is the
	// plain missing-observation error.
	if o.Claim.Status == OutcomeSatisfied {
		hasAuthObs := false
		for _, obs := range o.Claim.Observations {
			if obs.IsAuthoritative() {
				hasAuthObs = true
				break
			}
		}
		if !hasAuthObs {
			if len(o.Claim.Observations) == 0 && len(o.ActionReceipts) > 0 {
				return ErrActionReceiptNotOutcomeEvidence
			}
			return ErrSatisfiedNeedsObservation
		}
	}
	seen := map[string]bool{}
	for i, c := range o.Contributions {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("contributions[%d]: %w", i, err)
		}
		key := c.ContributorID + "\x00" + c.Kind
		if seen[key] {
			return ErrDuplicateContribution
		}
		seen[key] = true
	}
	if o.DecidedAt.IsZero() {
		return fmt.Errorf("decided_at is required")
	}
	// Supersession coherence: revision 1 has no predecessor; every later
	// revision must name the immediately prior revision it supersedes.
	if o.Revision == 1 {
		if o.Supersedes != nil {
			return ErrUnexpectedSupersession
		}
	} else {
		if o.Supersedes == nil {
			return ErrMissingSupersession
		}
		if err := o.Supersedes.Validate(); err != nil {
			return err
		}
		if o.Supersedes.OutcomeKey != o.OutcomeKey {
			return fmt.Errorf("supersedes.outcome_key %q must match this outcome_key %q", o.Supersedes.OutcomeKey, o.OutcomeKey)
		}
		if o.Supersedes.Revision != o.Revision-1 {
			return fmt.Errorf("supersedes.revision %d must be the immediately prior revision %d", o.Supersedes.Revision, o.Revision-1)
		}
	}
	return NoRawContentLeak(o)
}

// --- lineage graph facts ---------------------------------------------------

// LineageEndpoint is a versioned reference to a lineage node: (tenant, type,
// business id, revision). It is always versioned — an unversioned endpoint is
// rejected.
type LineageEndpoint struct {
	Tenant     string   `json:"tenant"`
	Type       NodeType `json:"node_type"`
	BusinessID string   `json:"business_id"`
	Revision   uint64   `json:"revision"`
}

// Validate enforces the LineageEndpoint rules.
func (e LineageEndpoint) Validate() error {
	if e.Tenant == "" {
		return fmt.Errorf("endpoint tenant is required")
	}
	if !e.Type.Valid() {
		return fmt.Errorf("endpoint node_type %q is not a valid node type", e.Type)
	}
	if e.BusinessID == "" || e.Revision < 1 {
		return ErrUnversionedEndpoint
	}
	return nil
}

// NodeID returns the deterministic id of the node this endpoint references.
func (e LineageEndpoint) NodeID() string {
	return StableID(e.Tenant, e.Type, e.BusinessID, e.Revision)
}

// LineageNode is one immutable, versioned lineage node. Its ID derives only
// from (tenant, type, business id, revision); it may carry a bounded permitted
// summary but never raw content or a graph-provider id.
type LineageNode struct {
	ID         string   `json:"id"`
	Tenant     string   `json:"tenant"`
	Type       NodeType `json:"node_type"`
	BusinessID string   `json:"business_id"`
	Revision   uint64   `json:"revision"`
	Summary    string   `json:"summary,omitempty"`
}

// DerivedID returns the deterministic id for this node.
func (n LineageNode) DerivedID() string {
	return StableID(n.Tenant, n.Type, n.BusinessID, n.Revision)
}

// Validate enforces the LineageNode rules.
func (n LineageNode) Validate() error {
	if n.Tenant == "" {
		return fmt.Errorf("node tenant is required")
	}
	if !n.Type.Valid() {
		return fmt.Errorf("node_type %q is not a valid node type", n.Type)
	}
	if n.BusinessID == "" || n.Revision < 1 {
		return ErrUnversionedEndpoint
	}
	if utf8.RuneCountInString(n.Summary) > 512 {
		return fmt.Errorf("node summary must be a bounded permitted summary (<=512 chars)")
	}
	return NoRawContentLeak(n)
}

// LineageEdge is one immutable, versioned lineage edge between two versioned
// endpoints in the SAME tenant. A cross-tenant edge is rejected.
type LineageEdge struct {
	ID     string          `json:"id"`
	Tenant string          `json:"tenant"`
	Type   EdgeType        `json:"edge_type"`
	From   LineageEndpoint `json:"from"`
	To     LineageEndpoint `json:"to"`
}

// DerivedID returns the deterministic id for this edge, derived from
// (tenant, edge type, from node id, to node id).
func (e LineageEdge) DerivedID() string {
	h := sha256.New()
	for _, p := range []string{e.Tenant, string(e.Type), e.From.NodeID(), e.To.NodeID()} {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "oe_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// Validate enforces the LineageEdge rules: a valid closed edge type, two
// valid versioned endpoints, and both endpoints in the edge's own tenant.
func (e LineageEdge) Validate() error {
	if e.Tenant == "" {
		return fmt.Errorf("edge tenant is required")
	}
	if !e.Type.Valid() {
		return fmt.Errorf("edge_type %q is not a valid edge type", e.Type)
	}
	if err := e.From.Validate(); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := e.To.Validate(); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	if e.From.Tenant != e.Tenant || e.To.Tenant != e.Tenant {
		return ErrCrossTenantEdge
	}
	return nil
}

// --- projection outbox -----------------------------------------------------

// ProjectionEvent is one immutable authoritative rebuild input for the
// downstream Outcome Graph (Task 0I). Events are append-only and retained for
// the lifetime of the referenced governed record; a source deletion/revocation
// appends a tombstone (+ a bounded reevaluation) and never edits an existing
// event. Sequence is per-tenant monotonic and assigned by the persisting
// store.
type ProjectionEvent struct {
	Tenant             string              `json:"tenant"`
	Sequence           uint64              `json:"sequence"`
	Kind               ProjectionEventKind `json:"kind"`
	SubjectType        NodeType            `json:"subject_type"`
	SubjectID          string              `json:"subject_id"`
	SubjectRevision    uint64              `json:"subject_revision"`
	PayloadHash        string              `json:"payload_hash"`
	SupersedesSequence uint64              `json:"supersedes_sequence,omitempty"`
	RecordedAt         time.Time           `json:"recorded_at"`
}

// Validate enforces the ProjectionEvent rules. Sequence is not required here
// (the store assigns it), but when present it must be >= 1.
func (e ProjectionEvent) Validate() error {
	if e.Tenant == "" {
		return fmt.Errorf("projection event tenant is required")
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("projection event kind %q is not valid", e.Kind)
	}
	if !e.SubjectType.Valid() {
		return fmt.Errorf("projection event subject_type %q is not valid", e.SubjectType)
	}
	if e.SubjectID == "" {
		return fmt.Errorf("projection event subject_id is required")
	}
	if e.SubjectRevision < 1 {
		return fmt.Errorf("projection event subject_revision must be >= 1")
	}
	if e.PayloadHash == "" {
		return fmt.Errorf("projection event payload_hash is required")
	}
	if e.RecordedAt.IsZero() {
		return fmt.Errorf("projection event recorded_at is required")
	}
	return nil
}

// ProjectionWatermark tracks per-tenant projection progress: the last
// projection-event sequence a downstream reader has durably consumed. It is
// the one mutable record in this contract, and it only ever advances.
type ProjectionWatermark struct {
	Tenant       string    `json:"tenant"`
	LastSequence uint64    `json:"last_sequence"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Validate enforces the ProjectionWatermark rules.
func (w ProjectionWatermark) Validate() error {
	if w.Tenant == "" {
		return fmt.Errorf("watermark tenant is required")
	}
	if w.UpdatedAt.IsZero() {
		return fmt.Errorf("watermark updated_at is required")
	}
	return nil
}
