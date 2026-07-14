// Package outcomelearning distills GOVERNED CANDIDATES from the provable
// Outcome lineage (Tasks 0G/0H) as projected by the Task 0I Outcome Graph. It
// is the result-driven method/Operating-Map learning layer of the AgentAtlas
// GA spine, and it is deliberately fail-closed:
//
//   - It reads the graph ONLY through the closed, typed, tenant/org-scoped
//     0I query surface (internal/outcomegraph.QueryService). There is no
//     arbitrary-Cypher or direct-AGE path anywhere in this package.
//   - A distilled candidate is IMMUTABLE and binds exactly the evidence it was
//     derived from: the projection watermark, the source Outcome/WorkCase/
//     receipt set, the generation policy + model versions, the evidence
//     coverage, a deterministic REPLAY against immutable historical Outcome
//     versions, and a bounded SHADOW comparison against the status quo.
//   - A candidate NEVER becomes published knowledge, method, Operating Map,
//     risk or assessment policy on its own. An ACCEPTED candidate is handed to
//     the EXISTING governance draft/review path (internal/governance
//     Service.Suggest) as a suggestion-only draft that only a human/governed
//     review can adopt. This package holds NO publish capability at all (its
//     GovernanceDrafts port exposes only Suggest), so automatic publication is
//     structurally impossible.
//   - The model is a PROPOSER, never a decider. The Distiller port may propose
//     a human-readable summary/change; every load-bearing fact (sources,
//     watermark, coverage, replay, shadow, disposition) is computed
//     deterministically from immutable data, and any prompt-injection in
//     evidence text is scrubbed and cannot alter the verdict or publish.
//
// Incomplete, contradictory, low-confidence, stale, revoked, replay-mismatched
// or shadow-regressed candidates are QUARANTINED (recorded, never emitted as
// adoptable), not handed to governance.
package outcomelearning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	govmodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	outcomemodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

// maxSummary bounds a candidate's permitted human summary (matches the
// sdk/go/outcome node-summary bound).
const maxSummary = 512

// redacted replaces any distiller-proposed text that matches a forbidden
// connector/credential/raw-query shape, so adversarial evidence can never leak
// into a candidate or a governance draft.
const redacted = "[redacted: proposed text contained forbidden connector/credential/raw-query content]"

// --- candidate kinds -------------------------------------------------------

// CandidateKind is the closed set of governed candidate kinds this distiller
// produces. Each maps 1:1 onto the plan's named candidate types:
//
//	KindMethod                  -> MethodCandidate
//	KindOperatingMapChange      -> OperatingMapChangeCandidate
//	KindSOPChange               -> SOPChangeCandidate
//	KindRiskRule                -> RiskRuleCandidate
//	KindConnectorCapabilityGap  -> ConnectorCapabilityGap
//	KindDataAuthorityConflict   -> DataAuthorityConflict
//	KindRecurringBlockerPattern -> RecurringBlockerPattern
type CandidateKind string

const (
	KindMethod                  CandidateKind = "method"
	KindOperatingMapChange      CandidateKind = "operating_map_change"
	KindSOPChange               CandidateKind = "sop_change"
	KindRiskRule                CandidateKind = "risk_rule"
	KindConnectorCapabilityGap  CandidateKind = "connector_capability_gap"
	KindDataAuthorityConflict   CandidateKind = "data_authority_conflict"
	KindRecurringBlockerPattern CandidateKind = "recurring_blocker_pattern"
)

// Kinds returns the closed set of candidate kinds in a stable order.
func Kinds() []CandidateKind {
	return []CandidateKind{
		KindMethod, KindOperatingMapChange, KindSOPChange, KindRiskRule,
		KindConnectorCapabilityGap, KindDataAuthorityConflict, KindRecurringBlockerPattern,
	}
}

// Valid reports whether k is a known candidate kind.
func (k CandidateKind) Valid() bool {
	switch k {
	case KindMethod, KindOperatingMapChange, KindSOPChange, KindRiskRule,
		KindConnectorCapabilityGap, KindDataAuthorityConflict, KindRecurringBlockerPattern:
		return true
	}
	return false
}

// strategy selects how a kind is distilled from the graph: goal-anchored
// (effective-method style) or blocker/impact-anchored (recurring-exception
// style).
type strategy int

const (
	strategyGoal strategy = iota
	strategyBlocker
)

