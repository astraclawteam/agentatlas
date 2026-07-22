// Package nexus (action.go) defines the governed Action Request, Grant and
// Receipt protocol AgentAtlas consumes from AgentNexus (GA Task 0D).
//
// Contract lineage:
//   - ActionStatus, ActionRequest (minus the WorkCase/Goal/Outcome/
//     postcondition/verification-need fields, which are AgentAtlas's own
//     governance additions), ActionGrant (field-compatible with the frozen
//     StepGrant) and ActionReceipt's execution-proof shape are all
//     field-compatible with AgentNexus's FROZEN v1 Action contract, pinned
//     at agentnexus@73ed5a7e83d0804673e3191cf79cc22699f41b3f
//     (sdk/go/runtime/{action,receipt,approval,context}.go and
//     services/agentnexus/api/proto/agentnexus/{actions,trust}/v1/*.proto):
//     same parameter-hash grammar (sha256:<64 hex>), same detached ed25519
//     Signature shape, same idempotency-key and RFC3339-expiry conventions.
//   - ObservationReceipt, PostconditionSpec and VerificationNeed are NOT
//     part of that frozen v1 contract (AgentNexus's own Task 0A has not
//     frozen them, and its Task 0G conformance-signing has not run). They
//     are implemented here against the CROSS-PRODUCT AGREED SPEC: both
//     GA plans (AgentAtlas's and AgentNexus's) describe these three types
//     field-for-field identically in their "Interfaces" sections. See
//     evidence/2026.2-lts/ga-task-0d/notes.md for the exact cross-check and
//     the pending-freeze/re-pin decision.
//
// AgentAtlas never implements an AgentNexus connector in this package: no
// connector endpoint, credential, raw enterprise content or connector-
// specific request body appears anywhere below — only opaque handles,
// hashes and business-semantic references cross this boundary.
package nexus

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// --- shared grammar (mirrors agentnexus@73ed5a7e sdk/go/runtime/validate.go) ---

var sha256RefPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidateSHA256Ref checks the canonical sha256:<64 hex> digest format the
// frozen AgentNexus v1 contract uses for every parameter/result/plan hash.
func ValidateSHA256Ref(value string) error {
	if !sha256RefPattern.MatchString(value) {
		return fmt.Errorf("%q is not a sha256:<64 hex> digest", value)
	}
	return nil
}

func fieldErrorf(field, format string, args ...any) error {
	return fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...))
}

func requireNonEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fieldErrorf(field, "is required")
	}
	return nil
}

// --- ActionStatus (frozen AgentNexus v1 Action lifecycle) ------------------

// ActionStatus mirrors the frozen agentnexus.actions.v1.ActionStatus /
// runtime.ActionStatus lifecycle exactly. AgentAtlas never invents its own
// vocabulary for the Action-execution state machine.
type ActionStatus string

const (
	ActionStatusRequested        ActionStatus = "requested"
	ActionStatusAwaitingApproval ActionStatus = "awaiting_approval"
	ActionStatusGranted          ActionStatus = "granted"
	ActionStatusDispatched       ActionStatus = "dispatched"
	ActionStatusExecuting        ActionStatus = "executing"
	ActionStatusSucceeded        ActionStatus = "succeeded"
	ActionStatusFailed           ActionStatus = "failed"
	ActionStatusResultUnknown    ActionStatus = "result_unknown"
	ActionStatusReconciling      ActionStatus = "reconciling"
	ActionStatusCompensating     ActionStatus = "compensating"
	ActionStatusHumanTakeover    ActionStatus = "human_takeover"
)

// ActionStatuses returns the frozen states in their frozen order.
func ActionStatuses() []ActionStatus {
	return []ActionStatus{
		ActionStatusRequested, ActionStatusAwaitingApproval, ActionStatusGranted, ActionStatusDispatched,
		ActionStatusExecuting, ActionStatusSucceeded, ActionStatusFailed, ActionStatusResultUnknown,
		ActionStatusReconciling, ActionStatusCompensating, ActionStatusHumanTakeover,
	}
}

