package workcase_test

import (
	"context"
	"errors"
	"testing"

	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

// validPlan returns a minimal, schema-valid WorkPlan (a single read step,
// no FailurePolicy required since it never uses Kind "write"). Callers that
// need a distinct Step ID mutate the returned value's Steps[0].ID.
func validPlan() sdkworkcase.WorkPlan {
	return sdkworkcase.WorkPlan{
		Steps: []sdkworkcase.Step{{
			ID: "step-1",
			Action: &sdkworkcase.ActionSpec{
				Kind:               "read",
				BusinessCapability: "mes.anomaly.read",
				ParametersHash:     "sha256:fixture",
				IdempotencyKey:     "case-1:step-1:v1",
			},
		}},
	}
}

func newTestService(t *testing.T) *workcase.Service {
	t.Helper()
	store := workcase.NewMemoryStore(nil)
	svc, err := workcase.NewService(store, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func createCase(t *testing.T, svc *workcase.Service, ent, org, actor, idem string) sdkworkcase.WorkCase {
	t.Helper()
	c, err := svc.Create(context.Background(), workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: org, ActorRef: actor, IdempotencyKey: idem,
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return c
}

func TestServiceCreateStartsDraftCaseAtRevisionOne(t *testing.T) {
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000001")
	if c.Status != sdkworkcase.StatusDraft || c.Revision != 1 || c.EnterpriseID != "ent-1" || c.OrgScope != "org:team-a" || c.ActorRef != "actor-1" || c.ID == "" {
		t.Fatalf("unexpected case %+v", c)
	}
	if len(c.Plans) != 0 {
		t.Fatalf("fresh case must have no plans yet: %+v", c.Plans)
	}
}

// TestServiceProposePlanAppendsImmutableRevisions covers the required
// "immutable executing plans" scenario: once a case is reviewing/executing,
// ProposePlan must never mutate an existing WorkPlan revision in place; it
// always appends a new revision, leaving prior revisions byte-for-byte
// unchanged.
func TestServiceProposePlanAppendsImmutableRevisions(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000002")

	planA := validPlan()
	afterA, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: "idem-plan-a-0000000001"},
		Plan:    planA,
	})
	if err != nil {
		t.Fatalf("ProposePlan A: %v", err)
	}
	if len(afterA.Plans) != 1 || afterA.Plans[0].Revision != 1 || afterA.Plans[0].Steps[0].ID != "step-1" {
		t.Fatalf("plan A not appended as revision 1: %+v", afterA.Plans)
	}

	afterReview, err := svc.StartReview(ctx, workcase.StartReviewCommand{Command: workcase.Command{
		EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterA.Revision, IdempotencyKey: "idem-review-0000000001",
	}})
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	afterExec, err := svc.StartExecution(ctx, workcase.StartExecutionCommand{Command: workcase.Command{
		EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterReview.Revision, IdempotencyKey: "idem-exec-0000000001",
	}})
	if err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	if afterExec.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("case not executing: %+v", afterExec)
	}
	if afterExec.Plans[0].Revision != 1 || afterExec.Plans[0].Steps[0].ID != "step-1" {
		t.Fatalf("revision 1 changed merely by entering review/execution: %+v", afterExec.Plans[0])
	}

	planB := validPlan()
	planB.Steps[0].ID = "step-2"
	afterB, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterExec.Revision, IdempotencyKey: "idem-plan-b-0000000001"},
		Plan:    planB,
	})
	if err != nil {
		t.Fatalf("ProposePlan B on an executing case: %v", err)
	}
	if len(afterB.Plans) != 2 {
		t.Fatalf("expected 2 plan revisions after proposing on an executing case, got %d: %+v", len(afterB.Plans), afterB.Plans)
	}
	if afterB.Plans[0].Revision != 1 || afterB.Plans[0].Steps[0].ID != "step-1" || len(afterB.Plans[0].Steps) != 1 {
		t.Fatalf("revision 1 was mutated once the case started executing: %+v", afterB.Plans[0])
	}
	if afterB.Plans[1].Revision != 2 || afterB.Plans[1].Steps[0].ID != "step-2" {
		t.Fatalf("revision 2 not appended correctly: %+v", afterB.Plans[1])
	}
}

