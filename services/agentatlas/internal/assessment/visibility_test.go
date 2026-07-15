package assessment

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

// Sentinel values that are MANAGER-ONLY (score/level + manager notes): they must
// NEVER appear in an employee projection, and MUST appear in an authorized
// manager projection. They carry no connector/credential shape so they do not
// trip NoRawContentLeak.
const (
	sentinelNarrative   = "MANAGER-ONLY narrative: final rating B plus, see calibration notes"
	sentinelManagerID   = "mgr-secret-line-lead"
	sentinelContributor = "actor-other-contributor"
	visSubject          = "actor-operator-7"
)

// assessmentFixture builds a valid, formal WorkAssessment for subject with:
//   - an assessed outcome_completion dimension carrying a satisfied SCORE, a
//     tier-1 SIGNED evidence receipt, a blocker attribution, a contribution that
//     names ANOTHER employee, and a full confidence LEVEL, and
//   - a not_assessable quality dimension (missing data),
//
// plus a MANAGER note (narrative) and a manager confirmation identity.
func assessmentFixture(subject string) model.WorkAssessment {
	wa := model.WorkAssessment{
		Tenant:         tnt,
		Org:            org,
		Subject:        subject,
		Level:          model.LevelIndividual,
		PolicyKey:      policyKey,
		PolicyRevision: 1,
		Version:        1,
		Formal:         true,
		OrgVersion:     3,
		Sources:        sources(),
		Period:         assessmentPeriod(),
		Graph: model.GraphSchema{
			Provider: "apache_age", SchemaVersion: "graph-v1", ProtocolVersion: "proj-v1",
			GraphName: "outcome_graph", Watermark: 42,
		},
		Dimensions: []model.DimensionResult{
			{
				Dimension:          model.DimOutcomeCompletion,
				State:              model.StateAssessed,
				Attribution:        model.AttrSharedOnce,
				CountedOutcomeKeys: []string{"outcome-a", "outcome-b"},
				SatisfiedOutcomes:  2,
				Evidence: []model.Evidence{{
					Tier: model.TierVerifiedOutcome, Handle: "outcome-a", Kind: "outcome",
					Revision: 1, Authority: "mes.official", SignatureKeyID: "sig-key-1", Verified: true,
					Summary: "verified throughput outcome",
				}},
				Blockers: []model.Blocker{{
					Handle: "blk-1", Kind: "dependency", Confidence: model.BlockerVerified,
					Delay: model.DelayExternal, Summary: "upstream dependency slipped",
				}},
				Contributions: []model.Contribution{{
					ContributorID: sentinelContributor, OutcomeKey: "outcome-b", Kind: "review", Weight: 0.5,
				}},
				Confidence: fullConfidence(model.ConfidenceHigh),
			},
			{
				Dimension:           model.DimQuality,
				State:               model.StateNotAssessable,
				NotAssessableReason: model.ReasonInsufficientData,
				Attribution:         model.AttrSharedOnce,
				Confidence:          fullConfidence(model.ConfidenceLow),
			},
		},
		Manager:   model.ManagerConfirmation{Confirmed: true, Manager: sentinelManagerID, ConfirmedAt: fixedNow()},
		CreatedAt: fixedNow(),
		Narrative: sentinelNarrative,
	}
	wa = wa.Normalize()
	wa.ID = wa.DerivedID()
	return wa
}

// --- GUARD 1: employee visibility B is fail-closed ---------------------------

