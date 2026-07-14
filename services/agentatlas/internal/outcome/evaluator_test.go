package outcome_test

// evaluator_test.go (RED first, Task 0H) drives the deterministic Outcome
// Evaluator and the idempotent ClosureService against SIGNED nexus fixtures. It
// proves, one test per Step-1 case and per critical semantic constraint:
//
//   - satisfied requires a fresh, authoritative, signature-verified observation;
//   - a technically-succeeded ActionReceipt with a refuting/absent observation is
//     NEVER satisfied (action success is not an outcome);
//   - freshness (VerificationNeed.MaxAge vs ObservationReceipt.ObservedAt) is
//     enforced HERE (0D/0E deferred it);
//   - the full lattice: unverified/satisfied/unsatisfied/disputed/blocked/unknown;
//   - a forged/unsigned/detached receipt contributes nothing (fail-closed);
//   - evaluation is a PURE function of its versioned governed input -- identical
//     input yields an identical Outcome content hash (replay equality), and the
//     decision reads no wall clock, randomness or map order;
//   - a re-evaluation that changes the conclusion APPENDS a superseding revision;
//   - closure completes a WorkCase ONLY through a satisfied Outcome verified at
//     read time, idempotently.
//
// The signing helpers mirror nexus (Signature cleared, json.Marshal, ed25519),
// exactly the 0D/0E pattern -- an AgentAtlas-controlled test key, no live
// AgentNexus, no imported AgentNexus code.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	nexus "github.com/astraclawteam/agentatlas/sdk/go/nexus"
	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
)

const (
	evTenant    = "ent-a"
	evKeyID     = "nexus-signing-key-1"
	evAuthority = "erp-system-of-record"
	evOrg       = "org:team-a"
	evActor     = "actor-1"
	policyRev   = "outcome-policy-rev-7"
)

var evBase = time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

// --- signing helpers (mirror sdk/go/nexus signingPayload) -------------------

func evKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func signObs(priv ed25519.PrivateKey, keyID string, o nexus.ObservationReceipt) nexus.ObservationReceipt {
	o.Signature = nil
	payload, _ := json.Marshal(o)
	sig := ed25519.Sign(priv, payload)
	o.Signature = &nexus.Signature{Algorithm: nexus.SignatureAlgorithmEd25519, KeyID: keyID, Value: base64.StdEncoding.EncodeToString(sig)}
	return o
}

func signAct(priv ed25519.PrivateKey, keyID string, r nexus.ActionReceipt) nexus.ActionReceipt {
	r.Signature = nil
	payload, _ := json.Marshal(r)
	sig := ed25519.Sign(priv, payload)
	r.Signature = &nexus.Signature{Algorithm: nexus.SignatureAlgorithmEd25519, KeyID: keyID, Value: base64.StdEncoding.EncodeToString(sig)}
	return r
}

func evHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// actionReq builds a governed side-effecting request declaring one postcondition
// confirmed by one verification need with the given freshness bound.
func actionReq(needID, postID string, maxAge time.Duration) nexus.ActionRequest {
	need := nexus.VerificationNeed{NeedID: needID, Source: "erp.system", Authority: evAuthority, MaxAge: maxAge}
	return nexus.ActionRequest{
		ActionID:               "act-" + needID,
		WorkCaseID:             "case-42",
		Actor:                  evActor,
		OrgScope:               evOrg,
		Capability:             "erp.purchase_order.approve",
		ParameterHash:          evHash("params-" + needID),
		VerificationNeeds:      []nexus.VerificationNeed{need},
		Postconditions:         []nexus.PostconditionSpec{{PostconditionID: postID, Description: "side effect confirmed", VerificationNeedID: needID}},
		Risk:                   nexus.RiskLow,
		IdempotencyKey:         "idem-" + needID + "-00000000",
		ExpiresAt:              evBase.Add(time.Hour),
		ExecutionReceiptSchema: "test.receipt.v1",
	}
}

func obsReceipt(req nexus.ActionRequest, needID, postID, normHash string, observedAt time.Time) nexus.ObservationReceipt {
	return nexus.ObservationReceipt{
		ObservationID:             "obs-" + needID,
		ActionID:                  req.ActionID,
		ParameterHash:             req.ParameterHash,
		PostconditionID:           postID,
		VerificationNeedID:        needID,
		Source:                    "erp.system",
		Authority:                 evAuthority,
		ObservedAt:                observedAt,
		NormalizedObservationHash: normHash,
		EvidenceRef:               "evh-" + needID,
		AuditRef:                  "audit-" + needID,
	}
}

