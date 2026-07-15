// Package assessment freezes the B2 employee work-assessment POLICY vocabulary:
// the versioned, governed rubric an enterprise evaluates work against. It is the
// foundation of the B2 assessment product line (Task 18B) and it is deliberately
// FAIR and GOVERNED by construction — the SDK enforces three load-bearing
// boundaries that no downstream code can weaken:
//
//   - Statuses are a CLOSED lifecycle set {draft, shadow, reviewing, published,
//     retired}. A published/retired revision is a frozen governed record.
//   - FAIRNESS (screened, then governed): personality, loyalty, private-life and
//     other forbidden categories, plus any hidden/undisclosed dimension, are
//     rejected at Dimension.Validate and re-screened in the service. That denylist
//     is a BEST-EFFORT SCREENING SIGNAL for common English forbidden terms —
//     applied to the normalized dimension key+title+rationale AND to every rule
//     rationale — and it cannot catch every euphemism, translation or homoglyph in
//     free-text human fields. Dimension KEYS are additionally constrained to a
//     lowercase-ASCII identifier so a homoglyph key cannot masquerade as a
//     built-in; titles and rationales stay free i18n text. The AUTHORITATIVE
//     fairness gate is the governed human review, which receives the FULL rubric
//     (every dimension key/title/rationale plus the evidence/attribution/
//     confidence summaries) through the publication handoff. Only the six built-in
//     candidate dimensions (outcome completion, quality, timeliness,
//     collaboration/handoff, risk/compliance, improvement contribution) or
//     governed, non-forbidden additions are permitted, and even those are
//     SUGGESTIONS — never a formal scoring policy — until publication.
//   - NON-FORMAL BEFORE PUBLICATION: a policy backs a FORMAL score only when it
//     is published (FormalScoringActive). A shadow-cycle output is explicitly
//     non-formal (ShadowCycle.IsFormal is always false), so a pre-cycle-1 or
//     shadow assessment can never be presented as a formal score.
//
// The publication gate is a strict, deterministic state machine
// (EvaluatePublicationGate): shadow cycle 1 is mandatory and must cover a
// complete assessment period; publication is allowed only when evidence-rule
// coverage is at least 90%, no formal dimension has low confidence, no rule
// changed after the cycle began, no correction or calibration issue remains
// unresolved, AND both the responsible manager and the assessment-policy owner
// confirm the calibration. A missing or ambiguous gate FAILS CLOSED (does not
// publish). Every reference the policy binds is OPAQUE (a versioned key or a
// handle/hash and a bounded permitted summary) — never a connector endpoint,
// credential or raw enterprise content; NoRawContentLeak enforces that boundary
// on every Policy.
//
// This package is self-contained (stdlib only) so any consumer can import it
// without pulling in service internals. Authoritative persistence and the
// governed lifecycle live in services/agentatlas/internal/assessment (Task 18B).
package assessment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"
)

// MinEvidenceCoverage is the minimum evidence-rule coverage a shadow cycle must
// reach before a policy may publish (90%).
const MinEvidenceCoverage = 0.90

// MaxShadowCycles is the hard cap on shadow cycles per policy revision: cycle 1
// is mandatory, exactly one cycle 2 is allowed on failure, and there is NEVER an
// automatic third cycle (a second failure returns the policy to draft).
const MaxShadowCycles = 2

// maxSummary bounds every permitted human string (title, rationale, summary),
// matching the sdk/go/outcome node-summary bound.
const maxSummary = 512

// --- Status (closed lifecycle enum) ----------------------------------------

// Status is the closed lifecycle state of an assessment policy revision.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusShadow    Status = "shadow"
	StatusReviewing Status = "reviewing"
	StatusPublished Status = "published"
	StatusRetired   Status = "retired"
)

// Statuses returns the closed set in a stable order.
func Statuses() []Status {
	return []Status{StatusDraft, StatusShadow, StatusReviewing, StatusPublished, StatusRetired}
}

// Valid reports whether s is one of the closed statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusShadow, StatusReviewing, StatusPublished, StatusRetired:
		return true
	}
	return false
}

// IsTerminal reports whether s is a FROZEN governed revision (published or
// retired). A terminal revision is immutable; a rule edit is a new revision.
func (s Status) IsTerminal() bool {
	return s == StatusPublished || s == StatusRetired
}

