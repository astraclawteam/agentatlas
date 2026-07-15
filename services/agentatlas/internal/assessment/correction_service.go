package assessment

// correction_service.go implements GUARD 3 of Task 18D: an AssessmentCorrection
// ADDS evidence or CHALLENGES a fact/attribution against a SPECIFIC immutable
// assessment version; it NEVER edits the old assessment in place, NEVER deletes a
// signed receipt and NEVER mutates a structured result. An ACCEPTED correction
// produces a NEW assessment VERSION through Task 18C deterministic re-evaluation
// (a re-assessment is a new version, never an in-place edit); the OLD version
// stays immutable AND traceable (the correction records the exact target id +
// digest). A REJECTED correction leaves the old version untouched and mints no
// version. The correction record itself is append-only: it is submitted once,
// resolved once, and then frozen.
//
// The service composes three ports it never bypasses: the append-only
// ResultStore (18C — the only writer of assessment versions, and it has no update
// path at all), the append-only CorrectionStore (this file, backed by
// db/migrations/000022_assessment_corrections.sql), and the deterministic
// Reevaluator (the 18C Evaluator — so an accepted correction can never fabricate
// a result: the re-assessment is verified deterministically or comes back
// not_assessable).

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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
)

// maxCorrectionSummary bounds the correction rationale (matches the SDK's
// permitted-summary bound).
const maxCorrectionSummary = 512

// Correction sentinel errors (ErrNotFound is shared with the policy/result
// stores). Callers compare with errors.Is.
var (
	// ErrCorrectionExists rejects re-submitting an identical correction (same
	// tenant, target, subject, kind, dimension and challenged handle).
	ErrCorrectionExists = errors.New("assessment: an identical correction already exists for this target")
	// ErrCorrectionResolved rejects re-deciding a correction that was already
	// accepted or rejected: a correction is resolved exactly once.
	ErrCorrectionResolved = errors.New("assessment: correction is already resolved (a decision is one-shot)")
	// ErrCorrectionImmutable rejects any change to a correction's immutable core
	// (its target, kind, dimension, added evidence or challenge) once submitted.
	ErrCorrectionImmutable = errors.New("assessment: a submitted correction's core is immutable; only its one-shot resolution may be recorded")
)

// --- closed vocabularies -----------------------------------------------------

// CorrectionKind is the closed set of what a correction may do: ADD evidence, or
// CHALLENGE a fact / an attribution. It may NEVER delete a receipt or mutate a
// result (there is deliberately no such kind).
type CorrectionKind string

const (
	CorrectionAddEvidence          CorrectionKind = "add_evidence"
	CorrectionChallengeFact        CorrectionKind = "challenge_fact"
	CorrectionChallengeAttribution CorrectionKind = "challenge_attribution"
)

// Valid reports whether k is a known correction kind.
func (k CorrectionKind) Valid() bool {
	switch k {
	case CorrectionAddEvidence, CorrectionChallengeFact, CorrectionChallengeAttribution:
		return true
	}
	return false
}

// CorrectionState is the closed lifecycle of a correction: it is submitted
// (pending) and then resolved exactly once (accepted or rejected).
type CorrectionState string

const (
	CorrectionPending  CorrectionState = "pending"
	CorrectionAccepted CorrectionState = "accepted"
	CorrectionRejected CorrectionState = "rejected"
)

// Valid reports whether s is a known state.
func (s CorrectionState) Valid() bool {
	return s == CorrectionPending || s == CorrectionAccepted || s == CorrectionRejected
}

// IsTerminal reports whether the correction has been resolved.
func (s CorrectionState) IsTerminal() bool {
	return s == CorrectionAccepted || s == CorrectionRejected
}

// --- correction record -------------------------------------------------------

// CorrectionResolution records the one-shot decision on a correction. On accept
// it links the NEW assessment version produced by re-evaluation (never an edit
// of the old one); on reject it records only the reason.
type CorrectionResolution struct {
	State           CorrectionState `json:"state"`
	DecidedBy       string          `json:"decided_by,omitempty"`
	DecidedAt       time.Time       `json:"decided_at,omitempty"`
	NewAssessmentID string          `json:"new_assessment_id,omitempty"`
	NewVersion      int             `json:"new_version,omitempty"`
	Reason          string          `json:"reason,omitempty"`
}