func actReceipt(req nexus.ActionRequest, result string) nexus.ActionReceipt {
	return nexus.ActionReceipt{
		ReceiptID:        "rcp-" + req.ActionID,
		ActionID:         req.ActionID,
		ParameterHash:    req.ParameterHash,
		ConnectorRef:     "connector-opaque",
		TargetRef:        "target-opaque",
		ExecutorIdentity: "svc-connector-7",
		ResultCode:       result,
		AuditRef:         "audit-" + req.ActionID,
		IssuedAt:         evBase,
	}
}

// oneNeedPolicy is the common published policy: a single required postcondition
// confirmed by an authoritative observation, optionally against an expected hash.
func oneNeedPolicy(needID, postID, expectedHash string) outcome.Policy {
	return outcome.Policy{Version: policyRev, Required: []outcome.RequiredPostcondition{
		{NeedID: needID, PostconditionID: postID, Authority: evAuthority, ExpectedObservationHash: expectedHash},
	}}
}

func baseCmd(pub ed25519.PublicKey, pol outcome.Policy) outcome.EvaluateCommand {
	return outcome.EvaluateCommand{
		Tenant: evTenant, OutcomeKey: "case-42-goal", Revision: 1,
		Goal:                model.GoalRef{Tenant: evTenant, GoalKey: "mes.close_anomaly", GoalVersion: 3},
		WorkCaseID:          "case-42",
		WorkCaseRevision:    5,
		WorkPlanRevision:    2,
		OperatingMapVersion: 4,
		OrgVersion:          1,
		Policy:              pol,
		DecidedAt:           evBase,
		TrustedKeyID:        evKeyID,
		TrustedKey:          pub,
		Contributions:       []model.ContributionRef{{ContributorID: "agent-planner", Kind: "method", Weight: 0.5}},
	}
}

func mustDecide(t *testing.T, cmd outcome.EvaluateCommand) model.Outcome {
	t.Helper()
	o, err := outcome.Decide(cmd)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return o
}

// --- lattice scenarios ------------------------------------------------------

func TestEvaluateSatisfiedRequiresFreshAuthoritativeObservation(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-30*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}
	cmd.Actions = []outcome.ActionInput{{Request: req, Receipt: signAct(priv, evKeyID, actReceipt(req, "succeeded"))}}

	o := mustDecide(t, cmd)
	if o.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("a fresh authoritative signed observation must satisfy the goal, got %q", o.Claim.Status)
	}
	// The satisfied Outcome MUST carry the authoritative observation (the store's
	// three-layer gate rejects a satisfied Outcome without one).
	if err := o.Validate(); err != nil {
		t.Fatalf("produced satisfied outcome must pass sdk Validate: %v", err)
	}
	authoritative := false
	for _, obs := range o.Claim.Observations {
		if obs.IsAuthoritative() {
			authoritative = true
		}
	}
	if !authoritative {
		t.Fatal("satisfied outcome must carry an authoritative observation ref")
	}
	if o.Claim.RuleVersion != policyRev {
		t.Fatalf("outcome must bind the published policy version as rule_version, got %q", o.Claim.RuleVersion)
	}
}

// action-succeeded / outcome-unsatisfied (constraint 3): the connector ran and
// signed a succeeded ActionReceipt, but the authoritative observation of the
// business postcondition refutes it (hash != expected) -> unsatisfied, NOT
// satisfied. Transport/technical success is never coerced into a business result.
func TestEvaluateActionSucceededButOutcomeUnsatisfied(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	// Expected "approved"; the authoritative observation reports "rejected".
	refuting := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("rejected"), evBase.Add(-5*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: refuting}}
	cmd.Actions = []outcome.ActionInput{{Request: req, Receipt: signAct(priv, evKeyID, actReceipt(req, "succeeded"))}}

	o := mustDecide(t, cmd)
	if o.Claim.Status != model.OutcomeUnsatisfied {
		t.Fatalf("a succeeded action whose authoritative postcondition is refuted must be unsatisfied, got %q", o.Claim.Status)
	}
}