// --- Dimension keys (the six built-in candidates) --------------------------

// DimensionKey identifies an assessment dimension. The six built-ins are
// SUGGESTIONS proposed at draft time, never a formal scoring policy before
// publication.
type DimensionKey string

const (
	DimOutcomeCompletion       DimensionKey = "outcome_completion"
	DimQuality                 DimensionKey = "quality"
	DimTimeliness              DimensionKey = "timeliness"
	DimCollaboration           DimensionKey = "collaboration"
	DimRiskCompliance          DimensionKey = "risk_compliance"
	DimImprovementContribution DimensionKey = "improvement_contribution"
)

// BuiltinDimensionKeys returns the six built-in candidate dimensions in a stable
// order.
func BuiltinDimensionKeys() []DimensionKey {
	return []DimensionKey{
		DimOutcomeCompletion, DimQuality, DimTimeliness,
		DimCollaboration, DimRiskCompliance, DimImprovementContribution,
	}
}

// IsBuiltin reports whether k is one of the six built-in candidate dimensions.
func (k DimensionKey) IsBuiltin() bool {
	switch k {
	case DimOutcomeCompletion, DimQuality, DimTimeliness,
		DimCollaboration, DimRiskCompliance, DimImprovementContribution:
		return true
	}
	return false
}

// --- confidence / evidence / attribution enums -----------------------------

// ConfidenceLevel is the closed confidence classification of a scoring rule.
type ConfidenceLevel string

const (
	ConfidenceHigh   ConfidenceLevel = "high"
	ConfidenceMedium ConfidenceLevel = "medium"
	ConfidenceLow    ConfidenceLevel = "low"
)

// Valid reports whether c is a known confidence level.
func (c ConfidenceLevel) Valid() bool {
	switch c {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		return true
	}
	return false
}

// EvidenceTier is the closed evidence tier an EvidenceRule requires. It mirrors
// the Task 18C evidence tiers: a verified Outcome backed by business-system fact
// and a signed observation (highest), an accepted WorkCase deliverable/milestone
// with signed execution evidence, and a human report (lowest).
type EvidenceTier string

const (
	TierVerifiedOutcome     EvidenceTier = "verified_outcome"
	TierAcceptedDeliverable EvidenceTier = "accepted_deliverable"
	TierHumanReport         EvidenceTier = "human_report"
)

// Valid reports whether t is a known evidence tier.
func (t EvidenceTier) Valid() bool {
	switch t {
	case TierVerifiedOutcome, TierAcceptedDeliverable, TierHumanReport:
		return true
	}
	return false
}

// AttributionMode is the closed attribution policy for a dimension: a shared
// outcome is attributed once, a contribution is weighted, or a blocked outcome
// is excluded from the assessed party.
type AttributionMode string

const (
	AttrSharedOnce           AttributionMode = "shared_once"
	AttrContributionWeighted AttributionMode = "contribution_weighted"
	AttrBlockerExcluded      AttributionMode = "blocker_excluded"
)

// Valid reports whether m is a known attribution mode.
func (m AttributionMode) Valid() bool {
	switch m {
	case AttrSharedOnce, AttrContributionWeighted, AttrBlockerExcluded:
		return true
	}
	return false
}

// --- fairness guard --------------------------------------------------------

// ErrForbiddenDimension rejects any dimension that assesses a forbidden
// category — personality, loyalty, private life or any protected personal
// attribute — or that is hidden/undisclosed. B2 assessments are evidence-graded
// on WORK, never on the person.
var ErrForbiddenDimension = errors.New("assessment: forbidden dimension (personality, loyalty, private-life, protected personal attributes and hidden/undisclosed dimensions are not permitted; assessments grade work, not the person)")

// ErrInvalidDimensionKey rejects a dimension key that is not a strict
// lowercase-ASCII identifier (^[a-z][a-z0-9_]*$). Keys are machine identifiers,
// never display text, so this closes a homoglyph or mixed-script key
// masquerading as a built-in. Titles and rationales stay free i18n text.
var ErrInvalidDimensionKey = errors.New("assessment: dimension key must be a lowercase ASCII identifier ^[a-z][a-z0-9_]*$ (a machine key, not display text)")

