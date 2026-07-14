package nexus

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// --- fixtures ---------------------------------------------------------

func validActionRequest() ActionRequest {
	return ActionRequest{
		ActionID:         "act-0001",
		GoalRef:          "goal-0001",
		OutcomeRef:       "outcome-0001",
		WorkCaseID:       "case-0001",
		WorkPlanRevision: 3,
		Actor:            "actor-alice",
		OrgScope:         "org-1",
		Capability:       "erp.purchase_order.approve",
		ParameterHash:    "sha256:" + hex64('a'),
		Preconditions: []Precondition{
			{Kind: "state_hash", Reference: "po-100", Expected: "sha256:" + hex64('b')},
		},
		Postconditions: []PostconditionSpec{
			{PostconditionID: "post-0001", Description: "PO 100 status is approved", VerificationNeedID: "need-0001"},
		},
		VerificationNeeds: []VerificationNeed{
			{NeedID: "need-0001", Source: "erp.purchase_order", Authority: "erp-system-of-record", MaxAge: time.Hour},
		},
		Risk:                   RiskMedium,
		ApprovalRefs:           []string{"apl_fixture0000000001"},
		IdempotencyKey:         "idem-key-0001-abcdefgh",
		ExpiresAt:              time.Now().Add(time.Hour),
		ExecutionReceiptSchema: "erp.purchase_order.approve.v1",
		CompensationRef:        "erp.purchase_order.revert_approval",
	}
}