// A succeeded ActionReceipt with NO observation at all is likewise never
// satisfied -- the required postcondition is unverified -> unknown.
func TestEvaluateActionReceiptAloneNeverSatisfies(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Actions = []outcome.ActionInput{{Request: req, Receipt: signAct(priv, evKeyID, actReceipt(req, "succeeded"))}}
	// no observations at all

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("an ActionReceipt alone must never satisfy an Outcome")
	}
	if o.Claim.Status != model.OutcomeUnknown {
		t.Fatalf("a required postcondition with no observation is unknown, got %q", o.Claim.Status)
	}
}

// freshness (constraint 4): an authentic, authoritative, signed observation that
// is OLDER than its need's MaxAge cannot satisfy.
func TestEvaluateStaleObservationIsNotSatisfied(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	stale := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-2*time.Hour)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: stale}}

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("a stale observation must not satisfy (freshness is enforced in 0H)")
	}
	if o.Claim.Status != model.OutcomeUnknown {
		t.Fatalf("a stale-only required postcondition is unknown, got %q", o.Claim.Status)
	}
}

func TestEvaluateConflictingAuthoritiesIsDisputed(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	a := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-10*time.Minute)))
	b := obsReceipt(req, "n1", "p1", evHash("rejected"), evBase.Add(-9*time.Minute))
	b.ObservationID = "obs-n1-b"
	b = signObs(priv, evKeyID, b)
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: a}, {Request: req, Receipt: b}}

	o := mustDecide(t, cmd)
	if o.Claim.Status != model.OutcomeDisputed {
		t.Fatalf("two fresh authoritative observations that disagree must be disputed, got %q", o.Claim.Status)
	}
}

func TestEvaluateMissingPostconditionIsUnknown(t *testing.T) {
	pub, _ := evKey(t)
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	// A required postcondition is declared but no observation supplied at all.
	o := mustDecide(t, cmd)
	if o.Claim.Status != model.OutcomeUnknown {
		t.Fatalf("a required postcondition with no evidence is unknown, got %q", o.Claim.Status)
	}
}

// result-unknown reconciliation / insufficient evidence: an observation exists
// but is NOT from the required authority (advisory only) -> cannot conclude.
func TestEvaluateInsufficientAuthorityIsUnknown(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	adv := obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-5*time.Minute))
	adv.Authority = "helpdesk-notes" // not the required system-of-record authority
	adv = signObs(priv, evKeyID, adv)
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: adv}}

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("a non-authoritative observation must not satisfy")
	}
	if o.Claim.Status != model.OutcomeUnknown {
		t.Fatalf("insufficient authoritative evidence is unknown, got %q", o.Claim.Status)
	}
}

func TestEvaluateExternalBlockerIsBlocked(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-5*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}
	cmd.Blockers = []model.BlockerRef{{Handle: "blk-1", Kind: "external_dependency", Authority: "erp-change-freeze", Summary: "ERP change freeze in effect"}}

	o := mustDecide(t, cmd)
	if o.Claim.Status != model.OutcomeBlocked {
		t.Fatalf("an external blocker must produce blocked (never a silent completion), got %q", o.Claim.Status)
	}
	if len(o.Claim.Blockers) != 1 {
		t.Fatalf("the blocked outcome must carry its blocker ref, got %d", len(o.Claim.Blockers))
	}
}

func TestEvaluateDisputedHumanStatementIsDisputed(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-5*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}
	cmd.HumanStatements = []outcome.HumanStatement{{Handle: "hs-1", Authority: "line-manager", Kind: "dispute", Disputed: true, Summary: "manager disputes the ERP result"}}

	o := mustDecide(t, cmd)
	if o.Claim.Status != model.OutcomeDisputed {
		t.Fatalf("a disputed human statement must produce disputed, got %q", o.Claim.Status)
	}
}

// compensation: a reversed side effect is authoritatively observed as NOT in the
// desired end state -> the Outcome is unsatisfied (the work was undone).
func TestEvaluateCompensationIsNotSatisfied(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	reverted := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("reverted"), evBase.Add(-2*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: reverted}}

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("a compensated (reverted) side effect must never be satisfied")
	}
	if o.Claim.Status != model.OutcomeUnsatisfied {
		t.Fatalf("a compensated postcondition observed as reverted is unsatisfied, got %q", o.Claim.Status)
	}
}