// forbiddenDimensionPattern is a BEST-EFFORT screening signal, NOT an
// authoritative fairness gate (that is the governed human review, which receives
// the full rubric via the publication handoff). It matches, case-insensitively
// and AFTER separator normalization (normalizeForScan), a denylist of common
// ENGLISH forbidden subjects. It is applied to the normalized dimension
// key+title+rationale AND to every rule rationale, so a forbidden subject cannot
// hide in a rationale behind an innocuous title — but it CANNOT catch every
// euphemism, translation or homoglyph in free-text human fields. None of the six
// built-in keys/titles or a legitimate, work-grounded rationale trips it.
var forbiddenDimensionPattern = regexp.MustCompile(`(?i)` +
	`personalit` + // personality
	`|temperament` +
	`|\bloyal` + // loyal, loyalty, loyalties
	`|allegiance` +
	`|obedien` + // obedience, obedient
	`|private[\s_-]?life` +
	`|personal[\s_-]?life` +
	`|\bmarital` +
	`|\breligio` + // religion, religious
	`|\bpregnan` + // pregnancy, pregnant
	`|\bpolitic` + // politics, political
	`|sexual[\s_-]?orient` +
	`|ethnic[\s_-]?(?:origin|background)` +
	`|\bcaste\b` +
	`|union[\s_-]?member`)

// separatorRun matches a run of ASCII whitespace, underscore or hyphen.
var separatorRun = regexp.MustCompile(`[\s_-]+`)

// normalizeForScan collapses every run of whitespace/underscore/hyphen to a
// single ASCII space so a forbidden term cannot slip past the denylist behind
// doubled or mixed separators (e.g. "Private  life", "private_-_life"). It only
// touches ASCII separators, so non-ASCII i18n text (e.g. Chinese) is unchanged.
func normalizeForScan(s string) string {
	return separatorRun.ReplaceAllString(s, " ")
}

// scanForbidden re-screens a free-text field for a forbidden category AFTER
// separator normalization, returning a wrapped ErrForbiddenDimension on a hit.
// Best-effort screening only (see forbiddenDimensionPattern): the authoritative
// fairness gate is the governed human review.
func scanForbidden(text string) error {
	norm := normalizeForScan(text)
	if loc := forbiddenDimensionPattern.FindStringIndex(norm); loc != nil {
		return fmt.Errorf("%w: text matches a forbidden category near %q", ErrForbiddenDimension, norm[loc[0]:min(loc[1]+16, len(norm))])
	}
	return nil
}

// dimensionKeyPattern constrains a dimension KEY to a strict lowercase-ASCII
// identifier. Keys are machine identifiers, never display text, so this closes a
// homoglyph/mixed-script key masquerading as a built-in. Titles and rationales
// remain free i18n text and are screened, not ASCII-restricted.
var dimensionKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// --- opaque-content guard (mirrors sdk/go/outcome.NoRawContentLeak) ---------

// connectorShapePattern is the same forbidden connector/credential/raw-query
// scanner sdk/go/outcome and sdk/go/operatingmap use, re-declared here so this
// package stays self-contained. A policy carries opaque handles/hashes,
// versioned keys and bounded permitted summaries only.
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

// ErrRawContentLeak rejects a policy whose marshaled form matches a connector,
// credential or raw-query shape.
var ErrRawContentLeak = errors.New("assessment: content matches a forbidden connector, credential or raw-query shape; an assessment policy carries opaque handles, versioned keys and permitted summaries only")

// NoRawContentLeak marshals v to JSON and reports ErrRawContentLeak if the
// result contains anything shaped like a connector endpoint, credential or raw
// query.
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

// --- opaque reference types ------------------------------------------------

// VersionedRef is an opaque, versioned pointer to a governed record the policy
// binds — a job responsibility, an organization goal, an SOP or an
// acceptance/review definition. It carries a business key and a version only.
type VersionedRef struct {
	Key     string `json:"key"`
	Version int64  `json:"version"`
}

// Validate enforces the VersionedRef rules.
func (r VersionedRef) Validate() error {
	if r.Key == "" || utf8.RuneCountInString(r.Key) > 256 {
		return errors.New("assessment: versioned ref key must contain 1..256 characters")
	}
	if r.Version < 1 {
		return errors.New("assessment: versioned ref must bind a version >= 1")
	}
	return nil
}

