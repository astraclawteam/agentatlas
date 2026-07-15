// result.go freezes the B2 WORK-ASSESSMENT RESULT vocabulary (Task 18C): the
// versioned, immutable WorkAssessment an enterprise produces when it evaluates an
// employee's (or a hierarchy's) work against a PUBLISHED assessment Policy
// (result.go joins policy.go in the same `assessment` package and REUSES its
// EvidenceTier, AttributionMode, DimensionKey, ConfidenceLevel and Policy — it
// does not redeclare them). Like policy.go it is deliberately FAIR, DETERMINISTIC
// and GOVERNED by construction; five boundaries are load-bearing and enforced
// here so no downstream code can weaken them:
//
//   - DETERMINISM / SHUFFLE-INVARIANCE: a WorkAssessment is byte-identical after
//     ANY reordering of its inputs. Normalize sorts every nested collection by a
//     content key, so json.Marshal(a.Normalize()) is stable regardless of the
//     order evidence/outcomes/sub-assessments/components arrived in. DerivedID and
//     Digest are content-addressed and provider-neutral.
//   - THREE EVIDENCE TIERS: Evidence.Validate enforces that a verified_outcome
//     carries a verified authoritative fact AND a signed observation (authority +
//     signature key), an accepted_deliverable carries signed execution evidence,
//     and a human_report names its human source. The tier is a VERIFIED-FACT
//     classification, never free-form.
//   - not_assessable IS A FIRST-CLASS STATE: a DimensionResult is EITHER assessed
//     OR not_assessable (with a named reason). A not_assessable dimension may
//     never carry a counted/satisfied outcome — an invented score is structurally
//     impossible.
//   - FAIR BLOCKER/DELAY GRADING: blocker classification confidence
//     {verified,corroborated,reported,inferred} and delay attribution
//     {external,personal,process,resource,unattributed} are closed enums.
//     "personal" grades WORK/PROCESS facts, never the person's character
//     (consistent with policy.go's fairness stance).
//   - OPAQUE CONTENT ONLY: every reference is an opaque handle/versioned key + a
//     bounded permitted summary and enums — never raw enterprise content, a
//     connector endpoint or a credential. NoRawContentLeak (reused from policy.go)
//     enforces that boundary on every WorkAssessment.
//
// This file is stdlib-only (policy.go's constraint): it binds to the authoritative
// Outcome/Graph/WorkCase truth through OPAQUE, self-contained reference shapes;
// the deterministic evaluator that verifies those references against the real
// sdk/go/outcome, internal/outcome and internal/outcomegraph surfaces lives in
// services/agentatlas/internal/assessment (Task 18C). The LLM is a NARRATION PORT
// only: Narrative is a bounded, non-identity field the deterministic core leaves
// empty and a separate narration step may fill — it can never change a tier,
// attribution, dimension result or confidence.
package assessment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"
)

// maxNarrative bounds the LLM narration port output stored on an assessment.
const maxNarrative = 8192

// --- hierarchical level (closed enum) --------------------------------------

// AssessmentLevel is the hierarchical level a WorkAssessment evaluates, mirroring
// the Dream distillation levels. A manager's assessment aggregates its reports'
// sub-assessments and must count a shared Outcome ONCE across levels.
type AssessmentLevel string

const (
	LevelIndividual   AssessmentLevel = "individual"
	LevelGroup        AssessmentLevel = "group"
	LevelDepartment   AssessmentLevel = "department"
	LevelBusinessUnit AssessmentLevel = "business_unit"
	LevelCompany      AssessmentLevel = "company"
)

// AssessmentLevels returns the closed set in a stable order.
func AssessmentLevels() []AssessmentLevel {
	return []AssessmentLevel{LevelIndividual, LevelGroup, LevelDepartment, LevelBusinessUnit, LevelCompany}
}

// Valid reports whether l is a known level.
func (l AssessmentLevel) Valid() bool {
	switch l {
	case LevelIndividual, LevelGroup, LevelDepartment, LevelBusinessUnit, LevelCompany:
		return true
	}
	return false
}

// --- dimension result state (closed enum) ----------------------------------

// DimensionState is the closed state of a per-dimension result: either an
// evidence-grounded assessment OR the first-class not_assessable state.
type DimensionState string