// forged receipt (constraint 8): an observation signed by the WRONG key is
// dropped by fail-closed verification and contributes nothing.
func TestEvaluateForgedReceiptNeverContributes(t *testing.T) {
	pub, _ := evKey(t)
	_, wrongPriv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	forged := signObs(wrongPriv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-5*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: forged}}

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("a forged/untrusted observation must never satisfy an Outcome (fail-closed)")
	}
	if o.Claim.Status != model.OutcomeUnknown {
		t.Fatalf("a forged observation is dropped, leaving the required need unknown, got %q", o.Claim.Status)
	}
	for _, obs := range o.Claim.Observations {
		if obs.ObservationHash == evHash("approved") {
			t.Fatal("a forged observation must not be carried into the Outcome")
		}
	}
}

// An unsigned observation is likewise rejected.
func TestEvaluateUnsignedReceiptNeverContributes(t *testing.T) {
	pub, _ := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	unsigned := obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-5*time.Minute)) // no Signature
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: unsigned}}

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("an unsigned observation must never satisfy an Outcome")
	}
}

// TestEvaluateFreshnessBoundaryInclusive pins the freshness comparison at the
// exact boundary (mutation guard for age <= bound vs age < bound): an
// observation exactly MaxAge old is fresh; one nanosecond older is stale.
func TestEvaluateFreshnessBoundaryInclusive(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	mk := func(observedAt time.Time) model.Outcome {
		obs := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), observedAt))
		c := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
		c.Observations = []outcome.ObservationInput{{Request: req, Receipt: obs}}
		return mustDecide(t, c)
	}
	if atBound := mk(evBase.Add(-time.Hour)); atBound.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("age == MaxAge must be fresh (inclusive boundary), got %q", atBound.Claim.Status)
	}
	if pastBound := mk(evBase.Add(-time.Hour - time.Nanosecond)); pastBound.Claim.Status == model.OutcomeSatisfied {
		t.Fatalf("age == MaxAge+1ns must be stale (not satisfied), got %q", pastBound.Claim.Status)
	}
}

// TestEvaluateAcceptingSetSatisfiesOnAnyMember proves the declared accepting
// SET: an observation matching ANY member confirms; one outside it refutes.
func TestEvaluateAcceptingSetSatisfiesOnAnyMember(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	pol := outcome.Policy{Version: policyRev, Required: []outcome.RequiredPostcondition{
		{NeedID: "n1", PostconditionID: "p1", Authority: evAuthority, AcceptingObservationHashes: []string{evHash("approved"), evHash("auto_approved")}},
	}}
	in := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("auto_approved"), evBase.Add(-5*time.Minute)))
	c := baseCmd(pub, pol)
	c.Observations = []outcome.ObservationInput{{Request: req, Receipt: in}}
	if o := mustDecide(t, c); o.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("an observation in the accepting set must satisfy, got %q", o.Claim.Status)
	}
	out := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("rejected"), evBase.Add(-5*time.Minute)))
	c2 := baseCmd(pub, pol)
	c2.Observations = []outcome.ObservationInput{{Request: req, Receipt: out}}
	if o := mustDecide(t, c2); o.Claim.Status != model.OutcomeUnsatisfied {
		t.Fatalf("an observation outside the accepting set must be unsatisfied, got %q", o.Claim.Status)
	}
}

// TestEvaluateZeroRequiredPostconditionsIsUnverified confirms the coverage floor:
// a plan with NO required postconditions can never be satisfied (at most
// unverified), even with a fresh authoritative observation present.
func TestEvaluateZeroRequiredPostconditionsIsUnverified(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	obs := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-5*time.Minute)))
	c := baseCmd(pub, outcome.Policy{Version: policyRev}) // zero required postconditions
	c.Observations = []outcome.ObservationInput{{Request: req, Receipt: obs}}
	if o := mustDecide(t, c); o.Claim.Status != model.OutcomeUnverified {
		t.Fatalf("zero required postconditions must floor to unverified (never satisfied), got %q", o.Claim.Status)
	}
}

// --- determinism / replay equality ------------------------------------------