// SourceBinding records the exact governed-record versions a policy draft was
// distilled from: the org version, the versioned job responsibility, the
// organization goal, the SOP set and the acceptance/review definition. It is the
// "what the rubric is grounded in" half of the draft; the Outcome Graph
// watermark and evidence links are the "what history justifies it" half.
type SourceBinding struct {
	OrgVersion        int64          `json:"org_version"`
	JobResponsibility VersionedRef   `json:"job_responsibility"`
	OrgGoal           VersionedRef   `json:"org_goal"`
	SOPs              []VersionedRef `json:"sops,omitempty"`
	AcceptanceReview  VersionedRef   `json:"acceptance_review"`
}

// Validate enforces the SourceBinding rules.
func (b SourceBinding) Validate() error {
	if b.OrgVersion < 0 {
		return errors.New("assessment: org_version must be non-negative")
	}
	if err := b.JobResponsibility.Validate(); err != nil {
		return fmt.Errorf("job_responsibility: %w", err)
	}
	if err := b.OrgGoal.Validate(); err != nil {
		return fmt.Errorf("org_goal: %w", err)
	}
	for i, s := range b.SOPs {
		if err := s.Validate(); err != nil {
			return fmt.Errorf("sops[%d]: %w", i, err)
		}
	}
	if err := b.AcceptanceReview.Validate(); err != nil {
		return fmt.Errorf("acceptance_review: %w", err)
	}
	return nil
}

// EvidenceLink is an opaque, bounded pointer to the historical evidence that
// grounds WHY a dimension or rule was proposed: a handle (an opaque graph/
// outcome descriptor), a business kind and a bounded permitted summary. Never
// raw content.
type EvidenceLink struct {
	Handle  string `json:"handle"`
	Kind    string `json:"kind"`
	Summary string `json:"summary,omitempty"`
}

// Validate enforces the EvidenceLink rules.
func (e EvidenceLink) Validate() error {
	if e.Handle == "" || utf8.RuneCountInString(e.Handle) > 256 {
		return errors.New("assessment: evidence link handle must contain 1..256 characters")
	}
	if e.Kind == "" || utf8.RuneCountInString(e.Kind) > 64 {
		return errors.New("assessment: evidence link kind must contain 1..64 characters")
	}
	if utf8.RuneCountInString(e.Summary) > maxSummary {
		return fmt.Errorf("assessment: evidence link summary must be a bounded permitted summary (<=%d chars)", maxSummary)
	}
	return nil
}

// --- Dimension -------------------------------------------------------------

// Dimension is one assessment dimension. Formal expresses the INTENT to score it
// formally once the policy publishes; the EFFECT is non-formal until publication
// (FormalScoringActive). Hidden is always rejected. Rationale records WHY the
// dimension was proposed and Evidence links the grounding descriptors.
type Dimension struct {
	Key       DimensionKey   `json:"key"`
	Title     string         `json:"title"`
	Formal    bool           `json:"formal"`
	Hidden    bool           `json:"hidden,omitempty"`
	Rationale string         `json:"rationale"`
	Evidence  []EvidenceLink `json:"evidence,omitempty"`
}