const (
	StateAssessed      DimensionState = "assessed"
	StateNotAssessable DimensionState = "not_assessable"
)

// Valid reports whether s is a known dimension state.
func (s DimensionState) Valid() bool { return s == StateAssessed || s == StateNotAssessable }

// NotAssessableReason names exactly why a dimension could not be assessed. It is
// NEVER an invented or guessed score: missing, stale, unverifiable or conflicting
// evidence each maps to a distinct reason.
type NotAssessableReason string

const (
	ReasonInsufficientData NotAssessableReason = "insufficient_data"
	ReasonStaleGraph       NotAssessableReason = "stale_graph_watermark"
	ReasonUnverifiable     NotAssessableReason = "unverifiable_evidence"
	ReasonConflicting      NotAssessableReason = "conflicting_evidence"
)

// Valid reports whether r is a known reason.
func (r NotAssessableReason) Valid() bool {
	switch r {
	case ReasonInsufficientData, ReasonStaleGraph, ReasonUnverifiable, ReasonConflicting:
		return true
	}
	return false
}

// --- blocker classification (closed enums, fair) ----------------------------

// BlockerConfidence is the closed classification confidence of a blocker,
// computed deterministically from the evidence backing it.
type BlockerConfidence string

const (
	BlockerVerified     BlockerConfidence = "verified"     // backed by a signed observation/receipt
	BlockerCorroborated BlockerConfidence = "corroborated" // multiple independent reports agree
	BlockerReported     BlockerConfidence = "reported"     // a single named human report
	BlockerInferred     BlockerConfidence = "inferred"     // derived from indirect signals only
)

// Valid reports whether c is a known blocker confidence.
func (c BlockerConfidence) Valid() bool {
	switch c {
	case BlockerVerified, BlockerCorroborated, BlockerReported, BlockerInferred:
		return true
	}
	return false
}

// DelayAttribution is the closed, FAIR attribution of a delay. "personal" grades
// the assessed party's OWN work/process facts, never their character (consistent
// with policy.go's fairness stance); "unattributed" is the honest default when
// the evidence does not support any attribution.
type DelayAttribution string

const (
	DelayExternal     DelayAttribution = "external"
	DelayPersonal     DelayAttribution = "personal"
	DelayProcess      DelayAttribution = "process"
	DelayResource     DelayAttribution = "resource"
	DelayUnattributed DelayAttribution = "unattributed"
)

// Valid reports whether d is a known delay attribution.
func (d DelayAttribution) Valid() bool {
	switch d {
	case DelayExternal, DelayPersonal, DelayProcess, DelayResource, DelayUnattributed:
		return true
	}
	return false
}

// --- confidence explanation (deterministic metadata) ------------------------

// The seven deterministic confidence-component kinds (invariant 3). Confidence is
// EXPLANATORY metadata computed from verified facts — never randomness or an LLM.
const (
	ConfCoverage            = "coverage"
	ConfAuthority           = "authority"
	ConfFreshness           = "freshness"
	ConfConsistency         = "consistency"
	ConfAttributionClarity  = "attribution_clarity"
	ConfProjectionFreshness = "projection_freshness"
	ConfRuleClarity         = "rule_clarity"
)

// ConfidenceKinds returns the seven component kinds in a stable order.
func ConfidenceKinds() []string {
	return []string{ConfCoverage, ConfAuthority, ConfFreshness, ConfConsistency, ConfAttributionClarity, ConfProjectionFreshness, ConfRuleClarity}
}

// ConfidenceComponent is one named, bounded [0,1] confidence component.
type ConfidenceComponent struct {
	Kind  string  `json:"kind"`
	Score float64 `json:"score"`
}

// ConfidenceExplanation is the deterministic confidence metadata for a dimension:
// an overall level plus the seven named components it was derived from.
type ConfidenceExplanation struct {
	Level      ConfidenceLevel       `json:"level"`
	Components []ConfidenceComponent `json:"components"`
}

