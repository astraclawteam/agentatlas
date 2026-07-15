package assessment

// visibility.go enforces the THREE fail-closed disclosure guards of Task 18D on
// top of the frozen Task 18C WorkAssessment:
//
//  1. EMPLOYEE VISIBILITY B (fail-closed): an employee, reading through the
//     Xiaozhi/ticket runtime, sees ONLY their OWN goals, WorkCase facts +
//     completion (opaque counted-outcome references), evidence REFERENCES,
//     blocker attribution and missing data — plus the ability to submit a
//     correction and read its outcome (correction_service.go). The projection
//     STRUCTURALLY omits the final score (SatisfiedOutcomes), the level
//     (ConfidenceExplanation), contributor attribution, the manager notes
//     (Narrative + ManagerConfirmation) and every other employee's
//     sub-assessment, and it REFUSES a read whose subject is not the
//     authenticated employee.
//
//  2. MANAGER SCOPE (fail-closed): the management BFF exposes the score/level and
//     manager notes ONLY after EXACT authorization — the reader is strictly
//     UPWARD of the subject in a CURRENT org snapshot (computed by the SAME
//     Task 0E governance hierarchy primitive used platform-wide, never an
//     invented org model) AND owns the governing policy. A peer, a subordinate,
//     an unrelated branch, a stale org snapshot or an unowned policy is refused.
//
//  4. HR-ACTION ISOLATION: there is NO code path from an assessment to an HR
//     consequence. DeriveHRAction ALWAYS refuses; every projection is
//     HR-action-free. A bonus/promotion/demotion/transfer/discipline/
//     termination/negative record requires a SEPARATE HR WorkCase with its own
//     policy and approval.
//
// (Guard 3 — correction never mutates the old assessment — lives in
// correction_service.go.) The projections carry only opaque handles, versioned
// keys, bounded permitted summaries and closed enums — never raw enterprise
// content (they are strict subsets of a WorkAssessment the SDK already screened
// with NoRawContentLeak).

