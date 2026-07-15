package assessment

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
)

// correctionEnv wires an append-only ResultStore seeded with an immutable v1
// assessment, an append-only CorrectionStore, and the REAL Task 18C evaluator
// (so an accepted correction produces a new version via deterministic
// re-evaluation, never an in-place edit).
type correctionEnv struct {
	svc     *CorrectionService
	results *MemoryResultStore
	corr    *MemoryCorrectionStore
	v1      model.WorkAssessment
}

func newCorrectionEnv(t *testing.T) correctionEnv {
	t.Helper()
	ctx := context.Background()

	results := NewMemoryResultStore(fixedNow)
	v1, err := results.AppendAssessment(ctx, assessmentFixture(visSubject))
	if err != nil {
		t.Fatalf("seed v1: %v", err)
	}

	// The authoritative outcome store + typed graph the re-evaluation grounds on.
	ostore := outcome.NewMemoryStore(fixedNow)
	seedOutcomeChain(t, ostore, tnt, fxOutcome{Key: "outcome-a", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
	graph := &fakeGraph{wm: 42, nodes: candidateNodes([]fxCandidate{{Key: "outcome-a", Revision: 1}})}
	ev := newEvaluator(t, graph, ostore)

	corr := NewMemoryCorrectionStore(fixedNow)
	svc := NewCorrectionService(corr, results, ev, fixedNow)
	return correctionEnv{svc: svc, results: results, corr: corr, v1: v1}
}

func reevaluationRequest() EvaluateRequest {
	return EvaluateRequest{
		Tenant: tnt, Org: org, Subject: visSubject, Level: model.LevelIndividual,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}),
		Period: assessmentPeriod(), Formal: true,
		Manager:         model.ManagerConfirmation{Confirmed: true, Manager: "mgr-line-lead", ConfirmedAt: fixedNow()},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-a"}}},
		DecidedAt:       fixedNow(),
	}
}

// --- GUARD 3: a correction never edits the old assessment in place -----------