// AssessmentCorrectionCase is an employee's append-only correction against ONE
// immutable assessment version. It binds the exact target id + digest (so the
// old version stays traceable), the correction intent (add evidence or challenge
// a fact/attribution), and the one-shot resolution.
type AssessmentCorrectionCase struct {
	ID                 string               `json:"id"`
	Tenant             string               `json:"tenant"`
	Subject            string               `json:"subject"`
	TargetAssessmentID string               `json:"target_assessment_id"`
	TargetDigest       string               `json:"target_digest"`
	Kind               CorrectionKind       `json:"kind"`
	Dimension          model.DimensionKey   `json:"dimension"`
	AddedEvidence      []model.Evidence     `json:"added_evidence,omitempty"`
	ChallengedHandle   string               `json:"challenged_handle,omitempty"`
	Rationale          string               `json:"rationale"`
	SubmittedAt        time.Time            `json:"submitted_at"`
	Resolution         CorrectionResolution `json:"resolution"`
}

// DerivedID is the deterministic, provider-neutral id of a correction, derived
// from its immutable core so an identical re-submission dedupes (ErrCorrectionExists).
func (c AssessmentCorrectionCase) DerivedID() string {
	h := sha256.New()
	for _, p := range []string{c.Tenant, c.TargetAssessmentID, c.Subject, string(c.Kind), string(c.Dimension), c.ChallengedHandle} {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return "cor_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// Validate enforces the correction contract and the opaque-content boundary. An
// employee-ADDED evidence item must be a human_report — an employee submits a
// report the system RE-VERIFIES on re-evaluation; they cannot self-sign a
// verified outcome or an execution receipt.
func (c AssessmentCorrectionCase) Validate() error {
	if c.Tenant == "" || utf8.RuneCountInString(c.Tenant) > 128 {
		return errors.New("assessment: correction tenant must contain 1..128 characters")
	}
	if c.Subject == "" || utf8.RuneCountInString(c.Subject) > 256 {
		return errors.New("assessment: correction subject must contain 1..256 characters")
	}
	if c.TargetAssessmentID == "" || c.TargetDigest == "" {
		return errors.New("assessment: correction must bind the target assessment id and digest (traceability)")
	}
	if !c.Kind.Valid() {
		return fmt.Errorf("assessment: correction kind %q is not valid", c.Kind)
	}
	if c.Dimension == "" {
		return errors.New("assessment: correction must name the dimension it corrects")
	}
	if c.Rationale == "" || utf8.RuneCountInString(c.Rationale) > maxCorrectionSummary {
		return fmt.Errorf("assessment: correction rationale must contain 1..%d characters", maxCorrectionSummary)
	}
	switch c.Kind {
	case CorrectionAddEvidence:
		if len(c.AddedEvidence) == 0 {
			return errors.New("assessment: an add_evidence correction must carry at least one added evidence item")
		}
		if c.ChallengedHandle != "" {
			return errors.New("assessment: an add_evidence correction does not challenge a handle")
		}
		for i, e := range c.AddedEvidence {
			if e.Tier != model.TierHumanReport {
				return fmt.Errorf("assessment: employee-added evidence must be a human_report (re-verified on re-evaluation), got %q", e.Tier)
			}
			if err := e.Validate(); err != nil {
				return fmt.Errorf("added_evidence[%d]: %w", i, err)
			}
		}
	case CorrectionChallengeFact, CorrectionChallengeAttribution:
		if c.ChallengedHandle == "" || utf8.RuneCountInString(c.ChallengedHandle) > 256 {
			return errors.New("assessment: a challenge must name the opaque handle it challenges (1..256 characters)")
		}
		if len(c.AddedEvidence) != 0 {
			return errors.New("assessment: a challenge correction does not add evidence")
		}
	}
	if !c.Resolution.State.Valid() {
		return fmt.Errorf("assessment: correction resolution state %q is not valid", c.Resolution.State)
	}
	return model.NoRawContentLeak(c)
}

func cloneCorrection(c AssessmentCorrectionCase) AssessmentCorrectionCase {
	raw, _ := json.Marshal(c)
	var out AssessmentCorrectionCase
	_ = json.Unmarshal(raw, &out)
	return out
}

// correctionCore returns the immutable-core JSON of a correction (everything but
// its resolution) so the stores can prove a resolve changed ONLY the resolution.
func correctionCore(c AssessmentCorrectionCase) string {
	c.Resolution = CorrectionResolution{}
	raw, _ := json.Marshal(c)
	return string(raw)
}

// checkCorrectionResolve is the shared immutability guard both stores apply when
// an existing pending correction is resolved: the core is immutable, the state
// moves pending -> terminal exactly once.
func checkCorrectionResolve(old, updated AssessmentCorrectionCase) error {
	if old.Resolution.State.IsTerminal() {
		return ErrCorrectionResolved
	}
	if !updated.Resolution.State.IsTerminal() {
		return fmt.Errorf("%w: a resolve must move the correction to accepted or rejected", ErrInvalidState)
	}
	if correctionCore(old) != correctionCore(updated) {
		return ErrCorrectionImmutable
	}
	return nil
}

// --- CorrectionStore ---------------------------------------------------------

// CorrectionStore is the append-only persistence port for corrections. A
// correction is Appended once (pending) and Resolved once (terminal); it is
// never deleted, and its core is never edited.
type CorrectionStore interface {
	// Append validates c, assigns its deterministic id and inserts it pending. A
	// duplicate (tenant, id) is ErrCorrectionExists.
	Append(ctx context.Context, c AssessmentCorrectionCase) (AssessmentCorrectionCase, error)
	// Get returns the exact (tenant, id) or ErrNotFound.
	Get(ctx context.Context, tenant, id string) (AssessmentCorrectionCase, error)
	// Resolve records the one-shot resolution under the immutability guard. An
	// already-resolved correction is ErrCorrectionResolved; a missing one ErrNotFound.
	Resolve(ctx context.Context, c AssessmentCorrectionCase) (AssessmentCorrectionCase, error)
	// ListForAssessment returns every correction targeting an assessment id,
	// oldest-first (the audit trail for a version).
	ListForAssessment(ctx context.Context, tenant, assessmentID string) ([]AssessmentCorrectionCase, error)
}

// --- MemoryCorrectionStore ---------------------------------------------------

// MemoryCorrectionStore is an in-memory CorrectionStore enforcing the same
// append-only + one-shot-resolution immutability contract as
// PostgresCorrectionStore, so their behavior cannot drift.
type MemoryCorrectionStore struct {
	mu   sync.Mutex
	now  func() time.Time
	rows map[string]map[string]AssessmentCorrectionCase // tenant -> id -> case
}

// NewMemoryCorrectionStore constructs an empty store. now defaults to time.Now.
func NewMemoryCorrectionStore(now func() time.Time) *MemoryCorrectionStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryCorrectionStore{now: now, rows: map[string]map[string]AssessmentCorrectionCase{}}
}

func (s *MemoryCorrectionStore) Append(_ context.Context, c AssessmentCorrectionCase) (AssessmentCorrectionCase, error) {
	c.ID = c.DerivedID()
	if err := c.Validate(); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := s.rows[c.Tenant]
	if byID == nil {
		byID = map[string]AssessmentCorrectionCase{}
		s.rows[c.Tenant] = byID
	}
	if _, ok := byID[c.ID]; ok {
		return AssessmentCorrectionCase{}, ErrCorrectionExists
	}
	byID[c.ID] = cloneCorrection(c)
	return cloneCorrection(c), nil
}

func (s *MemoryCorrectionStore) Get(_ context.Context, tenant, id string) (AssessmentCorrectionCase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[tenant][id]
	if !ok {
		return AssessmentCorrectionCase{}, ErrNotFound
	}
	return cloneCorrection(c), nil
}

func (s *MemoryCorrectionStore) Resolve(_ context.Context, c AssessmentCorrectionCase) (AssessmentCorrectionCase, error) {
	c.ID = c.DerivedID()
	if err := c.Validate(); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.rows[c.Tenant][c.ID]
	if !ok {
		return AssessmentCorrectionCase{}, ErrNotFound
	}
	if err := checkCorrectionResolve(old, c); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	s.rows[c.Tenant][c.ID] = cloneCorrection(c)
	return cloneCorrection(c), nil
}

func (s *MemoryCorrectionStore) ListForAssessment(_ context.Context, tenant, assessmentID string) ([]AssessmentCorrectionCase, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AssessmentCorrectionCase
	for _, c := range s.rows[tenant] {
		if c.TargetAssessmentID == assessmentID {
			out = append(out, cloneCorrection(c))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SubmittedAt.Equal(out[j].SubmittedAt) {
			return out[i].SubmittedAt.Before(out[j].SubmittedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

var _ CorrectionStore = (*MemoryCorrectionStore)(nil)

// --- PostgresCorrectionStore -------------------------------------------------

// PostgresCorrectionStore is the authoritative CorrectionStore backed by
// db/migrations/000022_assessment_corrections.sql. It follows the same direct-pgx
// idiom as policy_store.go / result_store.go: tenant-scoped queries, promoted
// scalar columns for indexing/CHECKs plus JSONB for the opaque record, and the
// append-only + one-shot-resolution immutability enforced BOTH in Go (clean
// sentinels) and by the table's own triggers (the database backstop). No Apache
// AGE access.
type PostgresCorrectionStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPostgresCorrectionStore constructs a PostgresCorrectionStore over pool.
func NewPostgresCorrectionStore(pool *pgxpool.Pool, now func() time.Time) (*PostgresCorrectionStore, error) {
	if pool == nil {
		return nil, errors.New("assessment postgres correction store requires a pool")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresCorrectionStore{pool: pool, now: now}, nil
}

const selectCorrectionSQL = `
	SELECT content FROM assessment_corrections`

func (s *PostgresCorrectionStore) Append(ctx context.Context, c AssessmentCorrectionCase) (AssessmentCorrectionCase, error) {
	c.ID = c.DerivedID()
	if err := c.Validate(); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	content, _ := json.Marshal(c)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO assessment_corrections (
			tenant, id, subject, target_assessment_id, target_digest, kind, dimension,
			resolution_state, new_assessment_id, content, submitted_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		c.Tenant, c.ID, c.Subject, c.TargetAssessmentID, c.TargetDigest, string(c.Kind), string(c.Dimension),
		string(c.Resolution.State), nullString(c.Resolution.NewAssessmentID), content, c.SubmittedAt.UTC()); err != nil {
		if isUniqueViolation(err) {
			return AssessmentCorrectionCase{}, ErrCorrectionExists
		}
		return AssessmentCorrectionCase{}, err
	}
	return cloneCorrection(c), nil
}

func (s *PostgresCorrectionStore) Get(ctx context.Context, tenant, id string) (AssessmentCorrectionCase, error) {
	return scanCorrectionRow(s.pool.QueryRow(ctx, selectCorrectionSQL+` WHERE tenant=$1 AND id=$2`, tenant, id))
}

func (s *PostgresCorrectionStore) Resolve(ctx context.Context, c AssessmentCorrectionCase) (AssessmentCorrectionCase, error) {
	c.ID = c.DerivedID()
	if err := c.Validate(); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AssessmentCorrectionCase{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	old, err := scanCorrectionRow(tx.QueryRow(ctx, selectCorrectionSQL+` WHERE tenant=$1 AND id=$2 FOR UPDATE`, c.Tenant, c.ID))
	if errors.Is(err, ErrNotFound) {
		return AssessmentCorrectionCase{}, ErrNotFound
	}
	if err != nil {
		return AssessmentCorrectionCase{}, err
	}
	if err := checkCorrectionResolve(old, c); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	content, _ := json.Marshal(c)
	var decidedAt any
	if !c.Resolution.DecidedAt.IsZero() {
		decidedAt = c.Resolution.DecidedAt.UTC()
	}
	if _, err := tx.Exec(ctx, `
		UPDATE assessment_corrections
		SET resolution_state=$3, new_assessment_id=$4, decided_at=$5, content=$6
		WHERE tenant=$1 AND id=$2`,
		c.Tenant, c.ID, string(c.Resolution.State), nullString(c.Resolution.NewAssessmentID), decidedAt, content); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	return cloneCorrection(c), nil
}

func (s *PostgresCorrectionStore) ListForAssessment(ctx context.Context, tenant, assessmentID string) ([]AssessmentCorrectionCase, error) {
	rows, err := s.pool.Query(ctx, selectCorrectionSQL+` WHERE tenant=$1 AND target_assessment_id=$2 ORDER BY submitted_at, id`, tenant, assessmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssessmentCorrectionCase
	for rows.Next() {
		var content []byte
		if err := rows.Scan(&content); err != nil {
			return nil, err
		}
		var c AssessmentCorrectionCase
		if err := json.Unmarshal(content, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCorrectionRow(row interface{ Scan(...any) error }) (AssessmentCorrectionCase, error) {
	var content []byte
	if err := row.Scan(&content); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AssessmentCorrectionCase{}, ErrNotFound
		}
		return AssessmentCorrectionCase{}, err
	}
	var c AssessmentCorrectionCase
	if err := json.Unmarshal(content, &c); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	return c, nil
}

var _ CorrectionStore = (*PostgresCorrectionStore)(nil)

// --- Reevaluator port --------------------------------------------------------

// Reevaluator is the deterministic Task 18C evaluation service an accepted
// correction re-runs to produce a NEW assessment version. It is satisfied by
// *Evaluator; the correction service never fabricates a result itself.
type Reevaluator interface {
	Evaluate(ctx context.Context, req EvaluateRequest) (model.WorkAssessment, error)
}

var _ Reevaluator = (*Evaluator)(nil)

// --- CorrectionService -------------------------------------------------------

// CorrectionService orchestrates the correction workflow across the three ports.
// It never edits an assessment in place: Submit only appends a correction record,
// and Resolve(accept) appends a NEW assessment version via deterministic
// re-evaluation while leaving the old version immutable and traceable.
type CorrectionService struct {
	corrections CorrectionStore
	results     ResultStore
	evaluator   Reevaluator
	now         func() time.Time
}

// NewCorrectionService constructs a CorrectionService. now defaults to time.Now.
func NewCorrectionService(corrections CorrectionStore, results ResultStore, evaluator Reevaluator, now func() time.Time) *CorrectionService {
	if now == nil {
		now = time.Now
	}
	return &CorrectionService{corrections: corrections, results: results, evaluator: evaluator, now: now}
}

// SubmitCorrectionRequest is an employee's correction submission against their
// OWN assessment version.
type SubmitCorrectionRequest struct {
	Tenant             string
	EmployeeID         string
	TargetAssessmentID string
	Kind               CorrectionKind
	Dimension          model.DimensionKey
	AddedEvidence      []model.Evidence
	ChallengedHandle   string
	Rationale          string
}

// Submit records a pending correction against the target version. It is
// fail-closed: an employee may only correct their OWN assessment
// (ErrNotAssessmentSubject), and it never touches the target assessment.
func (s *CorrectionService) Submit(ctx context.Context, req SubmitCorrectionRequest) (AssessmentCorrectionCase, error) {
	if req.Tenant == "" || req.EmployeeID == "" || req.TargetAssessmentID == "" {
		return AssessmentCorrectionCase{}, errors.New("assessment: correction requires tenant, employee and target assessment id")
	}
	target, err := s.results.GetAssessment(ctx, req.Tenant, req.TargetAssessmentID)
	if err != nil {
		return AssessmentCorrectionCase{}, err // ErrNotFound propagates (cross-tenant reads too)
	}
	if target.Subject != req.EmployeeID {
		return AssessmentCorrectionCase{}, fmt.Errorf("%w: employee %q, subject %q", ErrNotAssessmentSubject, req.EmployeeID, target.Subject)
	}
	c := AssessmentCorrectionCase{
		Tenant:             req.Tenant,
		Subject:            target.Subject,
		TargetAssessmentID: target.ID,
		TargetDigest:       target.Digest(),
		Kind:               req.Kind,
		Dimension:          req.Dimension,
		AddedEvidence:      req.AddedEvidence,
		ChallengedHandle:   req.ChallengedHandle,
		Rationale:          req.Rationale,
		SubmittedAt:        s.now().UTC(),
		Resolution:         CorrectionResolution{State: CorrectionPending},
	}
	c.ID = c.DerivedID()
	if err := c.Validate(); err != nil {
		return AssessmentCorrectionCase{}, err
	}
	return s.corrections.Append(ctx, c)
}

// ResolveCorrectionRequest is a manager's/governed decision on a correction. On
// Accept, Reevaluation carries the corrected governed facts for a Task 18C
// re-assessment; its identity-defining fields are OVERRIDDEN from the target so a
// re-assessment is always a NEW version of the SAME assessment lineage.
type ResolveCorrectionRequest struct {
	Tenant       string
	CorrectionID string
	Accept       bool
	DecidedBy    string
	Reason       string
	Reevaluation EvaluateRequest
}

// Resolve records the one-shot decision. On reject, nothing else changes. On
// accept, it produces a NEW immutable assessment version via 18C deterministic
// re-evaluation, appends it (the old version is untouched), and links it into the
// resolution — returning the new assessment.
func (s *CorrectionService) Resolve(ctx context.Context, req ResolveCorrectionRequest) (AssessmentCorrectionCase, model.WorkAssessment, error) {
	c, err := s.corrections.Get(ctx, req.Tenant, req.CorrectionID)
	if err != nil {
		return AssessmentCorrectionCase{}, model.WorkAssessment{}, err
	}
	if c.Resolution.State.IsTerminal() {
		return c, model.WorkAssessment{}, ErrCorrectionResolved
	}
	now := s.now().UTC()

	if !req.Accept {
		c.Resolution = CorrectionResolution{State: CorrectionRejected, DecidedBy: req.DecidedBy, DecidedAt: now, Reason: req.Reason}
		resolved, err := s.corrections.Resolve(ctx, c)
		return resolved, model.WorkAssessment{}, err
	}

	// Accept: re-assess deterministically (18C) into a NEW version.
	target, err := s.results.GetAssessment(ctx, req.Tenant, c.TargetAssessmentID)
	if err != nil {
		return c, model.WorkAssessment{}, err
	}
	latest, err := s.results.LatestVersion(ctx, req.Tenant, target.Subject, target.PolicyKey, target.PolicyRevision)
	if err != nil {
		return c, model.WorkAssessment{}, err
	}

	reReq := req.Reevaluation
	if reReq.Policy.PolicyKey != target.PolicyKey || reReq.Policy.Revision != target.PolicyRevision {
		return c, model.WorkAssessment{}, fmt.Errorf("assessment: correction re-evaluation must use the corrected assessment's policy revision (got %q r%d, want %q r%d)",
			reReq.Policy.PolicyKey, reReq.Policy.Revision, target.PolicyKey, target.PolicyRevision)
	}
	// Bind the identity-defining fields to the target lineage; the manager may only
	// supply the corrected FACTS, never a different subject/period/version.
	reReq.Tenant = target.Tenant
	reReq.Org = target.Org
	reReq.Subject = target.Subject
	reReq.Level = target.Level
	reReq.Period = target.Period
	reReq.Version = latest.Version + 1

	wa2, err := s.evaluator.Evaluate(ctx, reReq)
	if err != nil {
		return c, model.WorkAssessment{}, err
	}
	if wa2.Subject != target.Subject || wa2.Version <= latest.Version {
		return c, model.WorkAssessment{}, errors.New("assessment: re-evaluation must produce a new higher version for the same subject")
	}
	stored2, err := s.results.AppendAssessment(ctx, wa2)
	if err != nil {
		return c, model.WorkAssessment{}, err
	}

	c.Resolution = CorrectionResolution{
		State: CorrectionAccepted, DecidedBy: req.DecidedBy, DecidedAt: now, Reason: req.Reason,
		NewAssessmentID: stored2.ID, NewVersion: stored2.Version,
	}
	resolved, err := s.corrections.Resolve(ctx, c)
	if err != nil {
		return c, model.WorkAssessment{}, err
	}
	return resolved, stored2, nil
}

// Get returns the employee's OWN correction (fail-closed on subject) so the
// Xiaozhi/ticket runtime can surface its outcome. A correction belonging to
// another employee is refused (ErrNotAssessmentSubject); a missing one is
// ErrNotFound.
func (s *CorrectionService) Get(ctx context.Context, tenant, employeeID, correctionID string) (AssessmentCorrectionCase, error) {
	c, err := s.corrections.Get(ctx, tenant, correctionID)
	if err != nil {
		return AssessmentCorrectionCase{}, err
	}
	if c.Subject != employeeID || employeeID == "" {
		return AssessmentCorrectionCase{}, fmt.Errorf("%w: employee %q, correction subject %q", ErrNotAssessmentSubject, employeeID, c.Subject)
	}
	return c, nil
}

// --- employee correction projection ------------------------------------------

// EmployeeCorrectionOutcome is the correction outcome an employee may see: the
// state, when it was decided, an opaque reference to the NEW assessment version
// (if accepted), and the decision reason. It deliberately omits the manager
// identity (DecidedBy) — the employee sees the outcome of their OWN correction,
// not who decided it.
type EmployeeCorrectionOutcome struct {
	State           CorrectionState `json:"state"`
	DecidedAt       time.Time       `json:"decided_at,omitempty"`
	NewAssessmentID string          `json:"new_assessment_id,omitempty"`
	NewVersion      int             `json:"new_version,omitempty"`
	Reason          string          `json:"reason,omitempty"`
}

// EmployeeCorrectionView is the fail-closed projection of an employee's own
// correction submission + outcome for the Xiaozhi/ticket runtime.
type EmployeeCorrectionView struct {
	ID                 string                    `json:"id"`
	TargetAssessmentID string                    `json:"target_assessment_id"`
	Kind               CorrectionKind            `json:"kind"`
	Dimension          model.DimensionKey        `json:"dimension"`
	ChallengedHandle   string                    `json:"challenged_handle,omitempty"`
	AddedEvidence      []EmployeeEvidenceRef     `json:"added_evidence,omitempty"`
	Rationale          string                    `json:"rationale"`
	SubmittedAt        time.Time                 `json:"submitted_at"`
	Outcome            EmployeeCorrectionOutcome `json:"outcome"`
}

// ProjectCorrectionForEmployee projects a correction case to the employee view,
// stripping the manager identity while keeping the correction outcome.
func ProjectCorrectionForEmployee(c AssessmentCorrectionCase) EmployeeCorrectionView {
	view := EmployeeCorrectionView{
		ID:                 c.ID,
		TargetAssessmentID: c.TargetAssessmentID,
		Kind:               c.Kind,
		Dimension:          c.Dimension,
		ChallengedHandle:   c.ChallengedHandle,
		Rationale:          c.Rationale,
		SubmittedAt:        c.SubmittedAt,
		Outcome: EmployeeCorrectionOutcome{
			State:           c.Resolution.State,
			DecidedAt:       c.Resolution.DecidedAt,
			NewAssessmentID: c.Resolution.NewAssessmentID,
			NewVersion:      c.Resolution.NewVersion,
			Reason:          c.Resolution.Reason,
		},
	}
	for _, e := range c.AddedEvidence {
		view.AddedEvidence = append(view.AddedEvidence, EmployeeEvidenceRef{Handle: e.Handle, Kind: e.Kind, Tier: e.Tier, Summary: e.Summary})
	}
	return view
}