// Validate enforces that the explanation carries EXACTLY the seven known
// components (each once, bounded to [0,1]) and a valid level — so confidence is
// always fully explained, never a bare number.
func (c ConfidenceExplanation) Validate() error {
	if !c.Level.Valid() {
		return fmt.Errorf("assessment: confidence level %q is not valid", c.Level)
	}
	seen := map[string]bool{}
	for _, comp := range c.Components {
		if comp.Score < 0 || comp.Score > 1 {
			return fmt.Errorf("assessment: confidence component %q score must be in [0,1]", comp.Kind)
		}
		if seen[comp.Kind] {
			return fmt.Errorf("assessment: duplicate confidence component %q", comp.Kind)
		}
		seen[comp.Kind] = true
	}
	for _, k := range ConfidenceKinds() {
		if !seen[k] {
			return fmt.Errorf("assessment: confidence explanation is missing the %q component", k)
		}
	}
	if len(c.Components) != len(ConfidenceKinds()) {
		return errors.New("assessment: confidence explanation must carry exactly the seven known components")
	}
	return nil
}

// --- evidence (three tiers, verified facts) ---------------------------------

// Evidence is one opaque, tiered evidence item grounding a dimension result. The
// TIER is a verified-fact classification (Evidence.Validate enforces the tier's
// structural precondition); it can never be set to a higher tier than the facts
// support. It carries opaque handles/keys and a bounded summary — never raw
// content.
type Evidence struct {
	Tier           EvidenceTier `json:"tier"`
	Handle         string       `json:"handle"`
	Kind           string       `json:"kind"`
	Revision       uint64       `json:"revision,omitempty"`
	Authority      string       `json:"authority,omitempty"`
	SignatureKeyID string       `json:"signature_key_id,omitempty"`
	Verified       bool         `json:"verified"`
	Superseded     bool         `json:"superseded,omitempty"`
	Summary        string       `json:"summary,omitempty"`
}

// Validate enforces the per-tier structural precondition (invariant 2):
//   - verified_outcome: verified against an authoritative Outcome AND backed by a
//     signed observation (a named authority + a signing key id).
//   - accepted_deliverable: signed execution evidence (a signing key id).
//   - human_report: a named human authority (the lowest tier; unverified).
func (e Evidence) Validate() error {
	if !e.Tier.Valid() {
		return fmt.Errorf("assessment: evidence tier %q is not valid", e.Tier)
	}
	if e.Handle == "" || utf8.RuneCountInString(e.Handle) > 256 {
		return errors.New("assessment: evidence handle must contain 1..256 characters")
	}
	if e.Kind == "" || utf8.RuneCountInString(e.Kind) > 64 {
		return errors.New("assessment: evidence kind must contain 1..64 characters")
	}
	if utf8.RuneCountInString(e.Summary) > maxSummary {
		return fmt.Errorf("assessment: evidence summary must be a bounded permitted summary (<=%d chars)", maxSummary)
	}
	switch e.Tier {
	case TierVerifiedOutcome:
		if !e.Verified {
			return errors.New("assessment: a verified_outcome evidence item must be verified against an authoritative Outcome")
		}
		if e.Authority == "" || e.SignatureKeyID == "" {
			return errors.New("assessment: a verified_outcome must carry a signing authority and key (a signed ObservationReceipt)")
		}
	case TierAcceptedDeliverable:
		if e.SignatureKeyID == "" {
			return errors.New("assessment: an accepted_deliverable must carry signed execution evidence (a signature key id)")
		}
	case TierHumanReport:
		if e.Authority == "" {
			return errors.New("assessment: a human_report must name its human authority (source)")
		}
	}
	return nil
}

// --- blockers ---------------------------------------------------------------

// Blocker is one opaque blocker attached to a dimension result, with a
// deterministic classification confidence and a fair delay attribution.
type Blocker struct {
	Handle     string            `json:"handle"`
	Kind       string            `json:"kind"`
	Confidence BlockerConfidence `json:"confidence"`
	Delay      DelayAttribution  `json:"delay"`
	Summary    string            `json:"summary,omitempty"`
}