import (
	"errors"
	"fmt"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	govmodel "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

// --- GUARD 1: employee visibility B ------------------------------------------

// ErrNotAssessmentSubject refuses an employee's read of an assessment whose
// subject is not the authenticated employee. Visibility B is fail-closed: an
// employee can only ever see their OWN assessment (and correct their OWN facts).
var ErrNotAssessmentSubject = errors.New("assessment: employee visibility is limited to the employee's own assessment (subject mismatch)")

// EmployeeGoals are the employee's own goals the projection surfaces: the
// versioned job responsibility and organization goal the assessment was bound to.
type EmployeeGoals struct {
	JobResponsibility model.VersionedRef `json:"job_responsibility"`
	OrgGoal           model.VersionedRef `json:"org_goal"`
}

// EmployeeEvidenceRef is an opaque evidence REFERENCE an employee may see: the
// handle, its business kind, the evidence tier and a bounded permitted summary.
// It never carries the raw content, the signing authority or the signature key —
// only enough for the employee to recognize and, if needed, challenge it.
type EmployeeEvidenceRef struct {
	Handle  string             `json:"handle"`
	Kind    string             `json:"kind"`
	Tier    model.EvidenceTier `json:"tier"`
	Summary string             `json:"summary,omitempty"`
}

// EmployeeDimensionView is the per-dimension slice an employee sees: the
// dimension, the opaque outcomes counted toward it (completion facts), the
// missing-data reason when it could not be assessed, the evidence references and
// the blocker attribution. It deliberately carries NO satisfied-outcome score,
// NO confidence level and NO contributor attribution.
type EmployeeDimensionView struct {
	Dimension            model.DimensionKey        `json:"dimension"`
	CompletedOutcomeRefs []string                  `json:"completed_outcome_refs,omitempty"`
	MissingData          model.NotAssessableReason `json:"missing_data,omitempty"`
	Evidence             []EmployeeEvidenceRef     `json:"evidence,omitempty"`
	Blockers             []model.Blocker           `json:"blockers,omitempty"`
}

// EmployeeAssessmentView is the fail-closed projection an employee sees through
// the Xiaozhi/ticket runtime. It is a strict subset of a WorkAssessment: goals,
// period, and per-dimension facts/completion/evidence-references/blockers/
// missing-data only — never the final score, the level, the manager notes or any
// other employee's sub-assessment.
type EmployeeAssessmentView struct {
	Subject    string                  `json:"subject"`
	Period     model.Period            `json:"period"`
	Goals      EmployeeGoals           `json:"goals"`
	Dimensions []EmployeeDimensionView `json:"dimensions"`
}

// ProjectForEmployee returns the fail-closed employee projection of wa for the
// authenticated employee. It REFUSES (ErrNotAssessmentSubject) when the employee
// is empty or is not the assessment subject, and it structurally omits every
// score/level and manager-notes field and every sub-assessment.
func ProjectForEmployee(wa model.WorkAssessment, employeeID string) (EmployeeAssessmentView, error) {
	if employeeID == "" || wa.Subject != employeeID {
		return EmployeeAssessmentView{}, fmt.Errorf("%w: authenticated %q, subject %q", ErrNotAssessmentSubject, employeeID, wa.Subject)
	}
	view := EmployeeAssessmentView{
		Subject: wa.Subject,
		Period:  wa.Period,
		Goals: EmployeeGoals{
			JobResponsibility: wa.Sources.JobResponsibility,
			OrgGoal:           wa.Sources.OrgGoal,
		},
	}
	for _, d := range wa.Dimensions {
		ev := EmployeeDimensionView{Dimension: d.Dimension}
		// Completion facts: the opaque outcomes counted toward this dimension. The
		// satisfied-outcome SCORE is deliberately dropped.
		ev.CompletedOutcomeRefs = append(ev.CompletedOutcomeRefs, d.CountedOutcomeKeys...)
		if d.State == model.StateNotAssessable {
			ev.MissingData = d.NotAssessableReason
		}
		for _, e := range d.Evidence {
			ev.Evidence = append(ev.Evidence, EmployeeEvidenceRef{
				Handle: e.Handle, Kind: e.Kind, Tier: e.Tier, Summary: e.Summary,
			})
		}
		ev.Blockers = append(ev.Blockers, d.Blockers...)
		view.Dimensions = append(view.Dimensions, ev)
	}
	return view, nil
}

// --- GUARD 2: manager scope --------------------------------------------------

// Manager-scope sentinel errors. Callers compare with errors.Is.
var (
	// ErrManagerNotAuthorized refuses a manager read that is outside the subject's
	// org hierarchy scope (a peer, a subordinate, an unrelated branch or a stale
	// org snapshot).
	ErrManagerNotAuthorized = errors.New("assessment: manager is not authorized to read this subject's assessment (outside the subject's reporting line)")
	// ErrPolicyNotOwned refuses a manager read of an assessment governed by a
	// policy the manager does not own (no scope broadening across policies).
	ErrPolicyNotOwned = errors.New("assessment: manager does not own the governing assessment policy")
	// ErrSubjectPartyMismatch refuses a request whose supplied subject org-party
	// is not the assessment's subject.
	ErrSubjectPartyMismatch = errors.New("assessment: authorization subject party does not match the assessment subject")
)

// ManagerScopeRequest carries everything needed to authorize (and then project) a
// manager's read of a subject's assessment. Manager and Subject are expressed as
// the SAME governance.Party org model (OrgUnitID + strict-ancestor OrgPath +
// OrgVersion) used platform-wide, so the hierarchy check reuses the real Task 0E
// primitive and can never broaden scope. OwnedPolicyKeys are the policy keys the
// manager owns; production resolves these from the manager's governed policy
// scope, tests supply them directly (mirroring how the shadow-cycle gate takes
// PolicyOwnerConfirmed as an input rather than a stored field).
type ManagerScopeRequest struct {
	Assessment        model.WorkAssessment
	Manager           governance.Party
	Subject           governance.Party
	CurrentOrgVersion int64
	OwnedPolicyKeys   []string
}

// ManagerAssessmentView is the management-BFF projection: the FULL structured
// assessment INCLUDING the score/level (SatisfiedOutcomes + confidence) and the
// manager notes (narrative + manager confirmation). It is only ever returned by
// ProjectForManager, which first enforces the exact hierarchy + policy
// authorization, so there is no code path to the score without authorization.
type ManagerAssessmentView struct {
	Assessment model.WorkAssessment `json:"assessment"`
}

// AuthorizeManagerRead returns nil ONLY when the manager may read the subject's
// assessment: the subject party is the assessment's subject, the manager owns the
// governing policy, AND the manager is a distinct, current-org, strictly-UPWARD
// party relative to the subject (the real reporting-line predicate). It is
// fail-closed: any failure yields a distinct sentinel and never the score.
func AuthorizeManagerRead(req ManagerScopeRequest) error {
	if req.Subject.UserID == "" || req.Subject.UserID != req.Assessment.Subject {
		return fmt.Errorf("%w: party %q vs assessment subject %q", ErrSubjectPartyMismatch, req.Subject.UserID, req.Assessment.Subject)
	}
	if !ownsPolicy(req.OwnedPolicyKeys, req.Assessment.PolicyKey) {
		return fmt.Errorf("%w: policy %q", ErrPolicyNotOwned, req.Assessment.PolicyKey)
	}
	// The hierarchy check is delegated to the SAME governance upward-review
	// primitive the platform uses (Task 0E): the manager must be exactly ONE
	// distinct, current-org, strictly-upward party over the subject. A peer,
	// subordinate, unrelated branch or stale snapshot fails closed there.
	policy := governance.UpwardReviewPolicy{CurrentOrgVersion: req.CurrentOrgVersion}
	if err := policy.Evaluate(governance.ApprovalParties{
		Submitter: req.Subject,
		Reviewers: []governance.Party{req.Manager},
	}, govmodel.RiskLow); err != nil {
		return fmt.Errorf("%w: %v", ErrManagerNotAuthorized, err)
	}
	return nil
}

// ProjectForManager authorizes the manager (AuthorizeManagerRead) and, only on
// success, returns the full manager detail. There is deliberately no way to
// obtain the score/level or manager notes without passing this authorization.
func ProjectForManager(req ManagerScopeRequest) (ManagerAssessmentView, error) {
	if err := AuthorizeManagerRead(req); err != nil {
		return ManagerAssessmentView{}, err
	}
	return ManagerAssessmentView{Assessment: cloneAssessment(req.Assessment)}, nil
}

func ownsPolicy(owned []string, policyKey string) bool {
	for _, k := range owned {
		if k == policyKey {
			return true
		}
	}
	return false
}

// --- GUARD 4: HR-action isolation --------------------------------------------

// HRActionKind is the closed set of HR consequences an assessment must NEVER
// directly trigger. Each requires a SEPARATE HR WorkCase with its own policy and
// approval.
type HRActionKind string

const (
	HRBonus          HRActionKind = "bonus"
	HRPromotion      HRActionKind = "promotion"
	HRDemotion       HRActionKind = "demotion"
	HRTransfer       HRActionKind = "transfer"
	HRDiscipline     HRActionKind = "discipline"
	HRTermination    HRActionKind = "termination"
	HRNegativeRecord HRActionKind = "negative_record"
)

// HRActionKinds returns the closed set of forbidden direct HR consequences in a
// stable order.
func HRActionKinds() []HRActionKind {
	return []HRActionKind{HRBonus, HRPromotion, HRDemotion, HRTransfer, HRDiscipline, HRTermination, HRNegativeRecord}
}

// ErrHRActionRequiresSeparateWorkCase refuses ANY attempt to turn an assessment
// into an HR action. "Assessment never directly triggers HR consequences."
var ErrHRActionRequiresSeparateWorkCase = errors.New("assessment: an assessment never directly triggers an HR consequence; a bonus/promotion/demotion/transfer/discipline/termination/negative record requires a SEPARATE HR WorkCase with its own policy and approval")

// DeriveHRAction ALWAYS refuses. There is deliberately no path from an assessment
// to an HR action through this package; this function exists only so the
// isolation guarantee is explicit and test-provable (TestHRIsolation), not to
// enable a hidden capability.
func DeriveHRAction(wa model.WorkAssessment, kind HRActionKind) error {
	return fmt.Errorf("%w (kind %q, subject %q, assessment %q)", ErrHRActionRequiresSeparateWorkCase, kind, wa.Subject, wa.ID)
}