// Validate enforces the Dimension rules and the FAIRNESS SCREEN: a hidden
// dimension is hard-rejected; the dimension KEY must be a lowercase-ASCII
// identifier (dimensionKeyPattern) so a homoglyph key cannot masquerade as a
// built-in; and the best-effort denylist re-screens the normalized
// key+title+rationale for a forbidden category. The denylist is a SCREENING
// SIGNAL only (see forbiddenDimensionPattern) — the governed human review is the
// authoritative fairness gate. Titles and rationales stay free i18n text (e.g.
// Chinese) and are NOT ASCII-restricted. A built-in key or a governed,
// non-forbidden addition is permitted.
func (d Dimension) Validate() error {
	if d.Key == "" || utf8.RuneCountInString(string(d.Key)) > 128 {
		return errors.New("assessment: dimension key must contain 1..128 characters")
	}
	if !dimensionKeyPattern.MatchString(string(d.Key)) {
		return fmt.Errorf("%w (got %q)", ErrInvalidDimensionKey, d.Key)
	}
	if d.Title == "" || utf8.RuneCountInString(d.Title) > 256 {
		return errors.New("assessment: dimension title must contain 1..256 characters")
	}
	if d.Rationale == "" || utf8.RuneCountInString(d.Rationale) > maxSummary {
		return fmt.Errorf("assessment: dimension rationale must contain 1..%d characters (WHY it was proposed)", maxSummary)
	}
	for i, e := range d.Evidence {
		if err := e.Validate(); err != nil {
			return fmt.Errorf("evidence[%d]: %w", i, err)
		}
	}
	if d.Hidden {
		return fmt.Errorf("%w: dimension %q is hidden/undisclosed", ErrForbiddenDimension, d.Key)
	}
	// Best-effort fairness screen over the whole dimension (key + title +
	// rationale), normalized so a forbidden subject cannot hide behind doubled
	// separators or in a rationale behind an innocuous title.
	if raw, err := json.Marshal(d); err == nil {
		if err := scanForbidden(string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// --- rules -----------------------------------------------------------------

// EvidenceRule declares the evidence tier that grounds a dimension's score and
// WHY. Coverage of these rules over an assessment period is the >=90% publication
// gate input.
type EvidenceRule struct {
	Dimension DimensionKey `json:"dimension"`
	Tier      EvidenceTier `json:"tier"`
	Rationale string       `json:"rationale"`
}

// Validate enforces the EvidenceRule rules.
func (r EvidenceRule) Validate() error {
	if !r.Tier.Valid() {
		return fmt.Errorf("assessment: evidence rule tier %q is not valid", r.Tier)
	}
	return ruleRationale(r.Rationale)
}

// AttributionRule declares how a dimension attributes shared/blocked outcomes.
type AttributionRule struct {
	Dimension DimensionKey    `json:"dimension"`
	Mode      AttributionMode `json:"mode"`
	Rationale string          `json:"rationale"`
}

// Validate enforces the AttributionRule rules.
func (r AttributionRule) Validate() error {
	if !r.Mode.Valid() {
		return fmt.Errorf("assessment: attribution rule mode %q is not valid", r.Mode)
	}
	return ruleRationale(r.Rationale)
}

// ConfidenceRule declares the confidence level of a dimension's scoring. The
// publication gate forbids a FORMAL dimension carrying low confidence.
type ConfidenceRule struct {
	Dimension DimensionKey    `json:"dimension"`
	Level     ConfidenceLevel `json:"level"`
	Rationale string          `json:"rationale"`
}

// Validate enforces the ConfidenceRule rules.
func (r ConfidenceRule) Validate() error {
	if !r.Level.Valid() {
		return fmt.Errorf("assessment: confidence rule level %q is not valid", r.Level)
	}
	return ruleRationale(r.Rationale)
}

func ruleRationale(s string) error {
	if s == "" || utf8.RuneCountInString(s) > maxSummary {
		return fmt.Errorf("assessment: rule rationale must contain 1..%d characters", maxSummary)
	}
	// A forbidden subject must not hide in a rule rationale (best-effort screen).
	return scanForbidden(s)
}

// --- ShadowCycle -----------------------------------------------------------

// GateReason names exactly one publication-gate failure. Each maps to a distinct
// deterministic branch of EvaluatePublicationGate.
type GateReason string

const (
	GateIncompleteCycle         GateReason = "cycle_incomplete_or_partial_period"
	GateCoverageBelowThreshold  GateReason = "evidence_coverage_below_90pct"
	GateLowConfidenceFormal     GateReason = "formal_dimension_low_confidence"
	GateRuleChangedAfterStart   GateReason = "rule_changed_after_cycle_began"
	GateUnresolvedCorrection    GateReason = "unresolved_correction"
	GateUnresolvedCalibration   GateReason = "unresolved_calibration"
	GateManagerNotConfirmed     GateReason = "manager_not_confirmed"
	GatePolicyOwnerNotConfirmed GateReason = "policy_owner_not_confirmed"
)

// ShadowCycle is one complete, non-formal shadow assessment cycle over a policy
// assessment period. It binds the exact revision + rule digest + Outcome Graph
// watermark it ran against, and records the deterministic gate inputs observed
// at completion. A shadow-cycle output is NEVER a formal score (IsFormal is
// always false).
type ShadowCycle struct {
	Cycle       int       `json:"cycle"`
	Revision    int       `json:"revision"`
	RuleDigest  string    `json:"rule_digest"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Watermark   uint64    `json:"watermark"`

	// Deterministic gate inputs observed for the completed period.
	EvidenceCoverage       float64 `json:"evidence_coverage"`
	RuleChangedAfterStart  bool    `json:"rule_changed_after_start"`
	UnresolvedCorrections  int     `json:"unresolved_corrections"`
	UnresolvedCalibrations int     `json:"unresolved_calibrations"`
	ManagerConfirmed       bool    `json:"manager_confirmed"`
	PolicyOwnerConfirmed   bool    `json:"policy_owner_confirmed"`

	// Verdict recorded when the cycle is evaluated (never authoritative on its
	// own — EvaluatePublicationGate recomputes deterministically).
	Passed      bool         `json:"passed"`
	FailReasons []GateReason `json:"fail_reasons,omitempty"`
}

// IsFormal reports whether a shadow-cycle output is a formal score. It is ALWAYS
// false: a shadow assessment is explicitly non-formal and can never be presented
// as a formal score.
func (c ShadowCycle) IsFormal() bool { return false }

// IsComplete reports whether the cycle covers a complete assessment period: it
// started, completed, and its period is a non-empty interval.
func (c ShadowCycle) IsComplete() bool {
	return !c.StartedAt.IsZero() && !c.CompletedAt.IsZero() &&
		!c.PeriodStart.IsZero() && c.PeriodEnd.After(c.PeriodStart) &&
		!c.CompletedAt.Before(c.PeriodEnd)
}

// Validate enforces the ShadowCycle rules.
func (c ShadowCycle) Validate() error {
	if c.Cycle < 1 || c.Cycle > MaxShadowCycles {
		return fmt.Errorf("assessment: shadow cycle number must be 1..%d", MaxShadowCycles)
	}
	if c.Revision < 1 {
		return errors.New("assessment: shadow cycle must bind a policy revision >= 1")
	}
	if c.EvidenceCoverage < 0 || c.EvidenceCoverage > 1 {
		return errors.New("assessment: shadow cycle evidence_coverage must be in [0,1]")
	}
	if c.UnresolvedCorrections < 0 || c.UnresolvedCalibrations < 0 {
		return errors.New("assessment: shadow cycle unresolved counts must be non-negative")
	}
	return nil
}

// --- Policy ----------------------------------------------------------------

// Policy is one immutable, versioned assessment-policy revision. ID derives
// deterministically from (tenant, policy_key, revision). A rule edit is a NEW
// revision that restarts the shadow count; a published/retired revision is
// frozen. The rubric (dimensions + evidence/attribution/confidence rules) is
// bound to the exact governed-record versions it was distilled from (Sources)
// and the exact Outcome Graph watermark (Watermark).
type Policy struct {
	ID        string `json:"id"`
	Tenant    string `json:"tenant"`
	Org       string `json:"org,omitempty"`
	PolicyKey string `json:"policy_key"`
	Revision  int    `json:"revision"`
	Status    Status `json:"status"`

	Sources   SourceBinding `json:"sources"`
	Watermark uint64        `json:"watermark"`

	Dimensions       []Dimension       `json:"dimensions"`
	EvidenceRules    []EvidenceRule    `json:"evidence_rules"`
	AttributionRules []AttributionRule `json:"attribution_rules"`
	ConfidenceRules  []ConfidenceRule  `json:"confidence_rules"`

	ShadowCycles []ShadowCycle `json:"shadow_cycles,omitempty"`

	GovernanceChangeID string    `json:"governance_change_id,omitempty"`
	SupersedesID       string    `json:"supersedes_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// DerivedID returns the deterministic, provider-neutral id for this policy
// revision, derived only from (tenant, policy_key, revision).
func (p Policy) DerivedID() string {
	h := sha256.New()
	for _, s := range []string{p.Tenant, p.PolicyKey, fmt.Sprintf("%d", p.Revision)} {
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	return "ap_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// RuleDigest returns a deterministic digest of the policy's RULE SET only —
// dimensions and evidence/attribution/confidence rules, order-independent.
// Non-rule fields (status, shadow cycles, timestamps, governance link) never
// affect it, so a status transition keeps the digest stable while any rule edit
// changes it (the signal a shadow cycle binds and the gate compares against).
//
// Each list is sorted by its element's FULL marshaled tuple (not just
// Key/Dimension), so two byte-identical rule SETS in any order always digest
// identically — even if a dimension appears more than once (defense in depth;
// Policy.Validate already rejects duplicate rules per dimension per kind).
func (p Policy) RuleDigest() string {
	type ruleSet struct {
		Dimensions       []Dimension
		EvidenceRules    []EvidenceRule
		AttributionRules []AttributionRule
		ConfidenceRules  []ConfidenceRule
	}
	rs := ruleSet{
		Dimensions:       append([]Dimension(nil), p.Dimensions...),
		EvidenceRules:    append([]EvidenceRule(nil), p.EvidenceRules...),
		AttributionRules: append([]AttributionRule(nil), p.AttributionRules...),
		ConfidenceRules:  append([]ConfidenceRule(nil), p.ConfidenceRules...),
	}
	sortByMarshaled(rs.Dimensions)
	sortByMarshaled(rs.EvidenceRules)
	sortByMarshaled(rs.AttributionRules)
	sortByMarshaled(rs.ConfidenceRules)
	raw, _ := json.Marshal(rs)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// sortByMarshaled sorts items in place by each element's full JSON marshaling,
// giving an order-independent, content-addressed ordering for the rule digest.
// Each element carries its own marshaled bytes through the sort so the
// comparison key stays aligned with the value as the slice is permuted.
func sortByMarshaled[T any](items []T) {
	type keyed struct {
		raw []byte
		val T
	}
	pairs := make([]keyed, len(items))
	for i, v := range items {
		b, _ := json.Marshal(v)
		pairs[i] = keyed{raw: b, val: v}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return bytes.Compare(pairs[i].raw, pairs[j].raw) < 0
	})
	for i := range pairs {
		items[i] = pairs[i].val
	}
}

// FormalScoringActive reports whether the policy may back a FORMAL score. This
// is true ONLY when the policy is published: a draft/shadow/reviewing policy is
// a set of suggestions and can never be presented as a formal score.
func (p Policy) FormalScoringActive() bool { return p.Status == StatusPublished }

// ErrFormalScoringNotAllowed rejects an attempt to issue a formal score under a
// policy that is not published.
var ErrFormalScoringNotAllowed = errors.New("assessment: a formal score requires a published policy; draft/shadow/reviewing policies are non-formal suggestions")

// AssertFormalScoringAllowed returns nil only when the policy may back a formal
// score (published); otherwise ErrFormalScoringNotAllowed.
func (p Policy) AssertFormalScoringAllowed() error {
	if !p.FormalScoringActive() {
		return fmt.Errorf("%w (status %q)", ErrFormalScoringNotAllowed, p.Status)
	}
	return nil
}

// FormalDimensions returns the dimensions that carry formal-scoring intent.
func (p Policy) FormalDimensions() []Dimension {
	var out []Dimension
	for _, d := range p.Dimensions {
		if d.Formal {
			out = append(out, d)
		}
	}
	return out
}

// confidenceByDimension indexes the policy's confidence rules by dimension.
func (p Policy) confidenceByDimension() map[DimensionKey]ConfidenceLevel {
	m := map[DimensionKey]ConfidenceLevel{}
	for _, r := range p.ConfidenceRules {
		m[r.Dimension] = r.Level
	}
	return m
}

// Validate enforces the full Policy contract: identity, status, source binding,
// at least one dimension, valid rules referencing declared dimensions, the
// fairness guard on every dimension, and the opaque-content boundary.
func (p Policy) Validate() error {
	if p.Tenant == "" || utf8.RuneCountInString(p.Tenant) > 128 {
		return errors.New("assessment: tenant must contain 1..128 characters")
	}
	if p.PolicyKey == "" || utf8.RuneCountInString(p.PolicyKey) > 256 {
		return errors.New("assessment: policy_key must contain 1..256 characters")
	}
	if p.Revision < 1 {
		return errors.New("assessment: revision must be >= 1")
	}
	if !p.Status.Valid() {
		return fmt.Errorf("assessment: status %q is not valid", p.Status)
	}
	if err := p.Sources.Validate(); err != nil {
		return fmt.Errorf("sources: %w", err)
	}
	if len(p.Dimensions) == 0 {
		return errors.New("assessment: a policy must declare at least one dimension")
	}
	declared := map[DimensionKey]bool{}
	seen := map[DimensionKey]bool{}
	for i, d := range p.Dimensions {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("dimensions[%d]: %w", i, err)
		}
		if seen[d.Key] {
			return fmt.Errorf("assessment: dimension %q declared more than once", d.Key)
		}
		seen[d.Key] = true
		declared[d.Key] = true
	}
	checkRuleDim := func(kind string, dim DimensionKey) error {
		if !declared[dim] {
			return fmt.Errorf("assessment: %s rule references undeclared dimension %q", kind, dim)
		}
		return nil
	}
	// One rule per dimension per kind: defaultRules emits exactly one each and the
	// gate's confidenceByDimension assumes one confidence rule per dimension, so a
	// duplicate (which would silently last-win) is rejected here.
	oncePerKind := func(kind string, seen map[DimensionKey]bool, dim DimensionKey) error {
		if seen[dim] {
			return fmt.Errorf("assessment: %s rule for dimension %q declared more than once (one rule per dimension per kind)", kind, dim)
		}
		seen[dim] = true
		return nil
	}
	seenEvidence := map[DimensionKey]bool{}
	for i, r := range p.EvidenceRules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("evidence_rules[%d]: %w", i, err)
		}
		if err := checkRuleDim("evidence", r.Dimension); err != nil {
			return err
		}
		if err := oncePerKind("evidence", seenEvidence, r.Dimension); err != nil {
			return err
		}
	}
	seenAttribution := map[DimensionKey]bool{}
	for i, r := range p.AttributionRules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("attribution_rules[%d]: %w", i, err)
		}
		if err := checkRuleDim("attribution", r.Dimension); err != nil {
			return err
		}
		if err := oncePerKind("attribution", seenAttribution, r.Dimension); err != nil {
			return err
		}
	}
	seenConfidence := map[DimensionKey]bool{}
	for i, r := range p.ConfidenceRules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("confidence_rules[%d]: %w", i, err)
		}
		if err := checkRuleDim("confidence", r.Dimension); err != nil {
			return err
		}
		if err := oncePerKind("confidence", seenConfidence, r.Dimension); err != nil {
			return err
		}
	}
	for i, c := range p.ShadowCycles {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("shadow_cycles[%d]: %w", i, err)
		}
	}
	return NoRawContentLeak(p)
}