func hex64(b byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

func newTrustedKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

func signPayload(t *testing.T, priv ed25519.PrivateKey, keyID string, payload []byte) *Signature {
	t.Helper()
	sig := ed25519.Sign(priv, payload)
	return &Signature{
		Algorithm: SignatureAlgorithmEd25519,
		KeyID:     keyID,
		Value:     base64.StdEncoding.EncodeToString(sig),
	}
}

func validActionReceipt(t *testing.T, req ActionRequest, priv ed25519.PrivateKey, keyID string) ActionReceipt {
	t.Helper()
	r := ActionReceipt{
		ReceiptID:        "rcp-0001",
		ActionID:         req.ActionID,
		ParameterHash:    req.ParameterHash,
		ConnectorRef:     "connector-ref-opaque",
		TargetRef:        "target-ref-opaque",
		BeforeHash:       "sha256:" + hex64('c'),
		AfterHash:        "sha256:" + hex64('d'),
		ExecutorIdentity: "svc-connector-worker-7",
		ResultCode:       "succeeded",
		ExternalReceipt:  []byte(`{"erp_document_number":"PO-100-A"}`),
		AuditRef:         "audit-ref-0001",
		IssuedAt:         time.Now(),
	}
	r.Signature = signPayload(t, priv, keyID, r.signingPayload())
	return r
}

func validObservationReceipt(t *testing.T, req ActionRequest, priv ed25519.PrivateKey, keyID string) ObservationReceipt {
	t.Helper()
	o := ObservationReceipt{
		ObservationID:             "obs-0001",
		ActionID:                  req.ActionID,
		ParameterHash:             req.ParameterHash,
		PostconditionID:           req.Postconditions[0].PostconditionID,
		VerificationNeedID:        req.VerificationNeeds[0].NeedID,
		Source:                    req.VerificationNeeds[0].Source,
		SourceVersion:             "v2026.1",
		Authority:                 req.VerificationNeeds[0].Authority,
		ObservedAt:                time.Now(),
		FreshnessWindow:           5 * time.Minute,
		NormalizedObservationHash: "sha256:" + hex64('e'),
		EvidenceRef:               "evd_fixture0000000001",
		AuditRef:                  "audit-ref-0002",
	}
	o.Signature = signPayload(t, priv, keyID, o.signingPayload())
	return o
}

func validActionGrant(req ActionRequest) ActionGrant {
	return ActionGrant{
		GrantID:       "grant-0001",
		ActionID:      req.ActionID,
		WorkCaseID:    req.WorkCaseID,
		Actor:         req.Actor,
		OrgScope:      req.OrgScope,
		Capability:    req.Capability,
		ParameterHash: req.ParameterHash,
		OneUse:        true,
		IssuedAt:      time.Now(),
		ExpiresAt:     time.Now().Add(time.Hour),
	}
}

// --- ActionRequest.Validate ---------------------------------------------

func TestActionRequestValidateAcceptsWellFormedRequest(t *testing.T) {
	if err := validActionRequest().Validate(); err != nil {
		t.Fatalf("well-formed ActionRequest rejected: %v", err)
	}
}

func TestActionRequestValidateRejectsMissingIdempotencyKey(t *testing.T) {
	req := validActionRequest()
	req.IdempotencyKey = ""
	if err := req.Validate(); err == nil {
		t.Fatal("ActionRequest with no idempotency key was accepted")
	}
}

func TestActionRequestValidateRejectsShortIdempotencyKey(t *testing.T) {
	req := validActionRequest()
	req.IdempotencyKey = "short"
	if err := req.Validate(); err == nil {
		t.Fatal("ActionRequest with a too-short idempotency key was accepted")
	}
}

func TestActionRequestValidateRejectsMissingExpiry(t *testing.T) {
	req := validActionRequest()
	req.ExpiresAt = time.Time{}
	if err := req.Validate(); err == nil {
		t.Fatal("ActionRequest with zero expiry was accepted")
	}
}

func TestActionRequestValidateRejectsMalformedParameterHash(t *testing.T) {
	req := validActionRequest()
	req.ParameterHash = "not-a-hash"
	if err := req.Validate(); err == nil {
		t.Fatal("ActionRequest with a malformed parameter_hash was accepted")
	}
}

func TestActionRequestValidateRejectsMissingActorOrOrgScope(t *testing.T) {
	base := validActionRequest()

	noActor := base
	noActor.Actor = ""
	if err := noActor.Validate(); err == nil {
		t.Fatal("ActionRequest with no actor was accepted")
	}

	noOrg := base
	noOrg.OrgScope = ""
	if err := noOrg.Validate(); err == nil {
		t.Fatal("ActionRequest with no org_scope was accepted")
	}
}

func TestActionRequestValidateRejectsMissingWorkCase(t *testing.T) {
	req := validActionRequest()
	req.WorkCaseID = ""
	if err := req.Validate(); err == nil {
		t.Fatal("ActionRequest with no work_case_id was accepted")
	}
}

func TestActionRequestValidateRejectsInvalidRisk(t *testing.T) {
	req := validActionRequest()
	req.Risk = "extreme"
	if err := req.Validate(); err == nil {
		t.Fatal("ActionRequest with an unrecognized risk level was accepted")
	}
}

func TestActionRequestValidateRejectsDetachedPostconditionVerificationNeed(t *testing.T) {
	req := validActionRequest()
	req.Postconditions = []PostconditionSpec{
		{PostconditionID: "post-0001", Description: "orphaned", VerificationNeedID: "need-does-not-exist"},
	}
	if err := req.Validate(); err == nil {
		t.Fatal("ActionRequest with a postcondition referencing an undeclared verification need was accepted")
	}
}

// --- ActionStatus ---------------------------------------------------------

func TestActionStatusValid(t *testing.T) {
	for _, s := range ActionStatuses() {
		if !s.Valid() {
			t.Errorf("frozen status %q reported invalid", s)
		}
	}
	if ActionStatus("bogus").Valid() {
		t.Fatal("unrecognized ActionStatus reported valid")
	}
}

// --- ActionReceipt / VerifyActionReceipt -----------------------------------

func TestActionReceiptVerifyAcceptsValidSigned(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")

	if err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub); err != nil {
		t.Fatalf("valid signed ActionReceipt rejected: %v", err)
	}
}

func TestActionReceiptVerifyRejectsUnsigned(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")
	receipt.Signature = nil

	err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub)
	if err == nil {
		t.Fatal("unsigned ActionReceipt accepted")
	}
	if !errors.Is(err, ErrUnsignedReceipt) {
		t.Fatalf("unsigned ActionReceipt rejected for the wrong reason: %v", err)
	}
}

func TestActionReceiptVerifyRejectsWrongSigningKey(t *testing.T) {
	pub, _ := newTrustedKey(t)
	_, wrongPriv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, wrongPriv, "nexus-signing-key-1")

	err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub)
	if err == nil {
		t.Fatal("ActionReceipt signed by an untrusted key was accepted")
	}
	if !errors.Is(err, ErrUntrustedSignature) {
		t.Fatalf("wrong-key ActionReceipt rejected for the wrong reason: %v", err)
	}
}