// strategy returns the distillation strategy for a kind.
func (k CandidateKind) strategy() strategy {
	switch k {
	case KindConnectorCapabilityGap, KindDataAuthorityConflict, KindRecurringBlockerPattern:
		return strategyBlocker
	default:
		return strategyGoal
	}
}

// governanceTarget maps a candidate kind onto the EXISTING governance resource
// type and a NON-PUBLISH action. Every mapping is create/update only: an
// employee-suggestion draft can never carry the publish action (enforced again
// by govmodel.ChangeDraft.Validate), so the handoff can only ever produce a
// draft for human review.
func (k CandidateKind) governanceTarget() (govmodel.ResourceType, govmodel.Action) {
	switch k {
	case KindMethod:
		return govmodel.ResourceMethodOutline, govmodel.ActionUpdate
	case KindSOPChange:
		return govmodel.ResourceSOP, govmodel.ActionUpdate
	case KindOperatingMapChange:
		return govmodel.ResourceKnowledgeEntry, govmodel.ActionUpdate
	case KindRiskRule:
		return govmodel.ResourceKnowledgeEntry, govmodel.ActionUpdate
	default: // connector gap, data-authority conflict, recurring blocker pattern
		return govmodel.ResourceKnowledgeEntry, govmodel.ActionCreate
	}
}

// --- source references -----------------------------------------------------

// SourceKind classifies an opaque candidate source reference.
type SourceKind string

const (
	SourceOutcome     SourceKind = "outcome"
	SourceWorkCase    SourceKind = "work_case"
	SourceReceipt     SourceKind = "receipt"
	SourceBlocker     SourceKind = "blocker"
	SourceContributor SourceKind = "contributor"
	SourceEvidence    SourceKind = "evidence"
)

// SourceRef is an opaque, versioned reference to one distillation source. It
// carries no raw content — only a kind, a business id and a revision.
type SourceRef struct {
	Kind       SourceKind `json:"kind"`
	BusinessID string     `json:"business_id"`
	Revision   uint64     `json:"revision"`
}

// AnchorRef records the closed-vocabulary graph anchor a candidate was
// distilled from (a Goal for goal-anchored kinds, a Blocker for impact-anchored
// kinds), so a re-evaluation can re-run the same typed query. It is stored as
// plain strings so this file stays free of the outcomegraph import.
type AnchorRef struct {
	Label      string `json:"label"`
	BusinessID string `json:"business_id"`
	Revision   uint64 `json:"revision"`
}

func (r SourceRef) key() string {
	return string(r.Kind) + "\x00" + r.BusinessID + "\x00" + fmt.Sprintf("%d", r.Revision)
}

// --- coverage --------------------------------------------------------------

// Coverage records the evidence coverage of a candidate's source set: how many
// sources carry admissible evidence out of the total, and the derived score.
type Coverage struct {
	Covered int     `json:"covered"`
	Total   int     `json:"total"`
	Score   float64 `json:"score"`
}

func newCoverage(covered, total int) Coverage {
	c := Coverage{Covered: covered, Total: total}
	if total > 0 {
		c.Score = float64(covered) / float64(total)
	}
	return c
}

// --- disposition -----------------------------------------------------------

// Disposition is the terminal lifecycle state of a distilled candidate.
type Disposition string

const (
	// DispositionAccepted candidates were evidence-grounded, replayed and
	// shadow-checked successfully and have been handed to the governance draft
	// path for human review. Accepted is NOT adopted: only a governed review
	// can adopt.
	DispositionAccepted Disposition = "accepted"
	// DispositionQuarantined candidates failed a grounding/replay/shadow check
	// and are recorded but never handed to governance.
	DispositionQuarantined Disposition = "quarantined"
)

// QuarantineReason names why a candidate was quarantined. Each maps to exactly
// one fail-closed check.
type QuarantineReason string

const (
	ReasonNone                 QuarantineReason = ""
	ReasonInsufficientEvidence QuarantineReason = "insufficient_evidence_coverage"
	ReasonContradictory        QuarantineReason = "contradictory_outcomes"
	ReasonStaleWatermark       QuarantineReason = "stale_projection_watermark"
	ReasonRevokedSource        QuarantineReason = "revoked_source"
	ReasonReplayMismatch       QuarantineReason = "replay_mismatch"
	ReasonShadowRegression     QuarantineReason = "shadow_regression"
	ReasonLowConfidence        QuarantineReason = "low_confidence"
)