// Valid reports whether s is one of the frozen states.
func (s ActionStatus) Valid() bool {
	switch s {
	case ActionStatusRequested, ActionStatusAwaitingApproval, ActionStatusGranted, ActionStatusDispatched,
		ActionStatusExecuting, ActionStatusSucceeded, ActionStatusFailed, ActionStatusResultUnknown,
		ActionStatusReconciling, ActionStatusCompensating, ActionStatusHumanTakeover:
		return true
	}
	return false
}

// --- signatures and risk (field-compatible with agentnexus.trust.v1) -------

// SignatureAlgorithmEd25519 is the only signature algorithm the frozen v1
// contract recognizes.
const SignatureAlgorithmEd25519 = "ed25519"

// Signature is a detached signature by a named AgentNexus signing key.
// Field-compatible with the frozen agentnexus.trust.v1.Signature /
// runtime.Signature.
type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	// Value carries the base64-encoded signature bytes.
	Value string `json:"value"`
}

func (s *Signature) empty() bool {
	return s == nil || *s == Signature{}
}

// RiskLevel mirrors the frozen v1 risk classification vocabulary
// (agentnexus.trust.v1.RiskLevel / runtime.RiskLevel).
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// Valid reports whether l is one of the frozen risk levels.
func (l RiskLevel) Valid() bool {
	switch l {
	case RiskLow, RiskMedium, RiskHigh:
		return true
	}
	return false
}

// --- opaque cross-task references -------------------------------------

// GoalRef is an opaque reference into AgentAtlas's Goal graph. The Goal type
// itself is frozen by a later task, not GA Task 0D.
type GoalRef string

// OutcomeRef is an opaque reference into AgentAtlas's Outcome graph. The
// Outcome type itself is frozen by GA Task 0G, not this task — ActionRequest
// binds only the opaque reference here.
type OutcomeRef string

// --- ActionRequest -----------------------------------------------------

// Precondition states an assumption that must still hold when the Action
// executes. Field-compatible with the frozen runtime.Precondition.
type Precondition struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Expected  string `json:"expected,omitempty"`
}

// Validate applies the canonical Precondition rules.
func (p Precondition) Validate() error {
	if p.Kind == "" || p.Reference == "" {
		return fieldErrorf("precondition", "kind and reference are required")
	}
	return nil
}

// VerificationNeed declares how a PostconditionSpec must be confirmed after
// an Action executes: a bounded, source-authoritative, fresh observation —
// never a raw connector query left to the caller's discretion. NOT part of
// the frozen AgentNexus v1 contract; see the package doc comment.
type VerificationNeed struct {
	NeedID    string `json:"need_id"`
	Source    string `json:"source"`
	Authority string `json:"authority"`
	// MaxAge bounds how old a satisfying observation may be. Zero means no
	// freshness bound is declared.
	//
	// NOT ENFORCED IN GA TASK 0D: VerifyObservationReceipt (the only
	// consumer of this field's semantics here) does NOT compare MaxAge
	// against ObservationReceipt.ObservedAt — it rechecks identity, binding
	// and signature only. Outcome-closure (GA Tasks 0G/0H) MUST enforce
	// MaxAge against ObservedAt before treating a verified observation as
	// current.
	MaxAge time.Duration `json:"max_age,omitempty"`
}

// Validate applies the canonical VerificationNeed rules.
func (n VerificationNeed) Validate() error {
	if err := requireNonEmpty("need_id", n.NeedID); err != nil {
		return err
	}
	if err := requireNonEmpty("source", n.Source); err != nil {
		return err
	}
	return requireNonEmpty("authority", n.Authority)
}