// Validate enforces the Blocker rules.
func (b Blocker) Validate() error {
	if b.Handle == "" || utf8.RuneCountInString(b.Handle) > 256 {
		return errors.New("assessment: blocker handle must contain 1..256 characters")
	}
	if b.Kind == "" || utf8.RuneCountInString(b.Kind) > 64 {
		return errors.New("assessment: blocker kind must contain 1..64 characters")
	}
	if !b.Confidence.Valid() {
		return fmt.Errorf("assessment: blocker confidence %q is not valid", b.Confidence)
	}
	if !b.Delay.Valid() {
		return fmt.Errorf("assessment: blocker delay attribution %q is not valid", b.Delay)
	}
	if utf8.RuneCountInString(b.Summary) > maxSummary {
		return fmt.Errorf("assessment: blocker summary must be a bounded permitted summary (<=%d chars)", maxSummary)
	}
	return nil
}

// --- contributions ----------------------------------------------------------

// Contribution credits one contributor with one kind of contribution to one
// counted Outcome, for attribution and cross-level de-duplication. PlanExternal
// marks a contribution that fell outside the plan but still counted (it is
// included, never silently dropped).
type Contribution struct {
	ContributorID string  `json:"contributor_id"`
	OutcomeKey    string  `json:"outcome_key"`
	Kind          string  `json:"kind"`
	Weight        float64 `json:"weight,omitempty"`
	PlanExternal  bool    `json:"plan_external,omitempty"`
}

// Validate enforces the Contribution rules.
func (c Contribution) Validate() error {
	if c.ContributorID == "" || c.OutcomeKey == "" || c.Kind == "" {
		return errors.New("assessment: contribution requires contributor_id, outcome_key and kind")
	}
	if c.Weight < 0 || c.Weight > 1 {
		return errors.New("assessment: contribution weight must be in [0,1]")
	}
	return nil
}

// --- per-dimension result ---------------------------------------------------

// DimensionResult is the per-dimension outcome of an assessment: EITHER an
// evidence-grounded assessment (State assessed) OR the first-class not_assessable
// state (State not_assessable with a named reason). A not_assessable dimension
// never carries a counted/satisfied outcome — an invented score is structurally
// impossible.
type DimensionResult struct {
	Dimension           DimensionKey          `json:"dimension"`
	State               DimensionState        `json:"state"`
	NotAssessableReason NotAssessableReason   `json:"not_assessable_reason,omitempty"`
	Attribution         AttributionMode       `json:"attribution"`
	CountedOutcomeKeys  []string              `json:"counted_outcome_keys,omitempty"`
	SatisfiedOutcomes   int                   `json:"satisfied_outcomes"`
	Evidence            []Evidence            `json:"evidence,omitempty"`
	Blockers            []Blocker             `json:"blockers,omitempty"`
	Contributions       []Contribution        `json:"contributions,omitempty"`
	Confidence          ConfidenceExplanation `json:"confidence"`
}

// Validate enforces the DimensionResult rules and the not_assessable invariant.
func (d DimensionResult) Validate() error {
	if d.Dimension == "" || !dimensionKeyPattern.MatchString(string(d.Dimension)) {
		return fmt.Errorf("%w (got %q)", ErrInvalidDimensionKey, d.Dimension)
	}
	if !d.State.Valid() {
		return fmt.Errorf("assessment: dimension state %q is not valid", d.State)
	}
	if !d.Attribution.Valid() {
		return fmt.Errorf("assessment: attribution mode %q is not valid", d.Attribution)
	}
	if d.SatisfiedOutcomes < 0 {
		return errors.New("assessment: satisfied_outcomes must be non-negative")
	}
	for i, e := range d.Evidence {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("evidence[%d]: %w", i, err)
		}
	}
	for i, b := range d.Blockers {
		if err := b.Validate(); err != nil {
			return fmt.Errorf("blockers[%d]: %w", i, err)
		}
	}
	seenContrib := map[string]bool{}
	for i, c := range d.Contributions {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("contributions[%d]: %w", i, err)
		}
		key := c.ContributorID + "\x00" + c.OutcomeKey + "\x00" + c.Kind
		if seenContrib[key] {
			return fmt.Errorf("assessment: duplicate contribution (%s)", key)
		}
		seenContrib[key] = true
	}
	if err := d.Confidence.Validate(); err != nil {
		return err
	}
	switch d.State {
	case StateNotAssessable:
		if !d.NotAssessableReason.Valid() {
			return fmt.Errorf("assessment: a not_assessable dimension must name a valid reason (got %q)", d.NotAssessableReason)
		}
		// A not_assessable dimension must never carry an invented score.
		if len(d.CountedOutcomeKeys) != 0 || d.SatisfiedOutcomes != 0 {
			return errors.New("assessment: a not_assessable dimension must not carry counted/satisfied outcomes (never an invented score)")
		}
	case StateAssessed:
		if d.NotAssessableReason != "" {
			return errors.New("assessment: an assessed dimension must not carry a not_assessable reason")
		}
		if d.SatisfiedOutcomes > len(d.CountedOutcomeKeys) {
			return errors.New("assessment: satisfied_outcomes cannot exceed the counted outcomes")
		}
	}
	return nil
}