// --- candidate -------------------------------------------------------------

// Candidate is one immutable, governed learning candidate. It binds exactly the
// evidence it was distilled from and its deterministic verdict. It is never
// mutated in place: a re-evaluation produces a NEW candidate that supersedes
// this one (SupersedesID), mirroring the append-only Outcome discipline.
type Candidate struct {
	ID     string        `json:"id"`
	Tenant string        `json:"tenant"`
	Org    string        `json:"org,omitempty"`
	Kind   CandidateKind `json:"kind"`

	// Summary is the bounded, scrubbed human summary (a proposer hint only).
	Summary string `json:"summary,omitempty"`
	// Proposal is the scrubbed governance change payload handed to review.
	Proposal ProposedChange `json:"proposal"`

	// --- immutable evidence binding ---
	Watermark        uint64       `json:"watermark"`
	Anchor           AnchorRef    `json:"anchor"`
	Sources          []SourceRef  `json:"sources"`
	GenerationPolicy string       `json:"generation_policy"`
	ModelVersion     string       `json:"model_version"`
	Coverage         Coverage     `json:"coverage"`
	Replay           ReplayResult `json:"replay"`
	Shadow           ShadowResult `json:"shadow"`

	// --- verdict ---
	Disposition        Disposition      `json:"disposition"`
	QuarantineReason   QuarantineReason `json:"quarantine_reason,omitempty"`
	GovernanceChangeID string           `json:"governance_change_id,omitempty"`
	SupersedesID       string           `json:"supersedes_id,omitempty"`
	CreatedAt          time.Time        `json:"created_at"`
}

// deriveID computes the deterministic, provider-neutral candidate id from the
// immutable evidence a candidate is distilled from: tenant, org, kind,
// watermark, the sorted source set, the generation policy + model versions, the
// scrubbed proposal/summary/anchor AND the replay + shadow fingerprints. The
// replay/shadow digests deterministically capture the observed authoritative
// state of every source (status, membership, currentness), so re-distilling
// UNCHANGED evidence yields the SAME id (idempotent), while a source that has
// since been revoked, superseded or re-decided yields a DIFFERENT id — a fresh
// re-evaluation record that supersedes the prior candidate rather than silently
// colliding with it. The disposition itself is never part of the id.
func (c Candidate) deriveID() string {
	srcKeys := make([]string, 0, len(c.Sources))
	for _, s := range c.Sources {
		srcKeys = append(srcKeys, s.key())
	}
	sort.Strings(srcKeys)
	h := sha256.New()
	proposal, _ := json.Marshal(c.Proposal)
	anchor, _ := json.Marshal(c.Anchor)
	parts := append([]string{
		c.Tenant, c.Org, string(c.Kind), fmt.Sprintf("%d", c.Watermark),
		c.GenerationPolicy, c.ModelVersion, c.Summary, string(proposal), string(anchor),
		c.Replay.Digest, c.Shadow.Digest,
	}, srcKeys...)
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "olc_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// ErrRawContentLeak rejects a candidate whose scrubbed form still matches a
// forbidden connector/credential/raw-query shape (defense in depth over the
// scrub step).
var ErrRawContentLeak = errors.New("outcomelearning: candidate content matches a forbidden connector/credential/raw-query shape")

// Validate enforces the immutable candidate contract and the opaque-content
// boundary. A candidate carries opaque handles/hashes and a bounded permitted
// summary only — never raw enterprise content, a connector endpoint or a
// credential.
func (c Candidate) Validate() error {
	if c.Tenant == "" {
		return errors.New("outcomelearning: candidate tenant is required")
	}
	if !c.Kind.Valid() {
		return fmt.Errorf("outcomelearning: candidate kind %q is not valid", c.Kind)
	}
	if utf8.RuneCountInString(c.Summary) > maxSummary {
		return fmt.Errorf("outcomelearning: candidate summary must be a bounded permitted summary (<=%d chars)", maxSummary)
	}
	if c.Watermark == 0 {
		return errors.New("outcomelearning: candidate must bind the exact projection watermark it was distilled at")
	}
	if len(c.Sources) == 0 {
		return errors.New("outcomelearning: candidate must bind at least one source reference")
	}
	if c.GenerationPolicy == "" || c.ModelVersion == "" {
		return errors.New("outcomelearning: candidate must bind the generation policy and model versions")
	}
	switch c.Disposition {
	case DispositionAccepted:
		if c.QuarantineReason != ReasonNone {
			return errors.New("outcomelearning: an accepted candidate must not carry a quarantine reason")
		}
	case DispositionQuarantined:
		if c.QuarantineReason == ReasonNone {
			return errors.New("outcomelearning: a quarantined candidate must name its reason")
		}
		if c.GovernanceChangeID != "" {
			return errors.New("outcomelearning: a quarantined candidate must never reach governance")
		}
	default:
		return fmt.Errorf("outcomelearning: unknown disposition %q", c.Disposition)
	}
	// Opaque-content guard: reuse the exact sdk/go/outcome scanner so a
	// connector endpoint, credential or raw query can never enter a candidate.
	if err := outcomemodel.NoRawContentLeak(c); err != nil {
		return fmt.Errorf("%w: %v", ErrRawContentLeak, err)
	}
	return nil
}

// IsAdoptable reports whether the candidate was accepted (handed to governance
// for review). It is never "adopted" here — only a governed review can adopt.
func (c Candidate) IsAdoptable() bool { return c.Disposition == DispositionAccepted }

// --- content scrubbing -----------------------------------------------------

// scrubSummary bounds a proposer summary and redacts it entirely if it matches
// a forbidden connector/credential/raw-query shape.
func scrubSummary(s string) string {
	if outcomemodel.NoRawContentLeak(s) != nil {
		return redacted
	}
	if utf8.RuneCountInString(s) > maxSummary {
		r := []rune(s)
		return string(r[:maxSummary])
	}
	return s
}

// scrubContent redacts a proposer JSON change payload if it matches a forbidden
// shape (or is not valid JSON), returning a safe placeholder document so the
// governance draft never carries leaked content.
func scrubContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) || outcomemodel.NoRawContentLeak(raw) != nil {
		return json.RawMessage(`{"note":"` + redacted + `"}`)
	}
	return append(json.RawMessage(nil), raw...)
}