func TestActionReceiptVerifyRejectsUnknownKeyID(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "some-other-key-id")

	err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub)
	if err == nil {
		t.Fatal("ActionReceipt signed under an unexpected key_id was accepted")
	}
	if !errors.Is(err, ErrUntrustedSignature) {
		t.Fatalf("unexpected key_id rejected for the wrong reason: %v", err)
	}
}

func TestActionReceiptVerifyRejectsChangedParameterHash(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")
	// Tamper the parameter hash and RE-SIGN over the tampered payload with
	// the valid key, so the receipt is validly signed over its own bytes
	// and the ONLY thing wrong is that its parameter_hash no longer matches
	// the ActionRequest. This isolates the binding check: the signature
	// verifies, so only the receipt.ParameterHash != req.ParameterHash guard
	// can reject this receipt — deleting that guard would make this test
	// fail (whereas tampering without re-signing would let the signature
	// check shadow the binding check).
	receipt.ParameterHash = "sha256:" + hex64('9')
	receipt.Signature = signPayload(t, priv, "nexus-signing-key-1", receipt.signingPayload())

	err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub)
	if err == nil {
		t.Fatal("ActionReceipt with a changed parameter_hash was accepted")
	}
}

func TestActionReceiptVerifyRejectsDetachedAction(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")

	otherReq := validActionRequest()
	otherReq.ActionID = "act-9999-different"

	err := VerifyActionReceipt(otherReq, receipt, "nexus-signing-key-1", pub)
	if err == nil {
		t.Fatal("ActionReceipt detached from its Action was accepted")
	}
}

func TestActionReceiptNeverConfirmsOutcome(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")

	// Even a receipt that VerifyActionReceipt accepts as a technically
	// valid, correctly bound execution proof must never be usable as
	// business Outcome evidence.
	if err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub); err != nil {
		t.Fatalf("setup: valid receipt rejected: %v", err)
	}
	err := receipt.ConfirmsOutcome()
	if err == nil {
		t.Fatal("ActionReceipt.ConfirmsOutcome() must never return nil: a technical execution receipt is not a business Outcome assertion")
	}
	if !errors.Is(err, ErrReceiptNotOutcomeEvidence) {
		t.Fatalf("ConfirmsOutcome rejected for the wrong reason: %v", err)
	}
}

// --- verifySignature reject branches (via VerifyActionReceipt) -------------

func TestActionReceiptVerifyRejectsWrongAlgorithm(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")
	// A signature whose algorithm is not the frozen v1 ed25519 must fail
	// closed even if the signature bytes themselves would verify. The
	// algorithm is not part of signingPayload (Signature is cleared before
	// marshaling), so mutating it here does not invalidate the bytes — this
	// isolates the algorithm guard.
	receipt.Signature.Algorithm = "ecdsa-p256"
	err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub)
	if !errors.Is(err, ErrUntrustedSignature) {
		t.Fatalf("wrong-algorithm signature rejected for the wrong reason: %v", err)
	}
}

func TestActionReceiptVerifyRejectsInvalidBase64Signature(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")
	// A signature value that is not valid base64 must fail closed. ('!' is
	// not a base64 alphabet character, so DecodeString errors before any
	// verify attempt; even if the base64-error guard were removed, the
	// final ed25519.Verify over empty/garbage bytes would still reject —
	// defense in depth.)
	receipt.Signature.Value = "!!!not-valid-base64!!!"
	err := VerifyActionReceipt(req, receipt, "nexus-signing-key-1", pub)
	if !errors.Is(err, ErrUntrustedSignature) {
		t.Fatalf("invalid-base64 signature rejected for the wrong reason: %v", err)
	}
}

// --- external_receipt round-trip (json.RawMessage canonicalization) --------

func TestActionReceiptVerifyRoundTripsNonEmptyExternalReceipt(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	receipt := validActionReceipt(t, req, priv, "nexus-signing-key-1")
	// validActionReceipt already sets a non-empty ExternalReceipt; make the
	// round-trip explicit and non-trivial (a nested object whose bytes must
	// survive decode -> re-marshal unchanged for the signature to verify).
	receipt.ExternalReceipt = json.RawMessage(`{"erp_document_number":"PO-100-A","lines":[{"n":1},{"n":2}]}`)
	receipt.Signature = signPayload(t, priv, "nexus-signing-key-1", receipt.signingPayload())

	// Simulate the wire path: marshal the signed receipt, decode it back
	// into a fresh struct (as an HTTP client would), then verify. If the
	// json.RawMessage external_receipt bytes did not survive the
	// decode -> re-marshal (signingPayload) round-trip byte-for-byte, the
	// signature would fail.
	wire, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal signed receipt: %v", err)
	}
	var decoded ActionReceipt
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("decode wire receipt: %v", err)
	}
	if err := VerifyActionReceipt(req, decoded, "nexus-signing-key-1", pub); err != nil {
		t.Fatalf("valid receipt with a non-empty external_receipt failed to round-trip: %v", err)
	}
}