// PostconditionSpec states a business-semantic assertion that should hold
// after the Action executes (for example "purchase order PO-100 status is
// approved") and names the VerificationNeed that confirms it. AgentNexus
// never evaluates postconditions; it only proves the bounded observation
// named by VerificationNeedID. NOT part of the frozen AgentNexus v1
// contract; see the package doc comment.
type PostconditionSpec struct {
	PostconditionID    string `json:"postcondition_id"`
	Description        string `json:"description"`
	VerificationNeedID string `json:"verification_need_id"`
}

// Validate applies the canonical PostconditionSpec rules.
func (p PostconditionSpec) Validate() error {
	if err := requireNonEmpty("postcondition_id", p.PostconditionID); err != nil {
		return err
	}
	if err := requireNonEmpty("description", p.Description); err != nil {
		return err
	}
	return requireNonEmpty("verification_need_id", p.VerificationNeedID)
}

// ActionRequest is AgentAtlas's governed request for AgentNexus to execute
// one side effect. It binds the Goal and expected Outcome references, the
// WorkCase/WorkPlan context (sdk/go/workcase Task 0A types, where those
// types are plain primitives — WorkCaseID mirrors workcase.WorkCase.ID,
// WorkPlanRevision mirrors workcase.WorkPlan.Revision), the Action ID,
// actor and org scope, the exact hash-bound capability invocation, its
// preconditions/postconditions/verification needs, risk, approval
// references, idempotency key, expiry, expected execution-receipt schema
// and an optional compensation reference.
//
// This is the ONLY thing that crosses the nexusclient.ActionClient boundary
// toward AgentNexus: never a connector-specific request body, connector
// endpoint or credential.
//
// Risk and ApprovalRefs are deliberately minimal in this task (a RiskLevel
// enum and opaque approval references, not the frozen contract's fully
// embedded signed RiskDecision/ApprovalPlanRef structures): the reciprocal
// ELC-ATLAS-1 scope for full domain RiskDecision/ApprovalPlanRef binding
// spans GA Tasks 0D-0F, not 0D alone. See notes.md.
type ActionRequest struct {
	ActionID string `json:"action_id"`

	GoalRef    GoalRef    `json:"goal_ref"`
	OutcomeRef OutcomeRef `json:"outcome_ref"`

	WorkCaseID       string `json:"work_case_id"`
	WorkPlanRevision uint64 `json:"work_plan_revision"`

	// Actor and OrgScope are the AgentAtlas-declared identity and
	// organization scope this action is requested under. AgentNexus
	// re-derives and rechecks trusted identity from verified credentials
	// independently — these fields are never trusted as-is by AgentNexus.
	// AgentAtlas's OWN client uses them for defense-in-depth: it rechecks
	// that a returned ActionGrant still binds the SAME actor/org (see
	// VerifyActionGrant), catching a confused-deputy class of bug even
	// though AgentNexus remains the identity authority.
	Actor    string `json:"actor"`
	OrgScope string `json:"org_scope"`

	Capability    string `json:"capability"`
	ParameterHash string `json:"parameter_hash"`

	Preconditions     []Precondition      `json:"preconditions,omitempty"`
	Postconditions    []PostconditionSpec `json:"postconditions,omitempty"`
	VerificationNeeds []VerificationNeed  `json:"verification_needs,omitempty"`

	Risk RiskLevel `json:"risk"`

	// ApprovalRefs are opaque references to approval plans/evidence;
	// AgentNexus transmits but never authors approval plans, and this
	// request never inlines a full approval payload.
	ApprovalRefs []string `json:"approval_references,omitempty"`

	IdempotencyKey string    `json:"idempotency_key"`
	ExpiresAt      time.Time `json:"expires_at"`

	ExecutionReceiptSchema string `json:"execution_receipt_schema"`
	CompensationRef        string `json:"compensation_ref,omitempty"`
}

func validateIdempotencyKey(key string) error {
	if key == "" {
		return fieldErrorf("idempotency_key", "is required")
	}
	if len(key) < 16 || len(key) > 128 {
		return fieldErrorf("idempotency_key", "must be 16..128 bytes")
	}
	return nil
}