// --- publication gate (strict deterministic state machine) -----------------

// EvaluatePublicationGate applies the one-cycle publication gate to a completed
// shadow cycle against the policy revision it ran on, returning the ordered set
// of FAILING gate reasons. An empty result means every gate passed and the
// policy is publishable. The gate FAILS CLOSED: an incomplete/partial cycle, a
// cycle bound to a different revision or rule digest, a missing confidence rule
// for a formal dimension, or any missing confirmation yields a non-empty result
// (never publishes). This is the single deterministic source of truth for the
// shadow → reviewing transition.
func EvaluatePublicationGate(p Policy, c ShadowCycle) []GateReason {
	var reasons []GateReason

	// A cycle must cover one COMPLETE assessment period, be bound to THIS
	// revision, and carry the CURRENT rule digest — otherwise it proves nothing.
	if !c.IsComplete() || c.Revision != p.Revision {
		reasons = append(reasons, GateIncompleteCycle)
	}
	if c.RuleChangedAfterStart || c.RuleDigest != p.RuleDigest() {
		reasons = append(reasons, GateRuleChangedAfterStart)
	}
	if c.EvidenceCoverage < MinEvidenceCoverage {
		reasons = append(reasons, GateCoverageBelowThreshold)
	}
	// No FORMAL dimension may carry low confidence. A missing confidence rule for
	// a formal dimension is ambiguous and fails closed.
	conf := p.confidenceByDimension()
	for _, d := range p.FormalDimensions() {
		level, ok := conf[d.Key]
		if !ok || level == ConfidenceLow {
			reasons = append(reasons, GateLowConfidenceFormal)
			break
		}
	}
	if c.UnresolvedCorrections > 0 {
		reasons = append(reasons, GateUnresolvedCorrection)
	}
	if c.UnresolvedCalibrations > 0 {
		reasons = append(reasons, GateUnresolvedCalibration)
	}
	if !c.ManagerConfirmed {
		reasons = append(reasons, GateManagerNotConfirmed)
	}
	if !c.PolicyOwnerConfirmed {
		reasons = append(reasons, GatePolicyOwnerNotConfirmed)
	}
	return reasons
}