func TestServiceCrossTenantAccessLooksLikeNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000003")

	if _, err := svc.Get(ctx, "ent-2", "org:team-a", c.ID); !errors.Is(err, workcase.ErrNotFound) {
		t.Fatalf("wrong enterprise Get: got %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(ctx, "ent-1", "org:team-b", c.ID); !errors.Is(err, workcase.ErrNotFound) {
		t.Fatalf("wrong org scope Get: got %v, want ErrNotFound", err)
	}
	_, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-2", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: "idem-crosstenant-0000001"},
		Plan:    validPlan(),
	})
	if !errors.Is(err, workcase.ErrNotFound) {
		t.Fatalf("cross-tenant ProposePlan: got %v, want ErrNotFound (never a distinct forbidden/leak error)", err)
	}
}

func TestServiceStaleRevisionRejectsMismatchedExpectation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000004")

	if _, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: "idem-stale-first-00001"},
		Plan:    validPlan(),
	}); err != nil {
		t.Fatalf("first ProposePlan: %v", err)
	}

	// Same (now stale) expected revision, a fresh idempotency key so this
	// is not treated as a replay of the call above.
	_, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: "idem-stale-second-0001"},
		Plan:    validPlan(),
	})
	if !errors.Is(err, workcase.ErrStaleRevision) {
		t.Fatalf("got %v, want ErrStaleRevision", err)
	}

	// Re-Create over the same case ID: ExpectedRevision 0 no longer
	// matches reality (revision 2 by now) -- also a stale expectation.
	_, err = svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, IdempotencyKey: "idem-stale-recreate-01",
	}})
	if !errors.Is(err, workcase.ErrStaleRevision) {
		t.Fatalf("recreate got %v, want ErrStaleRevision", err)
	}
}

// TestServiceDuplicateIdempotencyKeyReplaysPreviousResult covers "duplicate
// command replay": a repeated IdempotencyKey must return the ORIGINAL
// result, even after the aggregate has moved on to a state where a fresh
// application of the same command shape would now be invalid.
func TestServiceDuplicateIdempotencyKeyReplaysPreviousResult(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	createCmd := workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: "idem-replay-0000000001",
	}}
	first, err := svc.Create(ctx, createCmd)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := svc.Create(ctx, createCmd)
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if second.ID != first.ID || second.Revision != first.Revision || second.Status != first.Status || len(second.Plans) != len(first.Plans) {
		t.Fatalf("replay produced a different result: first=%+v second=%+v", first, second)
	}

	planCmd := workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: first.ID, ExpectedRevision: first.Revision, IdempotencyKey: "idem-replay-plan-00001"},
		Plan:    validPlan(),
	}
	afterPlan, err := svc.ProposePlan(ctx, planCmd)
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	if _, err := svc.StartReview(ctx, workcase.StartReviewCommand{Command: workcase.Command{
		EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: first.ID, ExpectedRevision: afterPlan.Revision, IdempotencyKey: "idem-replay-review-001",
	}}); err != nil {
		t.Fatalf("StartReview: %v", err)
	}

	// Replaying planCmd now: current status is "reviewing" and
	// ExpectedRevision (1) no longer matches current revision (3), so a
	// FRESH application would fail both the transition check and the
	// revision check. The replay must short-circuit before either check
	// and hand back the original, still-valid result.
	replay, err := svc.ProposePlan(ctx, planCmd)
	if err != nil {
		t.Fatalf("replayed ProposePlan: %v", err)
	}
	if replay.Revision != afterPlan.Revision || len(replay.Plans) != len(afterPlan.Plans) {
		t.Fatalf("replay diverged from original: original=%+v replay=%+v", afterPlan, replay)
	}
}