func TestEvaluateReplayEqualityIdenticalContentHash(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-30*time.Minute)))
	build := func() outcome.EvaluateCommand {
		c := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
		c.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}
		return c
	}
	o1 := mustDecide(t, build())
	o2 := mustDecide(t, build())
	if outcome.ContentHash(o1) != outcome.ContentHash(o2) {
		t.Fatalf("identical versioned input must yield an identical content hash: %q vs %q", outcome.ContentHash(o1), outcome.ContentHash(o2))
	}
	if o1.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("setup: expected satisfied, got %q", o1.Claim.Status)
	}
}

// Determinism nuance (constraint 2): a different decided_at that does NOT flip
// any freshness verdict must not change the content hash; a decided_at that DOES
// flip freshness legitimately changes the decision (and thus the hash).
func TestEvaluateDecidedAtDoesNotAlterContentHashUnlessFreshnessFlips(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	obs := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-30*time.Minute)))
	mk := func(decidedAt time.Time) model.Outcome {
		c := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
		c.DecidedAt = decidedAt
		c.Observations = []outcome.ObservationInput{{Request: req, Receipt: obs}}
		return mustDecide(t, c)
	}
	// Both within MaxAge (fresh): identical content hash despite different decided_at.
	early := mk(evBase)
	later := mk(evBase.Add(20 * time.Minute))
	if early.Claim.Status != model.OutcomeSatisfied || later.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("both decisions should be satisfied, got %q and %q", early.Claim.Status, later.Claim.Status)
	}
	if outcome.ContentHash(early) != outcome.ContentHash(later) {
		t.Fatal("decided_at alone (freshness unchanged) must not change the content hash")
	}
	// Far enough that the observation is now stale: the decision legitimately flips.
	flipped := mk(evBase.Add(2 * time.Hour))
	if flipped.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("once the observation is stale the decision must not be satisfied")
	}
	if outcome.ContentHash(flipped) == outcome.ContentHash(early) {
		t.Fatal("a decision that legitimately flipped must have a different content hash")
	}
}

// --- confidence components (constraint 6) -----------------------------------

func TestEvaluateConfidenceComponentsNamedBoundedAndNonDeciding(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-30*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}

	o := mustDecide(t, cmd)
	want := map[string]bool{
		outcome.ConfidenceAuthority:   false,
		outcome.ConfidenceFreshness:   false,
		outcome.ConfidenceCoverage:    false,
		outcome.ConfidenceConsistency: false,
		outcome.ConfidenceAttribution: false,
	}
	for _, c := range o.Claim.Confidence {
		if _, ok := want[c.Kind]; ok {
			want[c.Kind] = true
		}
		if c.Score < 0 || c.Score > 1 {
			t.Fatalf("confidence component %q score %v out of [0,1]", c.Kind, c.Score)
		}
	}
	for k, seen := range want {
		if !seen {
			t.Fatalf("named confidence component %q must be present", k)
		}
	}
}

// --- correction / supersession / revocation ---------------------------------

// A re-evaluation that CHANGES the conclusion must append a NEW superseding
// revision (never mutate a terminal Outcome). Revision N>1 must name revision N-1.
func TestEvaluateCorrectionAppendsSupersedingRevision(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	ev, err := outcome.NewEvaluator(store)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	ctx := context.Background()

	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-30*time.Minute)))
	c1 := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	c1.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}
	first, err := ev.Evaluate(ctx, c1)
	if err != nil {
		t.Fatalf("Evaluate rev1: %v", err)
	}
	if first.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("rev1 should be satisfied, got %q", first.Claim.Status)
	}

	// Evidence revocation: the satisfying observation is revoked -> re-evaluate.
	c2 := c1
	c2.Revision = 2
	c2.RevokedObservationHandles = []string{fresh.ObservationID}
	c2.Supersedes = &model.OutcomeRevisionRef{
		OutcomeKey: first.OutcomeKey, Revision: 1, OutcomeID: first.ID, Reason: "evidence revoked",
	}
	second, err := ev.Evaluate(ctx, c2)
	if err != nil {
		t.Fatalf("Evaluate rev2 (revocation): %v", err)
	}
	if second.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("after its evidence is revoked the re-evaluation must NOT remain satisfied")
	}
	if second.Revision != 2 || second.Supersedes == nil || second.Supersedes.Revision != 1 {
		t.Fatalf("the correction must be revision 2 superseding revision 1, got rev=%d supersedes=%+v", second.Revision, second.Supersedes)
	}
	// History preserved: revision 1 is still readable and still satisfied.
	back, err := store.GetOutcome(ctx, evTenant, first.OutcomeKey, 1)
	if err != nil || back.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("prior satisfied revision must remain immutable and readable: %+v err=%v", back, err)
	}
}