// Validate applies the canonical ActionRequest rules: required references,
// the shared parameter-hash grammar, a bound (non-zero) expiry, a required
// idempotency key of the frozen contract's length, a recognized risk level,
// and — since AgentAtlas is where PostconditionSpec/VerificationNeed
// binding is authored — that every PostconditionSpec references a
// VerificationNeed actually declared on this same request (an ObservationReceipt
// can only ever be checked against needs the request itself declared).
func (r ActionRequest) Validate() error {
	if err := requireNonEmpty("action_id", r.ActionID); err != nil {
		return err
	}
	if err := requireNonEmpty("work_case_id", r.WorkCaseID); err != nil {
		return err
	}
	if err := requireNonEmpty("actor", r.Actor); err != nil {
		return err
	}
	if err := requireNonEmpty("org_scope", r.OrgScope); err != nil {
		return err
	}
	if err := requireNonEmpty("capability", r.Capability); err != nil {
		return err
	}
	if err := ValidateSHA256Ref(r.ParameterHash); err != nil {
		return fieldErrorf("parameter_hash", "%v", err)
	}
	for i, p := range r.Preconditions {
		if err := p.Validate(); err != nil {
			return fieldErrorf("preconditions", "[%d]: %v", i, err)
		}
	}
	needIDs := make(map[string]bool, len(r.VerificationNeeds))
	for i, n := range r.VerificationNeeds {
		if err := n.Validate(); err != nil {
			return fieldErrorf("verification_needs", "[%d]: %v", i, err)
		}
		needIDs[n.NeedID] = true
	}
	for i, p := range r.Postconditions {
		if err := p.Validate(); err != nil {
			return fieldErrorf("postconditions", "[%d]: %v", i, err)
		}
		if !needIDs[p.VerificationNeedID] {
			return fieldErrorf("postconditions", "[%d]: verification_need_id %q is not declared in verification_needs", i, p.VerificationNeedID)
		}
	}
	if !r.Risk.Valid() {
		return fieldErrorf("risk", "%q is not a frozen risk level", r.Risk)
	}
	if err := validateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
	if r.ExpiresAt.IsZero() {
		return fieldErrorf("expires_at", "is required")
	}
	if err := requireNonEmpty("execution_receipt_schema", r.ExecutionReceiptSchema); err != nil {
		return err
	}
	return nil
}

// --- ActionGrant (field-compatible with the frozen StepGrant) --------------