func TestServiceIdempotencyKeyReusedForDifferentCaseIsConflict(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	a := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-conflict-case-a-01")
	_ = createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-conflict-case-b-01")

	_, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: a.ID, ExpectedRevision: a.Revision, IdempotencyKey: "idem-conflict-case-b-01"},
		Plan:    validPlan(),
	})
	if !errors.Is(err, workcase.ErrIdempotencyKeyReused) {
		t.Fatalf("got %v, want ErrIdempotencyKeyReused", err)
	}
}

// TestServiceInvalidStateTransitionSkippingReview covers "invalid state
// transition": StartExecution requires the case to already be reviewing.
func TestServiceInvalidStateTransitionSkippingReview(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000005")
	_, err := svc.StartExecution(ctx, workcase.StartExecutionCommand{Command: workcase.Command{
		EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: "idem-skip-review-00001",
	}})
	if !errors.Is(err, workcase.ErrInvalidTransition) {
		t.Fatalf("got %v, want ErrInvalidTransition", err)
	}
}

func TestServiceStartReviewRequiresAProposedPlan(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000008")
	_, err := svc.StartReview(ctx, workcase.StartReviewCommand{Command: workcase.Command{
		EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: "idem-noplan-review-001",
	}})
	if !errors.Is(err, workcase.ErrInvalidTransition) {
		t.Fatalf("got %v, want ErrInvalidTransition", err)
	}
}

// idemKey pads prefix+"-"+suffix out to Command.IdempotencyKey's minimum
// length (16 chars) regardless of how short prefix is, so callers can pass
// short, readable prefixes without silently tripping ErrInvalidCommand.
func idemKey(prefix, suffix string) string {
	k := prefix + "-" + suffix
	for len(k) < 16 {
		k += "-0"
	}
	return k
}

func advanceToExecuting(t *testing.T, svc *workcase.Service, ent, org, actor, caseID string, rev uint64, idemPrefix string) sdkworkcase.WorkCase {
	t.Helper()
	ctx := context.Background()
	afterPlan, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: ent, OrgScope: org, ActorRef: actor, CaseID: caseID, ExpectedRevision: rev, IdempotencyKey: idemKey(idemPrefix, "plan")},
		Plan:    validPlan(),
	})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	afterReview, err := svc.StartReview(ctx, workcase.StartReviewCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: org, ActorRef: actor, CaseID: caseID, ExpectedRevision: afterPlan.Revision, IdempotencyKey: idemKey(idemPrefix, "review"),
	}})
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	afterExec, err := svc.StartExecution(ctx, workcase.StartExecutionCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: org, ActorRef: actor, CaseID: caseID, ExpectedRevision: afterReview.Revision, IdempotencyKey: idemKey(idemPrefix, "exec"),
	}})
	if err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	return afterExec
}