// --- persistence ------------------------------------------------------------

func TestEvaluatePersistsProducedOutcome(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	ev, err := outcome.NewEvaluator(store)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	ctx := context.Background()
	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-30*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}

	got, err := ev.Evaluate(ctx, cmd)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	latest, err := store.LatestOutcome(ctx, evTenant, cmd.OutcomeKey)
	if err != nil {
		t.Fatalf("LatestOutcome: %v", err)
	}
	if latest.ID != got.ID || latest.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("Evaluate must persist the produced Outcome: latest=%+v produced=%+v", latest, got)
	}
}

// --- closure ----------------------------------------------------------------

var errFakeStale = errors.New("fake closer: stale revision")

// fakeCloser is a minimal CaseCloser modelling optimistic-concurrency +
// idempotent completion of one WorkCase. It is the ONLY writer of the completed
// status in these tests.
type fakeCloser struct {
	mu            sync.Mutex
	rev           uint64
	status        sdkworkcase.Status
	outcomeRef    string
	completeCalls int
	idem          map[string]sdkworkcase.WorkCase
}

func newFakeCloser(startRev uint64) *fakeCloser {
	return &fakeCloser{rev: startRev, status: sdkworkcase.StatusExecuting, idem: map[string]sdkworkcase.WorkCase{}}
}

func (f *fakeCloser) snapshot(caseID string) sdkworkcase.WorkCase {
	return sdkworkcase.WorkCase{ID: caseID, EnterpriseID: evTenant, OrgScope: evOrg, ActorRef: evActor, Status: f.status, Revision: f.rev}
}

func (f *fakeCloser) CompleteCase(_ context.Context, enterpriseID, orgScope, actorRef, caseID string, expectedRevision uint64, idempotencyKey, outcomeRef string) (sdkworkcase.WorkCase, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if wc, ok := f.idem[idempotencyKey]; ok {
		return wc, nil // idempotent replay
	}
	if f.status == sdkworkcase.StatusCompleted {
		return f.snapshot(caseID), nil // already completed via another key: benign no-op
	}
	if expectedRevision != f.rev {
		return sdkworkcase.WorkCase{}, errFakeStale
	}
	f.rev++
	f.status = sdkworkcase.StatusCompleted
	f.outcomeRef = outcomeRef
	f.completeCalls++
	wc := f.snapshot(caseID)
	f.idem[idempotencyKey] = wc
	return wc, nil
}