// ActionGrant authorizes AgentAtlas to proceed with exactly one exact
// operation: one capability, one parameter hash, one WorkCase, once, until
// expiry. Field-compatible with the frozen agentnexus.trust.v1.StepGrant /
// runtime.StepGrant, plus the Actor/OrgScope echo AgentAtlas's own client
// uses for defense-in-depth cross-checking against the original
// ActionRequest (see VerifyActionGrant) — AgentNexus itself still derives
// trusted actor/org only from verified credentials, never from these
// echoed fields.
type ActionGrant struct {
	GrantID    string `json:"grant_id"`
	ActionID   string `json:"action_id"`
	WorkCaseID string `json:"work_case_id"`
	Actor      string `json:"actor"`
	OrgScope   string `json:"org_scope"`

	Capability    string `json:"capability"`
	ParameterHash string `json:"parameter_hash"`

	// OneUse is always true by contract: a step grant is one-use.
	OneUse bool `json:"one_use"`

	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// --- ActionReceipt (technical execution proof only) ------------------------

// ActionReceipt proves technical execution of one Action and binds
// connector/target references, before/after hashes, executor identity,
// result code, external receipt, an audit reference and AgentNexus's
// signature. It NEVER asserts that the WorkCase Goal or business Outcome
// was achieved — see ConfirmsOutcome, which always fails.
type ActionReceipt struct {
	ReceiptID     string `json:"receipt_id"`
	ActionID      string `json:"action_id"`
	ParameterHash string `json:"parameter_hash"`

	// ConnectorRef and TargetRef are AgentNexus-opaque handles — never a
	// connector endpoint, credential or raw enterprise identifier.
	ConnectorRef string `json:"connector_ref"`
	TargetRef    string `json:"target_ref"`

	BeforeHash string `json:"before_hash,omitempty"`
	AfterHash  string `json:"after_hash,omitempty"`

	ExecutorIdentity string `json:"executor_identity"`
	ResultCode       string `json:"result_code"`

	// ExternalReceipt is the opaque, hash-bound receipt payload the
	// underlying system of record returned (for example an ERP document
	// number) — content only, never a connector-specific body shape that
	// AgentAtlas interprets.
	ExternalReceipt json.RawMessage `json:"external_receipt,omitempty"`

	AuditRef string `json:"audit_ref"`

	IssuedAt  time.Time  `json:"issued_at"`
	Signature *Signature `json:"signature,omitempty"`
}

// signingPayload returns the canonical byte payload ActionReceipt.Signature
// is computed over: the receipt with Signature cleared, JSON-marshaled.
// Deterministic for a fixed Go type (encoding/json emits struct fields in
// declaration order), which is all a signer and verifier that both use this
// same method need.
func (r ActionReceipt) signingPayload() []byte {
	r.Signature = nil
	raw, _ := json.Marshal(r)
	return raw
}

// ErrReceiptNotOutcomeEvidence documents and enforces, at the API level,
// that a purely technical receipt can never be accepted as proof that a
// WorkCase Outcome was achieved.
var ErrReceiptNotOutcomeEvidence = errors.New("nexus: this receipt proves technical execution/observation only; it never asserts a WorkCase Outcome was achieved")

// ConfirmsOutcome always fails: callers must never treat a technical
// ActionReceipt as business Outcome evidence. Outcome evidence requires a
// verified ObservationReceipt bound to a PostconditionSpec AND (GA Task
// 0G/0H) deterministic Outcome-closure logic that does not exist in this
// package.
func (r ActionReceipt) ConfirmsOutcome() error {
	return ErrReceiptNotOutcomeEvidence
}

// --- ObservationReceipt (bounded authoritative observation only) ----------

// ObservationReceipt proves one bounded, source-authoritative post-action
// observation. It binds the original Action/parameter hash, the
// PostconditionSpec and VerificationNeed it confirms, source/version,
// authority, observed-at/freshness, a normalized observation hash, an
// opaque EvidenceHandle reference (agentnexus@73ed5a7e
// sdk/go/runtime/evidence.go EvidenceHandle.EvidenceRef — EvidenceHandle
// itself is evidence-retrieval scope, not this task's), an audit reference
// and AgentNexus's signature.
//
// Like ActionReceipt, it NEVER itself asserts that a business Outcome was
// achieved — see ConfirmsOutcome. NOT part of the frozen AgentNexus v1
// contract; see the package doc comment and notes.md.
type ObservationReceipt struct {
	ObservationID string `json:"observation_id"`

	ActionID      string `json:"action_id"`
	ParameterHash string `json:"parameter_hash"`

	PostconditionID    string `json:"postcondition_id"`
	VerificationNeedID string `json:"verification_need_id"`

	Source        string `json:"source"`
	SourceVersion string `json:"source_version,omitempty"`
	Authority     string `json:"authority"`

	// ObservedAt is when the authoritative source reported the observation;
	// FreshnessWindow bounds how long that report stays fresh. NEITHER is
	// checked by VerifyObservationReceipt in GA Task 0D — Outcome-closure
	// (GA Tasks 0G/0H) MUST compare ObservedAt against the satisfied
	// VerificationNeed.MaxAge before treating this observation as current.
	ObservedAt      time.Time     `json:"observed_at"`
	FreshnessWindow time.Duration `json:"freshness_window,omitempty"`

	NormalizedObservationHash string `json:"normalized_observation_hash"`

	// EvidenceRef is an opaque reference to the AgentNexus EvidenceHandle
	// this observation is bound to.
	EvidenceRef string `json:"evidence_ref"`

	AuditRef string `json:"audit_ref"`

	Signature *Signature `json:"signature,omitempty"`
}

// signingPayload mirrors ActionReceipt.signingPayload: the canonical byte
// payload Signature is computed over.
func (o ObservationReceipt) signingPayload() []byte {
	o.Signature = nil
	raw, _ := json.Marshal(o)
	return raw
}

// ConfirmsOutcome always fails, for the same reason as
// ActionReceipt.ConfirmsOutcome: even a fully verified ObservationReceipt is
// only ONE bounded observation, not a deterministic Outcome decision. Only a
// later Outcome-closure task (GA 0G/0H) may turn a set of verified
// ObservationReceipts into an Outcome.
func (o ObservationReceipt) ConfirmsOutcome() error {
	return ErrReceiptNotOutcomeEvidence
}

// --- signature verification hook -------------------------------------

// Sentinel errors returned by the verification hooks below. Every one wraps
// (via errors.Is) into either a signature-trust failure or a binding
// failure, so callers/tests can assert exactly which recheck rejected a
// given receipt or grant without string-matching messages.
var (
	// ErrUnsignedReceipt rejects a nil/zero-value Signature. Unsigned
	// authority claims never enter AgentAtlas's trust boundary.
	ErrUnsignedReceipt = errors.New("nexus: unsigned authority claim rejected")
	// ErrUntrustedSignature rejects a Signature that is present but does
	// not verify against the configured trusted AgentNexus signing key
	// (wrong algorithm, wrong key_id, or a cryptographically invalid
	// value).
	ErrUntrustedSignature = errors.New("nexus: signature does not verify against the trusted AgentNexus key")
	// ErrGrantExpired rejects an ActionGrant whose ExpiresAt has passed.
	ErrGrantExpired = errors.New("nexus: action grant has expired")
	// ErrGrantReplayed rejects an ActionGrant whose one-use grant_id has
	// already been consumed.
	ErrGrantReplayed = errors.New("nexus: action grant has already been consumed (replay)")
	// ErrActorOrgMismatch rejects an ActionGrant whose echoed actor/org
	// scope no longer matches the ActionRequest it claims to authorize.
	ErrActorOrgMismatch = errors.New("nexus: action grant actor/org_scope does not match the requesting ActionRequest")
)

func verifySignature(sig *Signature, trustedKeyID string, trustedKey ed25519.PublicKey, payload []byte) error {
	if sig.empty() {
		return ErrUnsignedReceipt
	}
	if sig.Algorithm != SignatureAlgorithmEd25519 {
		return fmt.Errorf("%w: algorithm %q is not a frozen v1 signature algorithm", ErrUntrustedSignature, sig.Algorithm)
	}
	if sig.KeyID != trustedKeyID {
		return fmt.Errorf("%w: key_id %q is not the configured trusted AgentNexus signing key", ErrUntrustedSignature, sig.KeyID)
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Value)
	if err != nil {
		return fmt.Errorf("%w: signature value is not valid base64: %v", ErrUntrustedSignature, err)
	}
	if len(trustedKey) != ed25519.PublicKeySize || !ed25519.Verify(trustedKey, payload, raw) {
		return ErrUntrustedSignature
	}
	return nil
}

// VerifyActionReceipt cryptographically and structurally rechecks a
// received ActionReceipt against the ActionRequest it claims to satisfy:
// exact Action binding, an unchanged parameter hash, and a signature that
// verifies against the trusted AgentNexus signing key. This is AgentAtlas's
// OWN client-side recheck (defense in depth) — it does not replace
// AgentNexus's server-side enforcement (identity, org, approval, replay,
// capability), which happens before execution and is out of this package's
// scope.
func VerifyActionReceipt(req ActionRequest, receipt ActionReceipt, trustedKeyID string, trustedKey ed25519.PublicKey) error {
	if receipt.ActionID != req.ActionID {
		return fmt.Errorf("nexus: action receipt %q is detached from action %q", receipt.ReceiptID, req.ActionID)
	}
	if receipt.ParameterHash != req.ParameterHash {
		return fmt.Errorf("nexus: action receipt parameter_hash %q does not match the requested action's %q", receipt.ParameterHash, req.ParameterHash)
	}
	if err := ValidateSHA256Ref(receipt.ParameterHash); err != nil {
		return fmt.Errorf("nexus: action receipt: %w", err)
	}
	return verifySignature(receipt.Signature, trustedKeyID, trustedKey, receipt.signingPayload())
}

// VerifyObservationReceipt cryptographically and structurally rechecks a
// received ObservationReceipt against the ActionRequest it claims to
// satisfy: exact Action/parameter-hash binding, that the referenced
// PostconditionSpec and VerificationNeed were actually declared on req (an
// ObservationReceipt detached from its Action/PostconditionSpec is
// rejected), and a signature that verifies against the trusted AgentNexus
// signing key.
//
// FRESHNESS IS NOT ENFORCED HERE. This function does NOT compare the
// receipt's ObservedAt against the satisfied VerificationNeed's MaxAge (nor
// against any wall clock): a stale-but-authentic observation passes this
// recheck. This is deliberate — GA Task 0D freezes the shape plus the
// identity/binding/signature recheck only; turning verified
// ObservationReceipts into a business Outcome is Outcome-closure (GA Tasks
// 0G/0H), and THAT is where MaxAge-vs-ObservedAt freshness MUST be
// enforced. Callers must NOT assume a receipt this function accepts is
// fresh. (Contrast VerifyActionGrant, which DOES reject an expired grant
// here: grant expiry is an execution-time safety gate; observation
// freshness is an Outcome-semantics gate a later task owns.)
func VerifyObservationReceipt(req ActionRequest, receipt ObservationReceipt, trustedKeyID string, trustedKey ed25519.PublicKey) error {
	if receipt.ActionID != req.ActionID {
		return fmt.Errorf("nexus: observation receipt %q is detached from action %q", receipt.ObservationID, req.ActionID)
	}
	if receipt.ParameterHash != req.ParameterHash {
		return fmt.Errorf("nexus: observation receipt parameter_hash %q does not match the requested action's %q", receipt.ParameterHash, req.ParameterHash)
	}
	var need *VerificationNeed
	for i := range req.VerificationNeeds {
		if req.VerificationNeeds[i].NeedID == receipt.VerificationNeedID {
			need = &req.VerificationNeeds[i]
			break
		}
	}
	if need == nil {
		return fmt.Errorf("nexus: observation receipt references verification_need_id %q not declared by the action request", receipt.VerificationNeedID)
	}
	postFound := false
	for _, p := range req.Postconditions {
		if p.PostconditionID == receipt.PostconditionID && p.VerificationNeedID == receipt.VerificationNeedID {
			postFound = true
			break
		}
	}
	if !postFound {
		return fmt.Errorf("nexus: observation receipt references postcondition_id %q not declared by the action request", receipt.PostconditionID)
	}
	return verifySignature(receipt.Signature, trustedKeyID, trustedKey, receipt.signingPayload())
}

// GrantUsageTracker records which one-use ActionGrant IDs have already been
// consumed, so VerifyActionGrant can reject a replayed grant even though an
// ActionGrant (like the frozen StepGrant it mirrors) carries no signature of
// its own to re-verify. Safe for concurrent use. The zero value is not
// usable; construct with NewGrantUsageTracker.
type GrantUsageTracker struct {
	mu   sync.Mutex
	used map[string]bool
}

// NewGrantUsageTracker returns a ready-to-use, empty GrantUsageTracker.
func NewGrantUsageTracker() *GrantUsageTracker {
	return &GrantUsageTracker{used: map[string]bool{}}
}

// Consume marks grantID as used and reports whether this was the first use.
func (t *GrantUsageTracker) Consume(grantID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.used[grantID] {
		return false
	}
	t.used[grantID] = true
	return true
}

// VerifyActionGrant rechecks a received ActionGrant against the
// ActionRequest it claims to authorize: exact WorkCase/actor/org/
// capability/parameter-hash binding, one-use semantics, non-expiry (as of
// now) and — via tracker — replay protection. tracker may be nil to skip
// replay tracking (for example when verifying a grant the caller has
// already separately deduplicated).
func VerifyActionGrant(req ActionRequest, grant ActionGrant, tracker *GrantUsageTracker, now time.Time) error {
	if grant.ActionID != req.ActionID {
		return fmt.Errorf("nexus: action grant %q is detached from action %q", grant.GrantID, req.ActionID)
	}
	if grant.WorkCaseID != req.WorkCaseID {
		return fmt.Errorf("nexus: action grant work_case_id %q does not match the requested %q", grant.WorkCaseID, req.WorkCaseID)
	}
	if grant.Actor != req.Actor || grant.OrgScope != req.OrgScope {
		return fmt.Errorf("%w: grant actor=%q org_scope=%q, request actor=%q org_scope=%q", ErrActorOrgMismatch, grant.Actor, grant.OrgScope, req.Actor, req.OrgScope)
	}
	if grant.Capability != req.Capability {
		return fmt.Errorf("nexus: action grant capability %q does not match the requested %q", grant.Capability, req.Capability)
	}
	if grant.ParameterHash != req.ParameterHash {
		return fmt.Errorf("nexus: action grant parameter_hash %q does not match the requested action's %q", grant.ParameterHash, req.ParameterHash)
	}
	if !grant.OneUse {
		return errors.New("nexus: action grant is not one-use; a step grant must be one-use by contract")
	}
	if !now.Before(grant.ExpiresAt) {
		return fmt.Errorf("%w: expired at %s (checked at %s)", ErrGrantExpired, grant.ExpiresAt, now)
	}
	if tracker != nil && !tracker.Consume(grant.GrantID) {
		return fmt.Errorf("%w: grant %q already consumed", ErrGrantReplayed, grant.GrantID)
	}
	return nil
}

// --- ActionClient --------------------------------------------------------

// ActionClient is the governed Action Request/Grant/Receipt protocol
// AgentAtlas consumes from AgentNexus. AgentAtlas never implements an
// AgentNexus connector: every call crosses this boundary with only the
// typed ActionRequest — never a connector-specific request body, endpoint
// or credential.
type ActionClient interface {
	// RequestAction submits a governed ActionRequest and returns the
	// resulting ActionGrant. Implementations recheck the returned grant
	// with VerifyActionGrant before returning it to the caller — an
	// expired, replayed or actor/org-mismatched grant is never returned as
	// success.
	RequestAction(ctx context.Context, req ActionRequest) (ActionGrant, error)

	// FetchActionReceipt retrieves the signed ActionReceipt under
	// receiptRef and rechecks it with VerifyActionReceipt before returning
	// it. An unsigned, wrong-key, detached or parameter-hash-mismatched
	// receipt is never returned as success.
	//
	// receiptRef is an opaque handle minted by AgentNexus: the frozen
	// receipt surface is addressed by handle and offers no by-action-id
	// lookup, so the caller carries the handle it was issued.
	FetchActionReceipt(ctx context.Context, req ActionRequest, receiptRef string) (ActionReceipt, error)

	// FetchObservationReceipt retrieves the signed ObservationReceipt
	// confirming verificationNeedID for req and rechecks it with
	// VerifyObservationReceipt before returning it.
	//
	// AgentNexus has frozen no observation-read surface, so implementations
	// are expected to fail closed rather than invent a path. The recheck
	// itself, VerifyObservationReceipt, stands on its own and is used
	// wherever an ObservationReceipt is obtained.
	FetchObservationReceipt(ctx context.Context, req ActionRequest, verificationNeedID string) (ObservationReceipt, error)
}
