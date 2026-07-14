package governance

import (
	"errors"
	"testing"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
)

// Org hierarchy used across these tests (root..leaf):
//
//	org:root
//	  └─ org:div
//	       ├─ org:team-a   (submitter sits here)
//	       └─ org:team-b
//
// OrgPath is the strict ancestor chain of a party's OrgUnitID (root-first,
// excluding the unit itself).
const currentOrgVersion = int64(42)

func submitterParty() Party {
	return Party{UserID: "u-submitter", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: currentOrgVersion}
}

// upwardReviewer sits at org:div, a strict ancestor of the submitter's team.
func upwardReviewer(user string) Party {
	return Party{UserID: user, OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: currentOrgVersion}
}

// higherReviewer sits at org:root, also a strict ancestor of the submitter.
func higherReviewer(user string) Party {
	return Party{UserID: user, OrgUnitID: "org:root", OrgPath: nil, OrgVersion: currentOrgVersion}
}

func policy() UpwardReviewPolicy { return UpwardReviewPolicy{CurrentOrgVersion: currentOrgVersion} }

func TestUpwardReviewPolicyLowRiskRequiresOneEligible(t *testing.T) {
	err := policy().Evaluate(ApprovalParties{
		Submitter: submitterParty(),
		Reviewers: []Party{upwardReviewer("u-mgr")},
	}, model.RiskLow)
	if err != nil {
		t.Fatalf("one eligible upward reviewer must satisfy a low-risk side effect, got %v", err)
	}
}

func TestUpwardReviewPolicyLowRiskRejectsZeroReviewers(t *testing.T) {
	err := policy().Evaluate(ApprovalParties{Submitter: submitterParty()}, model.RiskLow)
	if !errors.Is(err, ErrNoUpwardReviewer) {
		t.Fatalf("low risk with no reviewer must fail with ErrNoUpwardReviewer, got %v", err)
	}
}

func TestUpwardReviewPolicyHighRiskRequiresTwoDistinct(t *testing.T) {
	err := policy().Evaluate(ApprovalParties{
		Submitter: submitterParty(),
		Reviewers: []Party{upwardReviewer("u-mgr"), higherReviewer("u-dir")},
	}, model.RiskHigh)
	if err != nil {
		t.Fatalf("two distinct eligible upward reviewers must satisfy a high-risk side effect, got %v", err)
	}
}

func TestUpwardReviewPolicyHighRiskRejectsSingleReviewer(t *testing.T) {
	err := policy().Evaluate(ApprovalParties{
		Submitter: submitterParty(),
		Reviewers: []Party{upwardReviewer("u-mgr")},
	}, model.RiskHigh)
	if !errors.Is(err, ErrNoUpwardReviewer) {
		t.Fatalf("high risk with only one reviewer must fail (needs two), got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsSubmitterAsReviewer(t *testing.T) {
	sub := submitterParty()
	selfReview := upwardReviewer(sub.UserID) // same UserID as the submitter
	err := policy().Evaluate(ApprovalParties{Submitter: sub, Reviewers: []Party{selfReview}}, model.RiskLow)
	if !errors.Is(err, ErrReviewerIsSubmitter) {
		t.Fatalf("a submitter reviewing their own side effect must fail, got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsPeer(t *testing.T) {
	// A reviewer in the SAME org unit as the submitter is a peer, not upward.
	peer := Party{UserID: "u-peer", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: currentOrgVersion}
	err := policy().Evaluate(ApprovalParties{Submitter: submitterParty(), Reviewers: []Party{peer}}, model.RiskLow)
	if !errors.Is(err, ErrReviewerIsPeer) {
		t.Fatalf("a peer reviewer must fail, got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsSubordinate(t *testing.T) {
	// A reviewer BELOW the submitter (submitter's unit is a strict ancestor of
	// the reviewer's) is a subordinate.
	sub := Party{UserID: "u-lead", OrgUnitID: "org:div", OrgPath: []string{"org:root"}, OrgVersion: currentOrgVersion}
	subordinate := Party{UserID: "u-junior", OrgUnitID: "org:team-a", OrgPath: []string{"org:root", "org:div"}, OrgVersion: currentOrgVersion}
	err := policy().Evaluate(ApprovalParties{Submitter: sub, Reviewers: []Party{subordinate}}, model.RiskLow)
	if !errors.Is(err, ErrReviewerIsSubordinate) {
		t.Fatalf("a subordinate reviewer must fail, got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsDuplicateReviewer(t *testing.T) {
	// Two decisions from the SAME reviewer identity cannot satisfy the
	// "two distinct" high-risk requirement.
	dup := upwardReviewer("u-mgr")
	err := policy().Evaluate(ApprovalParties{Submitter: submitterParty(), Reviewers: []Party{dup, dup}}, model.RiskHigh)
	if !errors.Is(err, ErrDuplicateReviewer) {
		t.Fatalf("a duplicated reviewer identity must fail, got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsStaleOrgReviewer(t *testing.T) {
	stale := upwardReviewer("u-mgr")
	stale.OrgVersion = currentOrgVersion - 1 // decided against an out-of-date org snapshot
	err := policy().Evaluate(ApprovalParties{Submitter: submitterParty(), Reviewers: []Party{stale}}, model.RiskLow)
	if !errors.Is(err, ErrStaleOrgReviewer) {
		t.Fatalf("a reviewer deciding against a stale org version must fail, got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsStaleSubmitter(t *testing.T) {
	sub := submitterParty()
	sub.OrgVersion = currentOrgVersion - 5
	err := policy().Evaluate(ApprovalParties{Submitter: sub, Reviewers: []Party{upwardReviewer("u-mgr")}}, model.RiskLow)
	if !errors.Is(err, ErrStaleOrgReviewer) {
		t.Fatalf("a stale submitter org snapshot must invalidate the whole decision, got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsUnrelatedBranch(t *testing.T) {
	// A reviewer in a different branch that is neither an ancestor nor a
	// descendant of the submitter is not "upward".
	other := Party{UserID: "u-other", OrgUnitID: "org:other-div", OrgPath: []string{"org:root"}, OrgVersion: currentOrgVersion}
	err := policy().Evaluate(ApprovalParties{Submitter: submitterParty(), Reviewers: []Party{other}}, model.RiskLow)
	if !errors.Is(err, ErrReviewerNotUpward) {
		t.Fatalf("an unrelated-branch reviewer must fail as not-upward, got %v", err)
	}
}

func TestUpwardReviewPolicyRejectsUnknownRisk(t *testing.T) {
	if err := policy().Evaluate(ApprovalParties{Submitter: submitterParty(), Reviewers: []Party{upwardReviewer("u-mgr")}}, model.RiskLevel("extreme")); err == nil {
		t.Fatal("an unrecognized risk tier must be rejected")
	}
}

func TestUpwardReviewPolicyRequiredCounts(t *testing.T) {
	p := policy()
	if n, err := p.Required(model.RiskLow); err != nil || n != 1 {
		t.Fatalf("low risk requires 1 reviewer, got %d, %v", n, err)
	}
	if n, err := p.Required(model.RiskHigh); err != nil || n != 2 {
		t.Fatalf("high risk requires 2 reviewers, got %d, %v", n, err)
	}
	if _, err := p.Required(model.RiskLevel("bogus")); err == nil {
		t.Fatal("unknown risk must error")
	}
}