// persist a satisfied Outcome and return its ref for closure.
func persistOutcome(t *testing.T, store outcome.Store, pub ed25519.PublicKey, priv ed25519.PrivateKey, status model.OutcomeStatus) model.Outcome {
	t.Helper()
	ev, err := outcome.NewEvaluator(store)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	req := actionReq("n1", "p1", time.Hour)
	// The accepting definition ("approved") is always declared; satisfaction is
	// reached by CONFIRMATION (a matching observation), refutation by a mismatch.
	hash := evHash("approved")
	if status == model.OutcomeUnsatisfied {
		hash = evHash("rejected")
	}
	obs := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", hash, evBase.Add(-10*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: obs}}
	o, err := ev.Evaluate(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if o.Claim.Status != status {
		t.Fatalf("setup: wanted %q, got %q", status, o.Claim.Status)
	}
	return o
}

func TestOutcomeClosureCompletesOnlyOnSatisfied(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	o := persistOutcome(t, store, pub, priv, model.OutcomeSatisfied)

	closer := newFakeCloser(5)
	cs, err := outcome.NewClosureService(store, closer)
	if err != nil {
		t.Fatalf("NewClosureService: %v", err)
	}
	ref := outcome.OutcomeRef{Tenant: evTenant, OutcomeKey: o.OutcomeKey, Revision: o.Revision, OrgScope: evOrg, ActorRef: evActor, CaseID: o.WorkCaseID}
	res, err := cs.Apply(context.Background(), ref, 5, "closure-key-0000001")
	if err != nil {
		t.Fatalf("Apply on satisfied: %v", err)
	}
	if !res.Completed || res.Case.Status != sdkworkcase.StatusCompleted {
		t.Fatalf("a satisfied Outcome must complete the WorkCase, got %+v", res)
	}
	if closer.completeCalls != 1 {
		t.Fatalf("expected exactly one completion, got %d", closer.completeCalls)
	}
}

func TestOutcomeClosureRefusesNonSatisfied(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	o := persistOutcome(t, store, pub, priv, model.OutcomeUnsatisfied)

	closer := newFakeCloser(5)
	cs, _ := outcome.NewClosureService(store, closer)
	ref := outcome.OutcomeRef{Tenant: evTenant, OutcomeKey: o.OutcomeKey, Revision: o.Revision, OrgScope: evOrg, ActorRef: evActor, CaseID: o.WorkCaseID}
	res, err := cs.Apply(context.Background(), ref, 5, "closure-key-0000002")
	if err == nil {
		t.Fatal("closure of a non-satisfied Outcome must be refused")
	}
	if res.Completed || closer.completeCalls != 0 {
		t.Fatalf("a non-satisfied Outcome must NEVER complete a case: completeCalls=%d", closer.completeCalls)
	}
}

// The closure NEVER trusts the caller's word: it re-reads the referenced Outcome
// and verifies its status is satisfied at read time.
func TestOutcomeClosureVerifiesStatusAtReadTime(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	o := persistOutcome(t, store, pub, priv, model.OutcomeUnsatisfied)
	closer := newFakeCloser(5)
	cs, _ := outcome.NewClosureService(store, closer)
	// A ref that (dishonestly) points at the unsatisfied revision must still be refused.
	ref := outcome.OutcomeRef{Tenant: evTenant, OutcomeKey: o.OutcomeKey, Revision: o.Revision, OrgScope: evOrg, ActorRef: evActor, CaseID: o.WorkCaseID}
	if _, err := cs.Apply(context.Background(), ref, 5, "closure-key-0000003"); err == nil {
		t.Fatal("closure must read the Outcome and refuse a non-satisfied status regardless of the caller")
	}
}

func TestOutcomeClosureIdempotentDuplicateApply(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	o := persistOutcome(t, store, pub, priv, model.OutcomeSatisfied)
	closer := newFakeCloser(5)
	cs, _ := outcome.NewClosureService(store, closer)
	ref := outcome.OutcomeRef{Tenant: evTenant, OutcomeKey: o.OutcomeKey, Revision: o.Revision, OrgScope: evOrg, ActorRef: evActor, CaseID: o.WorkCaseID}

	// Same idempotency key twice (a redelivered closure): exactly one completion.
	for i := 0; i < 2; i++ {
		if _, err := cs.Apply(context.Background(), ref, 5, "closure-key-dup-00001"); err != nil {
			t.Fatalf("Apply %d: %v", i, err)
		}
	}
	if closer.completeCalls != 1 {
		t.Fatalf("a duplicate/redelivered closure must transition exactly once, got %d", closer.completeCalls)
	}
}

// --- post-review: fail-closed satisfaction, freshness, superseded head ------

// BLOCKER (RED): an ObservationReceipt attests an observed STATE (its hash); it
// does NOT self-declare confirmation. When the policy declares NO accepting
// definition, a fresh, signed, authoritative observation -- even one REFUTING
// the postcondition -- must NOT satisfy (observation presence != confirmation).
func TestEvaluateRefutingObservationWithoutAcceptingDefinitionIsNotSatisfied(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", time.Hour)
	// The authoritative ERP observes the postcondition as FAILED ("rejected").
	refuting := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("rejected"), evBase.Add(-5*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", "")) // NO accepting definition declared
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: refuting}}

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("FAIL-OPEN: a refuting observation with no accepting definition was treated as satisfied")
	}
	if o.Claim.Status != model.OutcomeUnknown {
		t.Fatalf("an unconfirmable postcondition (no accepting definition) must be unknown, got %q", o.Claim.Status)
	}
}