// --- candidate store -------------------------------------------------------

// ErrCandidateExists is returned when an identical (same deterministic id)
// candidate is appended twice; the store is append-only and idempotent.
var ErrCandidateExists = errors.New("outcomelearning: candidate already exists (history is append-only)")

// CandidateStore persists immutable candidates append-only. The authoritative
// implementation is Postgres (migration 000018); MemoryCandidateStore mirrors
// the same contract for unit tests.
type CandidateStore interface {
	// Append stores an immutable candidate. Re-appending the same id with the
	// same content is idempotent; a conflicting re-append is rejected.
	Append(ctx context.Context, c Candidate) error
	// Get returns the (tenant, id) candidate, if present.
	Get(ctx context.Context, tenant, id string) (Candidate, bool, error)
}

// MemoryCandidateStore is an in-memory, append-only CandidateStore for unit
// tests. It enforces the same immutability contract as the Postgres store:
// once appended, a candidate id is never mutated in place.
type MemoryCandidateStore struct {
	mu   sync.Mutex
	rows map[string]Candidate // tenant\x00id -> candidate
}

// NewMemoryCandidateStore constructs an empty in-memory candidate store.
func NewMemoryCandidateStore() *MemoryCandidateStore {
	return &MemoryCandidateStore{rows: map[string]Candidate{}}
}

func candidateKey(tenant, id string) string { return tenant + "\x00" + id }

// Append stores an immutable candidate (idempotent on identical content).
func (s *MemoryCandidateStore) Append(_ context.Context, c Candidate) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := candidateKey(c.Tenant, c.ID)
	if existing, ok := s.rows[k]; ok {
		if existing.ID == c.ID && existing.Disposition == c.Disposition && existing.GovernanceChangeID == c.GovernanceChangeID {
			return nil // idempotent re-append
		}
		return ErrCandidateExists
	}
	s.rows[k] = c
	return nil
}

// Get returns the (tenant, id) candidate.
func (s *MemoryCandidateStore) Get(_ context.Context, tenant, id string) (Candidate, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[candidateKey(tenant, id)]
	return c, ok, nil
}

// Len reports how many candidates are stored (test helper).
func (s *MemoryCandidateStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

var _ CandidateStore = (*MemoryCandidateStore)(nil)