func TestVisibilityEmployeeSeesOwnFactsNotScoreOrNotes(t *testing.T) {
	wa := assessmentFixture(visSubject)
	if err := wa.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	view, err := ProjectForEmployee(wa, visSubject)
	if err != nil {
		t.Fatalf("ProjectForEmployee: %v", err)
	}

	// The employee sees their OWN subject and goals.
	if view.Subject != visSubject {
		t.Fatalf("subject = %q, want %q", view.Subject, visSubject)
	}
	if view.Goals.OrgGoal.Key != sources().OrgGoal.Key || view.Goals.JobResponsibility.Key != sources().JobResponsibility.Key {
		t.Fatalf("employee must see their goals (job responsibility + org goal)")
	}

	var completion, quality *EmployeeDimensionView
	for i := range view.Dimensions {
		switch view.Dimensions[i].Dimension {
		case model.DimOutcomeCompletion:
			completion = &view.Dimensions[i]
		case model.DimQuality:
			quality = &view.Dimensions[i]
		}
	}
	if completion == nil || quality == nil {
		t.Fatalf("employee must see both dimensions, got %+v", view.Dimensions)
	}
	// WorkCase facts + completion (opaque counted-outcome refs).
	if len(completion.CompletedOutcomeRefs) != 2 {
		t.Fatalf("employee must see completion facts, got %v", completion.CompletedOutcomeRefs)
	}
	// Evidence REFERENCES.
	if len(completion.Evidence) == 0 || completion.Evidence[0].Handle == "" {
		t.Fatalf("employee must see evidence references")
	}
	// Blocker attribution.
	if len(completion.Blockers) == 0 || completion.Blockers[0].Delay != model.DelayExternal {
		t.Fatalf("employee must see blocker attribution")
	}
	// Missing data.
	if quality.MissingData != model.ReasonInsufficientData {
		t.Fatalf("employee must see missing-data reason, got %q", quality.MissingData)
	}

	// FAIL-CLOSED: the employee must NEVER see the final score/level or manager
	// notes. Assert structurally on the marshaled projection.
	raw, _ := json.Marshal(view)
	s := string(raw)
	for _, forbidden := range []string{
		sentinelNarrative,    // manager note text
		sentinelManagerID,    // manager identity
		sentinelContributor,  // another employee's contribution
		"satisfied_outcomes", // the score
		"components",         // the confidence-level breakdown
		"contributions",      // contributor attribution
		"narrative",          // manager note field
		"confirmed_at",       // manager confirmation field
	} {
		if strings.Contains(s, forbidden) {
			t.Fatalf("employee projection leaked forbidden score/level/notes token %q:\n%s", forbidden, s)
		}
	}
}

func TestVisibilityEmployeeCannotReadOthersAssessment(t *testing.T) {
	wa := assessmentFixture(visSubject)
	if _, err := ProjectForEmployee(wa, "actor-someone-else"); !errors.Is(err, ErrNotAssessmentSubject) {
		t.Fatalf("cross-employee read = %v, want ErrNotAssessmentSubject", err)
	}
	if _, err := ProjectForEmployee(wa, ""); !errors.Is(err, ErrNotAssessmentSubject) {
		t.Fatalf("empty employee id = %v, want ErrNotAssessmentSubject (fail closed)", err)
	}
}

func TestVisibilityEmployeeViewOmitsSubAssessments(t *testing.T) {
	// A hierarchical assessment (a manager who is also a subject) must never leak
	// a report's sub-assessment through the employee surface.
	child := assessmentFixture("actor-report-1")
	parent := assessmentFixture(visSubject)
	parent.SubAssessments = []model.WorkAssessment{child}
	parent = parent.Normalize()
	parent.ID = parent.DerivedID()

	view, err := ProjectForEmployee(parent, visSubject)
	if err != nil {
		t.Fatalf("ProjectForEmployee: %v", err)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), "actor-report-1") {
		t.Fatalf("employee projection must not leak a report's sub-assessment:\n%s", raw)
	}
}

// --- GUARD 2: manager scope is fail-closed (exact hierarchy + policy) --------

func TestVisibilityManagerWithinHierarchySeesScore(t *testing.T) {
	const orgVer = 7
	wa := assessmentFixture(visSubject)
	subject := governance.Party{UserID: visSubject, OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVer}
	manager := governance.Party{UserID: "u:div-lead", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: orgVer}

	view, err := ProjectForManager(ManagerScopeRequest{
		Assessment: wa, Manager: manager, Subject: subject,
		CurrentOrgVersion: orgVer, OwnedPolicyKeys: []string{policyKey},
	})
	if err != nil {
		t.Fatalf("authorized manager read: %v", err)
	}
	// An authorized manager sees the FULL structured result: score/level + notes.
	raw, _ := json.Marshal(view)
	s := string(raw)
	for _, must := range []string{"satisfied_outcomes", "components", sentinelNarrative, sentinelManagerID} {
		if !strings.Contains(s, must) {
			t.Fatalf("authorized manager must see %q in the detail:\n%s", must, s)
		}
	}
}