// MINOR (RED): an unbounded freshness (need MaxAge==0 AND receipt
// FreshnessWindow==0) must NOT yield satisfied even with an accepting definition
// and a matching observation -- an unenforceable freshness bound is fail-closed.
func TestEvaluateUnboundedFreshnessCannotSatisfy(t *testing.T) {
	pub, priv := evKey(t)
	req := actionReq("n1", "p1", 0) // MaxAge == 0 -> no enforceable freshness bound
	obs := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-5*time.Minute)))
	cmd := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved"))) // accepting definition declared
	cmd.Observations = []outcome.ObservationInput{{Request: req, Receipt: obs}}

	o := mustDecide(t, cmd)
	if o.Claim.Status == model.OutcomeSatisfied {
		t.Fatal("FAIL-OPEN: an observation with no enforceable freshness bound was treated as satisfied")
	}
}

// MAJOR (RED): a satisfied revision that a correction has SUPERSEDED with an
// unsatisfied conclusion must not close the case; closure requires the current
// satisfied head, not merely an immutable revision that was once satisfied.
func TestOutcomeClosureSupersededHeadRefused(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	ev, _ := outcome.NewEvaluator(store)
	ctx := context.Background()

	req := actionReq("n1", "p1", time.Hour)
	fresh := signObs(priv, evKeyID, obsReceipt(req, "n1", "p1", evHash("approved"), evBase.Add(-10*time.Minute)))
	c1 := baseCmd(pub, oneNeedPolicy("n1", "p1", evHash("approved")))
	c1.Observations = []outcome.ObservationInput{{Request: req, Receipt: fresh}}
	rev1, err := ev.Evaluate(ctx, c1)
	if err != nil || rev1.Claim.Status != model.OutcomeSatisfied {
		t.Fatalf("setup rev1 must be satisfied: status=%q err=%v", rev1.Claim.Status, err)
	}

	// A correction supersedes rev1 with an unsatisfied conclusion (evidence revoked).
	c2 := c1
	c2.Revision = 2
	c2.RevokedObservationHandles = []string{fresh.ObservationID}
	c2.Supersedes = &model.OutcomeRevisionRef{OutcomeKey: rev1.OutcomeKey, Revision: 1, OutcomeID: rev1.ID, Reason: "evidence revoked"}
	rev2, err := ev.Evaluate(ctx, c2)
	if err != nil || rev2.Claim.Status == model.OutcomeSatisfied {
		t.Fatalf("setup rev2 must NOT be satisfied: status=%q err=%v", rev2.Claim.Status, err)
	}

	closer := newFakeCloser(5)
	cs, _ := outcome.NewClosureService(store, closer)
	// A closure command minted while rev1 (satisfied) was head, delivered AFTER
	// rev2 (unsatisfied) supersedes it, must be refused.
	ref := outcome.OutcomeRef{Tenant: evTenant, OutcomeKey: rev1.OutcomeKey, Revision: 1, OrgScope: evOrg, ActorRef: evActor, CaseID: rev1.WorkCaseID}
	if _, err := cs.Apply(ctx, ref, 5, "closure-superseded-001"); err == nil {
		t.Fatal("closing a SUPERSEDED satisfied revision must be refused (closure requires the current satisfied head)")
	}
	if closer.completeCalls != 0 {
		t.Fatal("a superseded revision must never complete the case")
	}
}

func TestOutcomeClosureMismatchedCaseRefused(t *testing.T) {
	pub, priv := evKey(t)
	store := outcome.NewMemoryStore(func() time.Time { return evBase })
	o := persistOutcome(t, store, pub, priv, model.OutcomeSatisfied)
	closer := newFakeCloser(5)
	cs, _ := outcome.NewClosureService(store, closer)
	// The ref names a different case than the Outcome binds.
	ref := outcome.OutcomeRef{Tenant: evTenant, OutcomeKey: o.OutcomeKey, Revision: o.Revision, OrgScope: evOrg, ActorRef: evActor, CaseID: "some-other-case"}
	if _, err := cs.Apply(context.Background(), ref, 5, "closure-key-0000004"); err == nil {
		t.Fatal("closure must refuse when the ref's case does not match the Outcome's work_case_id")
	}
	if closer.completeCalls != 0 {
		t.Fatal("a mismatched case must never complete")
	}
}