func TestCorrectionAddsEvidenceNeverMutatesOld(t *testing.T) {
	ctx := context.Background()
	env := newCorrectionEnv(t)
	before, _ := json.Marshal(env.v1)

	c, err := env.svc.Submit(ctx, SubmitCorrectionRequest{
		Tenant: tnt, EmployeeID: visSubject, TargetAssessmentID: env.v1.ID,
		Kind: CorrectionAddEvidence, Dimension: model.DimOutcomeCompletion,
		AddedEvidence: []model.Evidence{{Tier: model.TierHumanReport, Handle: "hr-1", Kind: "report", Authority: "peer.lead", Summary: "I also delivered milestone x"}},
		Rationale:     "adding an accepted deliverable the assessment missed",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if c.Resolution.State != CorrectionPending {
		t.Fatalf("a freshly submitted correction must be pending, got %q", c.Resolution.State)
	}
	if c.TargetAssessmentID != env.v1.ID || c.TargetDigest != env.v1.Digest() {
		t.Fatalf("correction must reference the exact immutable target version (id+digest) for traceability")
	}

	// The old assessment is byte-identical: submission never touches it.
	got, err := env.results.GetAssessment(ctx, tnt, env.v1.ID)
	if err != nil {
		t.Fatalf("GetAssessment old: %v", err)
	}
	after, _ := json.Marshal(got)
	if string(before) != string(after) {
		t.Fatalf("submitting a correction must NOT mutate the old assessment in place")
	}
}

func TestCorrectionAcceptedProducesNewImmutableVersion(t *testing.T) {
	ctx := context.Background()
	env := newCorrectionEnv(t)
	oldBytes, _ := json.Marshal(env.v1)

	c, err := env.svc.Submit(ctx, SubmitCorrectionRequest{
		Tenant: tnt, EmployeeID: visSubject, TargetAssessmentID: env.v1.ID,
		Kind: CorrectionChallengeAttribution, Dimension: model.DimOutcomeCompletion,
		ChallengedHandle: "outcome-b", Rationale: "outcome-b should not be attributed to me; I only reviewed it",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	resolved, newWA, err := env.svc.Resolve(ctx, ResolveCorrectionRequest{
		Tenant: tnt, CorrectionID: c.ID, Accept: true, DecidedBy: "u:div-lead",
		Reason: "challenge upheld; re-assessed without the mis-attributed outcome", Reevaluation: reevaluationRequest(),
	})
	if err != nil {
		t.Fatalf("Resolve accept: %v", err)
	}

	// An accepted correction yields a NEW assessment VERSION (never an edit).
	if newWA.Version != env.v1.Version+1 {
		t.Fatalf("accepted correction must produce version %d, got %d", env.v1.Version+1, newWA.Version)
	}
	if newWA.ID == env.v1.ID {
		t.Fatalf("the re-assessment must be a new immutable id, not the old one")
	}
	if resolved.Resolution.State != CorrectionAccepted {
		t.Fatalf("resolution state = %q, want accepted", resolved.Resolution.State)
	}
	if resolved.Resolution.NewAssessmentID != newWA.ID || resolved.Resolution.NewVersion != newWA.Version {
		t.Fatalf("resolution must link the new version (%s v%d), got %s v%d", newWA.ID, newWA.Version, resolved.Resolution.NewAssessmentID, resolved.Resolution.NewVersion)
	}

	// The OLD version stays immutable AND traceable.
	old, err := env.results.GetAssessment(ctx, tnt, env.v1.ID)
	if err != nil {
		t.Fatalf("old version must remain retrievable: %v", err)
	}
	afterBytes, _ := json.Marshal(old)
	if string(oldBytes) != string(afterBytes) {
		t.Fatalf("accepting a correction must NOT mutate the old version")
	}
	// Both versions are retrievable — the audit trail is intact.
	if _, err := env.results.GetAssessment(ctx, tnt, newWA.ID); err != nil {
		t.Fatalf("new version must be retrievable: %v", err)
	}
}

func TestCorrectionNeverDeletesSignedReceipt(t *testing.T) {
	ctx := context.Background()
	env := newCorrectionEnv(t)

	c, err := env.svc.Submit(ctx, SubmitCorrectionRequest{
		Tenant: tnt, EmployeeID: visSubject, TargetAssessmentID: env.v1.ID,
		Kind: CorrectionChallengeFact, Dimension: model.DimOutcomeCompletion,
		ChallengedHandle: "outcome-a", Rationale: "the outcome-a fact is wrong",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, _, err := env.svc.Resolve(ctx, ResolveCorrectionRequest{
		Tenant: tnt, CorrectionID: c.ID, Accept: true, DecidedBy: "u:div-lead", Reevaluation: reevaluationRequest(),
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Even after an accepted correction, the OLD version's signed tier-1 receipt
	// is untouched: a correction can never delete a signed receipt.
	old, err := env.results.GetAssessment(ctx, tnt, env.v1.ID)
	if err != nil {
		t.Fatalf("GetAssessment old: %v", err)
	}
	found := false
	for _, d := range old.Dimensions {
		for _, e := range d.Evidence {
			if e.Handle == "outcome-a" && e.SignatureKeyID == "sig-key-1" && e.Verified {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the old version's signed observation receipt must remain intact after a correction")
	}
}

func TestCorrectionRejectedKeepsOldTraceable(t *testing.T) {
	ctx := context.Background()
	env := newCorrectionEnv(t)

	c, err := env.svc.Submit(ctx, SubmitCorrectionRequest{
		Tenant: tnt, EmployeeID: visSubject, TargetAssessmentID: env.v1.ID,
		Kind: CorrectionAddEvidence, Dimension: model.DimQuality,
		AddedEvidence: []model.Evidence{{Tier: model.TierHumanReport, Handle: "hr-2", Kind: "report", Authority: "peer.qa", Summary: "quality evidence"}},
		Rationale:     "quality was assessable after all",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	resolved, newWA, err := env.svc.Resolve(ctx, ResolveCorrectionRequest{
		Tenant: tnt, CorrectionID: c.ID, Accept: false, DecidedBy: "u:div-lead", Reason: "insufficient corroboration",
	})
	if err != nil {
		t.Fatalf("Resolve reject: %v", err)
	}
	if resolved.Resolution.State != CorrectionRejected {
		t.Fatalf("resolution = %q, want rejected", resolved.Resolution.State)
	}
	if newWA.Version != 0 {
		t.Fatalf("a rejected correction produces no new version, got %d", newWA.Version)
	}
	// The old version stays put and no v2 was minted.
	if _, err := env.results.LatestVersion(ctx, tnt, visSubject, policyKey, 1); err != nil {
		t.Fatalf("old version must remain retrievable: %v", err)
	}
	if _, err := env.results.GetAssessment(ctx, tnt, env.v1.ID); err != nil {
		t.Fatalf("old version must remain retrievable after reject: %v", err)
	}
}

func TestCorrectionResolveIsOneShot(t *testing.T) {
	ctx := context.Background()
	env := newCorrectionEnv(t)
	c, err := env.svc.Submit(ctx, SubmitCorrectionRequest{
		Tenant: tnt, EmployeeID: visSubject, TargetAssessmentID: env.v1.ID,
		Kind: CorrectionChallengeFact, Dimension: model.DimOutcomeCompletion,
		ChallengedHandle: "outcome-a", Rationale: "fact disputed",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, _, err := env.svc.Resolve(ctx, ResolveCorrectionRequest{Tenant: tnt, CorrectionID: c.ID, Accept: false, DecidedBy: "u:div-lead"}); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// A resolved correction is frozen: it cannot be re-decided.
	if _, _, err := env.svc.Resolve(ctx, ResolveCorrectionRequest{Tenant: tnt, CorrectionID: c.ID, Accept: true, DecidedBy: "u:div-lead", Reevaluation: reevaluationRequest()}); !errors.Is(err, ErrCorrectionResolved) {
		t.Fatalf("re-resolving a decided correction = %v, want ErrCorrectionResolved", err)
	}
}

// --- fail-closed: an employee may only correct their OWN assessment ----------

func TestCorrectionSubjectMismatchRefused(t *testing.T) {
	ctx := context.Background()
	env := newCorrectionEnv(t)
	if _, err := env.svc.Submit(ctx, SubmitCorrectionRequest{
		Tenant: tnt, EmployeeID: "actor-someone-else", TargetAssessmentID: env.v1.ID,
		Kind: CorrectionChallengeFact, Dimension: model.DimOutcomeCompletion,
		ChallengedHandle: "outcome-a", Rationale: "not my assessment",
	}); !errors.Is(err, ErrNotAssessmentSubject) {
		t.Fatalf("cross-employee correction = %v, want ErrNotAssessmentSubject", err)
	}
}

func TestCorrectionUnknownTargetRefused(t *testing.T) {
	ctx := context.Background()
	env := newCorrectionEnv(t)
	if _, err := env.svc.Submit(ctx, SubmitCorrectionRequest{
		Tenant: tnt, EmployeeID: visSubject, TargetAssessmentID: "wa_does_not_exist",
		Kind: CorrectionChallengeFact, Dimension: model.DimOutcomeCompletion,
		ChallengedHandle: "outcome-a", Rationale: "no such target",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("correction against a missing target = %v, want ErrNotFound", err)
	}
}