// --- ObservationReceipt / VerifyObservationReceipt -------------------------
// Named TestActionObservation* (not TestObservation*) so the canonical
// `-run TestAction` filter also captures the ObservationReceipt half of the
// protocol: an ObservationReceipt is meaningless detached from its Action.

func TestActionObservationReceiptVerifyAcceptsValidSigned(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, priv, "nexus-signing-key-1")

	if err := VerifyObservationReceipt(req, obs, "nexus-signing-key-1", pub); err != nil {
		t.Fatalf("valid signed ObservationReceipt rejected: %v", err)
	}
}

func TestActionObservationReceiptVerifyRejectsUnsigned(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, priv, "nexus-signing-key-1")
	obs.Signature = nil

	err := VerifyObservationReceipt(req, obs, "nexus-signing-key-1", pub)
	if !errors.Is(err, ErrUnsignedReceipt) {
		t.Fatalf("unsigned ObservationReceipt rejected for the wrong reason: %v", err)
	}
}

func TestActionObservationReceiptVerifyRejectsWrongSigningKey(t *testing.T) {
	pub, _ := newTrustedKey(t)
	_, wrongPriv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, wrongPriv, "nexus-signing-key-1")

	err := VerifyObservationReceipt(req, obs, "nexus-signing-key-1", pub)
	if !errors.Is(err, ErrUntrustedSignature) {
		t.Fatalf("wrong-key ObservationReceipt rejected for the wrong reason: %v", err)
	}
}

func TestActionObservationReceiptVerifyRejectsChangedParameterHash(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, priv, "nexus-signing-key-1")
	// Tamper then RE-SIGN over the tampered payload so ONLY the
	// parameter_hash binding mismatch can reject this receipt (see
	// TestActionReceiptVerifyRejectsChangedParameterHash for the rationale).
	obs.ParameterHash = "sha256:" + hex64('9')
	obs.Signature = signPayload(t, priv, "nexus-signing-key-1", obs.signingPayload())

	if err := VerifyObservationReceipt(req, obs, "nexus-signing-key-1", pub); err == nil {
		t.Fatal("ObservationReceipt with a changed parameter_hash was accepted")
	}
}

func TestActionObservationReceiptVerifyRejectsDetachedFromAction(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, priv, "nexus-signing-key-1")

	otherReq := validActionRequest()
	otherReq.ActionID = "act-9999-different"

	if err := VerifyObservationReceipt(otherReq, obs, "nexus-signing-key-1", pub); err == nil {
		t.Fatal("ObservationReceipt detached from its Action was accepted")
	}
}

func TestActionObservationReceiptVerifyRejectsDetachedFromPostconditionSpec(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, priv, "nexus-signing-key-1")
	obs.PostconditionID = "post-does-not-exist"
	// Re-sign so the tamper is isolated to the binding check, not the
	// signature check (this proves detachment is rejected even for an
	// otherwise validly signed payload).
	obs.Signature = signPayload(t, priv, "nexus-signing-key-1", obs.signingPayload())

	if err := VerifyObservationReceipt(req, obs, "nexus-signing-key-1", pub); err == nil {
		t.Fatal("ObservationReceipt detached from its PostconditionSpec was accepted")
	}
}

func TestActionObservationReceiptVerifyRejectsUndeclaredVerificationNeed(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, priv, "nexus-signing-key-1")
	obs.VerificationNeedID = "need-does-not-exist"
	obs.Signature = signPayload(t, priv, "nexus-signing-key-1", obs.signingPayload())

	if err := VerifyObservationReceipt(req, obs, "nexus-signing-key-1", pub); err == nil {
		t.Fatal("ObservationReceipt referencing an undeclared VerificationNeed was accepted")
	}
}