// --- period + graph binding + manager confirmation --------------------------

// Period is the assessment period the WorkAssessment covers.
type Period struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Validate enforces a non-empty interval.
func (p Period) Validate() error {
	if p.Start.IsZero() || p.End.IsZero() {
		return errors.New("assessment: period start and end are required")
	}
	if !p.End.After(p.Start) {
		return errors.New("assessment: period end must be after start")
	}
	return nil
}

// GraphSchema records the EXACT Outcome Graph read model an assessment was
// grounded against: the provider and schema/protocol versions, the graph name and
// the exact projection watermark. Binding the watermark is load-bearing — a stale
// watermark blocks formal assessment.
type GraphSchema struct {
	Provider        string `json:"provider"`
	SchemaVersion   string `json:"schema_version"`
	ProtocolVersion string `json:"protocol_version"`
	GraphName       string `json:"graph_name"`
	Watermark       uint64 `json:"watermark"`
}

// Validate enforces the GraphSchema rules.
func (g GraphSchema) Validate() error {
	if g.Provider == "" || g.SchemaVersion == "" || g.ProtocolVersion == "" || g.GraphName == "" {
		return errors.New("assessment: graph schema must bind provider, schema/protocol versions and graph name")
	}
	return nil
}

// ManagerConfirmation records the responsible manager's confirmation of the
// assessment (a formal assessment binds it).
type ManagerConfirmation struct {
	Confirmed   bool      `json:"confirmed"`
	Manager     string    `json:"manager,omitempty"`
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
}

// --- WorkAssessment ---------------------------------------------------------

// WorkAssessment is one immutable, versioned work-assessment record bound to the
// assessment Policy revision, the org version, the goals/responsibilities, the
// period, the exact Outcome Graph schema/watermark, the per-dimension results and
// the manager confirmation. Version is the monotonic assessment version for the
// (subject, policy revision, period): a re-assessment is a NEW version, never an
// in-place edit, and running the SAME inputs again reproduces the SAME version's
// content exactly (deterministic). Narrative is the LLM narration-port output: a
// bounded, non-identity field the deterministic core leaves empty.
type WorkAssessment struct {
	ID             string              `json:"id"`
	Tenant         string              `json:"tenant"`
	Org            string              `json:"org,omitempty"`
	Subject        string              `json:"subject"`
	Level          AssessmentLevel     `json:"level"`
	PolicyKey      string              `json:"policy_key"`
	PolicyRevision int                 `json:"policy_revision"`
	Version        int                 `json:"version"`
	Formal         bool                `json:"formal"`
	OrgVersion     int64               `json:"org_version"`
	Sources        SourceBinding       `json:"sources"`
	Period         Period              `json:"period"`
	Graph          GraphSchema         `json:"graph"`
	Dimensions     []DimensionResult   `json:"dimensions"`
	SubAssessments []WorkAssessment    `json:"sub_assessments,omitempty"`
	Manager        ManagerConfirmation `json:"manager"`
	CreatedAt      time.Time           `json:"created_at"`
	Narrative      string              `json:"narrative,omitempty"`
}