func TestVisibilityManagerOutsideScopeRefused(t *testing.T) {
	const orgVer = 7
	wa := assessmentFixture(visSubject)
	subject := governance.Party{UserID: visSubject, OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVer}
	base := func(mgr governance.Party, curOrg int64, owned []string) ManagerScopeRequest {
		return ManagerScopeRequest{Assessment: wa, Manager: mgr, Subject: subject, CurrentOrgVersion: curOrg, OwnedPolicyKeys: owned}
	}
	authorizedMgr := governance.Party{UserID: "u:div-lead", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: orgVer}

	subjectMismatch := base(authorizedMgr, orgVer, []string{policyKey})
	subjectMismatch.Subject = governance.Party{UserID: "actor-someone-else", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVer}

	cases := []struct {
		name string
		req  ManagerScopeRequest
		want error
	}{
		{"peer_same_unit", base(governance.Party{UserID: "u:peer", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVer}, orgVer, []string{policyKey}), ErrManagerNotAuthorized},
		{"unrelated_branch", base(governance.Party{UserID: "u:other", OrgUnitID: "org:other-div", OrgPath: []string{"org:root"}, OrgVersion: orgVer}, orgVer, []string{policyKey}), ErrManagerNotAuthorized},
		{"subordinate", base(governance.Party{UserID: "u:sub", OrgUnitID: "org:sub-team", OrgPath: []string{"org:root", "org:div", "org:team-a"}, OrgVersion: orgVer}, orgVer, []string{policyKey}), ErrManagerNotAuthorized},
		// Self-review: the subject cannot read their own assessment as a "manager"
		// (they own the policy and pass the subject-party check, but the hierarchy
		// primitive refuses a self-reviewer).
		{"self_manager_is_subject", base(governance.Party{UserID: visSubject, OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: orgVer}, orgVer, []string{policyKey}), ErrManagerNotAuthorized},
		{"stale_org_snapshot", base(governance.Party{UserID: "u:div-lead", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: orgVer - 1}, orgVer, []string{policyKey}), ErrManagerNotAuthorized},
		{"policy_not_owned", base(authorizedMgr, orgVer, nil), ErrPolicyNotOwned},
		{"subject_party_mismatch", subjectMismatch, ErrSubjectPartyMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ProjectForManager(tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("ProjectForManager = %v, want %v", err, tc.want)
			}
		})
	}
}

// --- GUARD 4: HR-action isolation --------------------------------------------

func TestHRIsolationNoAssessmentToHRAction(t *testing.T) {
	wa := assessmentFixture(visSubject)

	kinds := HRActionKinds()
	if len(kinds) < 7 {
		t.Fatalf("expected the closed set of HR consequences (bonus/promotion/demotion/transfer/discipline/termination/negative record), got %d", len(kinds))
	}
	for _, k := range kinds {
		if err := DeriveHRAction(wa, k); !errors.Is(err, ErrHRActionRequiresSeparateWorkCase) {
			t.Fatalf("DeriveHRAction(%q) = %v, want ErrHRActionRequiresSeparateWorkCase (assessment never directly triggers HR consequences)", k, err)
		}
	}

	// The projections expose NO HR-action capability: no HR-action term appears in
	// either the employee or the authorized manager projection.
	empView, err := ProjectForEmployee(wa, visSubject)
	if err != nil {
		t.Fatalf("employee view: %v", err)
	}
	subject := governance.Party{UserID: visSubject, OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: 7}
	manager := governance.Party{UserID: "u:div-lead", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: 7}
	mgrView, err := ProjectForManager(ManagerScopeRequest{Assessment: wa, Manager: manager, Subject: subject, CurrentOrgVersion: 7, OwnedPolicyKeys: []string{policyKey}})
	if err != nil {
		t.Fatalf("manager view: %v", err)
	}
	for _, blob := range []any{empView, mgrView} {
		raw, _ := json.Marshal(blob)
		s := strings.ToLower(string(raw))
		for _, term := range []string{"bonus", "promotion", "demotion", "termination", "discipline", "hr_action"} {
			if strings.Contains(s, term) {
				t.Fatalf("assessment projection must expose no HR-action term, found %q:\n%s", term, s)
			}
		}
	}
}