func TestActionObservationReceiptNeverConfirmsOutcome(t *testing.T) {
	pub, priv := newTrustedKey(t)
	req := validActionRequest()
	obs := validObservationReceipt(t, req, priv, "nexus-signing-key-1")
	if err := VerifyObservationReceipt(req, obs, "nexus-signing-key-1", pub); err != nil {
		t.Fatalf("setup: valid observation receipt rejected: %v", err)
	}
	if err := obs.ConfirmsOutcome(); !errors.Is(err, ErrReceiptNotOutcomeEvidence) {
		t.Fatalf("ObservationReceipt.ConfirmsOutcome() must always fail (0D scope; Outcome closure is Task 0G/0H), got: %v", err)
	}
}

// --- ActionGrant / VerifyActionGrant ---------------------------------------

func TestActionGrantVerifyAcceptsValid(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	if err := VerifyActionGrant(req, grant, NewGrantUsageTracker(), time.Now()); err != nil {
		t.Fatalf("valid ActionGrant rejected: %v", err)
	}
}

func TestActionGrantVerifyRejectsExpired(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	grant.IssuedAt = time.Now().Add(-2 * time.Hour)
	grant.ExpiresAt = time.Now().Add(-time.Hour)

	err := VerifyActionGrant(req, grant, NewGrantUsageTracker(), time.Now())
	if !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("expired ActionGrant rejected for the wrong reason: %v", err)
	}
}

func TestActionGrantVerifyRejectsReplay(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	tracker := NewGrantUsageTracker()

	if err := VerifyActionGrant(req, grant, tracker, time.Now()); err != nil {
		t.Fatalf("first use of a fresh grant rejected: %v", err)
	}
	err := VerifyActionGrant(req, grant, tracker, time.Now())
	if err == nil {
		t.Fatal("replayed ActionGrant (same grant_id consumed twice) was accepted")
	}
	if !errors.Is(err, ErrGrantReplayed) {
		t.Fatalf("replayed ActionGrant rejected for the wrong reason: %v", err)
	}
}

func TestActionGrantVerifyRejectsMismatchedOrgActor(t *testing.T) {
	req := validActionRequest()

	wrongActor := validActionGrant(req)
	wrongActor.Actor = "actor-mallory"
	if err := VerifyActionGrant(req, wrongActor, NewGrantUsageTracker(), time.Now()); !errors.Is(err, ErrActorOrgMismatch) {
		t.Fatalf("ActionGrant with a mismatched actor rejected for the wrong reason: %v", err)
	}

	wrongOrg := validActionGrant(req)
	wrongOrg.OrgScope = "org-intruder"
	if err := VerifyActionGrant(req, wrongOrg, NewGrantUsageTracker(), time.Now()); !errors.Is(err, ErrActorOrgMismatch) {
		t.Fatalf("ActionGrant with a mismatched org_scope rejected for the wrong reason: %v", err)
	}
}

func TestActionGrantVerifyRejectsNotOneUse(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	grant.OneUse = false
	if err := VerifyActionGrant(req, grant, NewGrantUsageTracker(), time.Now()); err == nil {
		t.Fatal("non-one-use ActionGrant was accepted")
	}
}

func TestActionGrantVerifyRejectsChangedParameterHash(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	grant.ParameterHash = "sha256:" + hex64('9')
	if err := VerifyActionGrant(req, grant, NewGrantUsageTracker(), time.Now()); err == nil {
		t.Fatal("ActionGrant with a changed parameter_hash was accepted")
	}
}

func TestActionGrantVerifyRejectsWrongCapability(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	// A grant bound to a DIFFERENT capability than the request must never
	// authorize this action, even if everything else (action/workcase/
	// actor/org/parameter-hash) matches.
	grant.Capability = "erp.purchase_order.delete"
	if err := VerifyActionGrant(req, grant, NewGrantUsageTracker(), time.Now()); err == nil {
		t.Fatal("ActionGrant bound to a different capability was accepted")
	}
}

func TestActionGrantVerifyRejectsDetachedActionID(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	grant.ActionID = "act-9999-different"
	if err := VerifyActionGrant(req, grant, NewGrantUsageTracker(), time.Now()); err == nil {
		t.Fatal("ActionGrant detached from its Action (different action_id) was accepted")
	}
}

func TestActionGrantVerifyRejectsWrongWorkCase(t *testing.T) {
	req := validActionRequest()
	grant := validActionGrant(req)
	grant.WorkCaseID = "case-9999-different"
	if err := VerifyActionGrant(req, grant, NewGrantUsageTracker(), time.Now()); err == nil {
		t.Fatal("ActionGrant bound to a different work_case_id was accepted")
	}
}