// DerivedID returns the deterministic, provider-neutral id for this assessment,
// derived only from (tenant, subject, policy_key, policy_revision, version,
// period). The narrative and manager note never affect it; a re-assessment (a new
// version) yields a new id, while an identical re-run of one version yields the
// same id.
func (w WorkAssessment) DerivedID() string {
	h := sha256.New()
	parts := []string{
		w.Tenant, w.Subject, w.PolicyKey, fmt.Sprintf("%d", w.PolicyRevision), fmt.Sprintf("%d", w.Version),
		w.Period.Start.UTC().Format(time.RFC3339Nano), w.Period.End.UTC().Format(time.RFC3339Nano),
	}
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "wa_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// clone deep-copies the assessment via a JSON round-trip so a stored value can
// never be mutated in place by a caller holding a returned copy.
func (w WorkAssessment) clone() WorkAssessment {
	raw, _ := json.Marshal(w)
	var out WorkAssessment
	_ = json.Unmarshal(raw, &out)
	return out
}

// Normalize returns a deterministically-sorted canonical copy: every nested
// collection is ordered by a TOTAL order, so json.Marshal(w.Normalize()) is
// byte-identical regardless of the order the inputs arrived in. This is the
// shuffle-invariance guarantee (invariant 1) AND what anchors the immutability
// Digest.
//
// Every comparator is a TOTAL order: a readable partial key first, then a FINAL
// full-marshaled-JSON tiebreak over the ALREADY-normalized element. A partial key
// alone is NOT enough — two elements sharing the partial key (e.g. two human
// reports with the same handle but different authority, or two blockers with the
// same handle but different confidence) would otherwise be left in input order
// (Go's sort is not order-preserving across inputs), so a reordering of the inputs
// would change the bytes. The JSON tiebreak closes that class of bug the same way
// 18B's RuleDigest full-tuple sort does, and stays future-proof against new
// fields. Children are normalized BEFORE their parent collection is sorted so the
// tiebreak compares canonical bytes.
func (w WorkAssessment) Normalize() WorkAssessment {
	out := w.clone()
	sort.Slice(out.Sources.SOPs, func(i, j int) bool {
		a, b := out.Sources.SOPs[i], out.Sources.SOPs[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return jsonLess(a, b)
	})
	// Normalize each dimension's inner collections FIRST, then sort the dimensions
	// by a total order (the JSON tiebreak needs canonical children).
	for i := range out.Dimensions {
		normalizeDimension(&out.Dimensions[i])
	}
	sort.Slice(out.Dimensions, func(i, j int) bool {
		a, b := out.Dimensions[i], out.Dimensions[j]
		if a.Dimension != b.Dimension {
			return a.Dimension < b.Dimension
		}
		return jsonLess(a, b)
	})
	// Normalize each sub-assessment FIRST (recursively), then sort by a total order:
	// two sub-assessments can share a DerivedID (same subject/policy/version/period)
	// yet differ in content, so DerivedID alone is not enough.
	for i := range out.SubAssessments {
		out.SubAssessments[i] = out.SubAssessments[i].Normalize()
	}
	sort.Slice(out.SubAssessments, func(i, j int) bool {
		a, b := out.SubAssessments[i], out.SubAssessments[j]
		if ai, bi := a.DerivedID(), b.DerivedID(); ai != bi {
			return ai < bi
		}
		return jsonLess(a, b)
	})
	return out
}

func normalizeDimension(d *DimensionResult) {
	sort.Slice(d.Evidence, func(i, j int) bool {
		a, b := d.Evidence[i], d.Evidence[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Handle != b.Handle {
			return a.Handle < b.Handle
		}
		if a.Revision != b.Revision {
			return a.Revision < b.Revision
		}
		return jsonLess(a, b)
	})
	sort.Slice(d.Blockers, func(i, j int) bool {
		a, b := d.Blockers[i], d.Blockers[j]
		if a.Handle != b.Handle {
			return a.Handle < b.Handle
		}
		return jsonLess(a, b)
	})
	sort.Slice(d.Contributions, func(i, j int) bool {
		a, b := d.Contributions[i], d.Contributions[j]
		if a.ContributorID != b.ContributorID {
			return a.ContributorID < b.ContributorID
		}
		if a.OutcomeKey != b.OutcomeKey {
			return a.OutcomeKey < b.OutcomeKey
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return jsonLess(a, b)
	})
	sort.Strings(d.CountedOutcomeKeys)
	sort.Slice(d.Confidence.Components, func(i, j int) bool {
		a, b := d.Confidence.Components[i], d.Confidence.Components[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return jsonLess(a, b)
	})
}

// jsonLess is the final TOTAL-order tiebreak: it compares the full marshaled JSON
// of two already-normalized elements. Two byte-identical elements compare equal
// (their relative order cannot affect the serialized output), so this makes every
// comparator a total order regardless of which fields exist — deterministic and
// stdlib-only.
func jsonLess(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Compare(ab, bb) < 0
}

// CountedOutcomeKeys returns the sorted, de-duplicated union of every Outcome key
// counted across this assessment's dimensions AND (recursively) its
// sub-assessments — the set a parent level excludes to avoid double-counting a
// shared Outcome across levels.
func (w WorkAssessment) CountedOutcomeKeys() []string {
	set := map[string]bool{}
	for _, d := range w.Dimensions {
		for _, k := range d.CountedOutcomeKeys {
			set[k] = true
		}
	}
	for _, sub := range w.SubAssessments {
		for _, k := range sub.CountedOutcomeKeys() {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Digest returns a deterministic content digest of the NORMALIZED structured
// result with the service-assigned id, the created-at stamp and the LLM narrative
// zeroed (recursively): re-narrating or re-persisting an assessment leaves the
// structural digest unchanged, while any change to a computed fact changes it.
func (w WorkAssessment) Digest() string {
	n := w.Normalize()
	zeroVolatile(&n)
	raw, _ := json.Marshal(n)
	sum := sha256.Sum256(raw)
	return "wad_" + hex.EncodeToString(sum[:])
}

func zeroVolatile(w *WorkAssessment) {
	w.ID = ""
	w.CreatedAt = time.Time{}
	w.Narrative = ""
	for i := range w.SubAssessments {
		zeroVolatile(&w.SubAssessments[i])
	}
}

// Validate enforces the full WorkAssessment contract: identity/scope, the source
// binding, the period, the graph binding, at least one dimension, valid
// per-dimension results with no duplicate dimension, a bounded narrative, valid
// sub-assessments and the opaque-content boundary.
func (w WorkAssessment) Validate() error {
	if w.Tenant == "" || utf8.RuneCountInString(w.Tenant) > 128 {
		return errors.New("assessment: tenant must contain 1..128 characters")
	}
	if w.Subject == "" || utf8.RuneCountInString(w.Subject) > 256 {
		return errors.New("assessment: subject must contain 1..256 characters")
	}
	if !w.Level.Valid() {
		return fmt.Errorf("assessment: level %q is not valid", w.Level)
	}
	if w.PolicyKey == "" || utf8.RuneCountInString(w.PolicyKey) > 256 {
		return errors.New("assessment: policy_key must contain 1..256 characters")
	}
	if w.PolicyRevision < 1 {
		return errors.New("assessment: policy_revision must be >= 1")
	}
	if w.Version < 1 {
		return errors.New("assessment: version must be >= 1 (a re-assessment is a new version)")
	}
	if w.OrgVersion < 0 {
		return errors.New("assessment: org_version must be non-negative")
	}
	if err := w.Sources.Validate(); err != nil {
		return fmt.Errorf("sources: %w", err)
	}
	if err := w.Period.Validate(); err != nil {
		return err
	}
	if err := w.Graph.Validate(); err != nil {
		return err
	}
	if len(w.Dimensions) == 0 {
		return errors.New("assessment: an assessment must carry at least one dimension result")
	}
	seen := map[DimensionKey]bool{}
	for i, d := range w.Dimensions {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("dimensions[%d]: %w", i, err)
		}
		if seen[d.Dimension] {
			return fmt.Errorf("assessment: dimension %q assessed more than once", d.Dimension)
		}
		seen[d.Dimension] = true
	}
	if utf8.RuneCountInString(w.Narrative) > maxNarrative {
		return fmt.Errorf("assessment: narrative must be a bounded permitted summary (<=%d chars)", maxNarrative)
	}
	for i, sub := range w.SubAssessments {
		if err := sub.Validate(); err != nil {
			return fmt.Errorf("sub_assessments[%d]: %w", i, err)
		}
	}
	return NoRawContentLeak(w)
}