func TestServiceTransitionStepDrivesCaseLifecycleToCompleted(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000006")
	afterExec := advanceToExecuting(t, svc, "ent-1", "org:team-a", "actor-1", c.ID, c.Revision, "idem-complete")

	running, err := svc.TransitionStep(ctx, workcase.TransitionStepCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterExec.Revision, IdempotencyKey: "idem-complete-run0001"},
		StepID:  "step-1", Status: sdkworkcase.StepRunning,
	})
	if err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	if running.Status != sdkworkcase.StatusExecuting {
		t.Fatalf("case left executing early: %+v", running)
	}

	completed, err := svc.TransitionStep(ctx, workcase.TransitionStepCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: running.Revision, IdempotencyKey: "idem-complete-done001"},
		StepID:  "step-1", Status: sdkworkcase.StepCompleted,
		Evidence: []sdkworkcase.EvidenceRef{{Handle: "ev-1", ContentHash: "sha256:ev", Authority: "integration"}},
	})
	if err != nil {
		t.Fatalf("transition to completed: %v", err)
	}
	if completed.Status != sdkworkcase.StatusCompleted {
		t.Fatalf("case did not auto-complete once every step completed: %+v", completed)
	}
	step := completed.Plans[len(completed.Plans)-1].Steps[0]
	if step.Status != sdkworkcase.StepCompleted || len(step.Evidence) != 1 || step.Evidence[0].Handle != "ev-1" {
		t.Fatalf("step not recorded: %+v", step)
	}

	// Invalid step transition: completed -> running must be rejected.
	_, err = svc.TransitionStep(ctx, workcase.TransitionStepCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: completed.Revision, IdempotencyKey: "idem-complete-redo001"},
		StepID:  "step-1", Status: sdkworkcase.StepRunning,
	})
	if !errors.Is(err, workcase.ErrInvalidTransition) {
		t.Fatalf("got %v, want ErrInvalidTransition", err)
	}
}

func TestServiceTransitionStepFailureTerminatesCase(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000007")
	afterExec := advanceToExecuting(t, svc, "ent-1", "org:team-a", "actor-1", c.ID, c.Revision, "idem-fail")

	running, err := svc.TransitionStep(ctx, workcase.TransitionStepCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterExec.Revision, IdempotencyKey: "idem-fail-run-000001"},
		StepID:  "step-1", Status: sdkworkcase.StepRunning,
	})
	if err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	failed, err := svc.TransitionStep(ctx, workcase.TransitionStepCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: running.Revision, IdempotencyKey: "idem-fail-failed-0001"},
		StepID:  "step-1", Status: sdkworkcase.StepFailed,
	})
	if err != nil {
		t.Fatalf("transition to failed: %v", err)
	}
	if failed.Status != sdkworkcase.StatusTerminated {
		t.Fatalf("case did not terminate on step failure: %+v", failed)
	}

	// Terminal state: further commands are rejected outright.
	_, err = svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: failed.Revision, IdempotencyKey: "idem-fail-after-00001"},
		Plan:    validPlan(),
	})
	if !errors.Is(err, workcase.ErrInvalidTransition) {
		t.Fatalf("ProposePlan on a terminated case: got %v, want ErrInvalidTransition", err)
	}
}

func TestServiceCreateValidatesCommandShape(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	cases := []workcase.CreateCommand{
		{Command: workcase.Command{OrgScope: "org:a", ActorRef: "actor", IdempotencyKey: "idem-missing-ent-00001"}},
		{Command: workcase.Command{EnterpriseID: "ent-1", ActorRef: "actor", IdempotencyKey: "idem-missing-org-00001"}},
		{Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:a", IdempotencyKey: "idem-missing-actor-0001"}},
		{Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:a", ActorRef: "actor", IdempotencyKey: "short"}},
	}
	for i, cmd := range cases {
		if _, err := svc.Create(ctx, cmd); !errors.Is(err, workcase.ErrInvalidCommand) {
			t.Fatalf("case %d: got %v, want ErrInvalidCommand", i, err)
		}
	}
}

func TestServiceProposePlanRejectsInvalidPlan(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	c := createCase(t, svc, "ent-1", "org:team-a", "actor-1", "idem-create-0000000009")
	badPlan := validPlan()
	badPlan.Steps[0].Action = &sdkworkcase.ActionSpec{
		Kind: "write", BusinessCapability: "mes.work_order.create",
		ParametersHash: "sha256:fixture", IdempotencyKey: "case-1:step-1:v1",
		// FailurePolicy deliberately omitted: workcase.ValidatePlan must reject this.
	}
	_, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: "ent-1", OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: "idem-badplan-0000001"},
		Plan:    badPlan,
	})
	if !errors.Is(err, workcase.ErrInvalidCommand) {
		t.Fatalf("got %v, want ErrInvalidCommand", err)
	}
}
