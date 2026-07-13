// Package workcase mirrors schemas/workcase/workcase.schema.json,
// schemas/workcase/work-plan.schema.json and schemas/workcase/action.schema.json.
// WorkCase, WorkPlan and ActionSpec are the frozen operating-work contracts:
// every orchestration, execution, governance and receipt-verification task
// (0B-0F, 4-10, 18A-18D, 21) builds against these types and no other
// Go/domain vocabulary. Authoritative validation happens server-side against
// the JSON Schemas. These types carry opaque handles and hashes only, never
// connector endpoints, credentials, raw enterprise content or HR actions.
package workcase

import (
	"errors"
	"time"
)

// Status is the lifecycle state of a WorkCase.
type Status string

// StepStatus is the execution state of a single Step within a WorkPlan.
type StepStatus string

const (
	StatusDraft      Status = "draft"
	StatusReviewing  Status = "reviewing"
	StatusExecuting  Status = "executing"
	StatusCompleted  Status = "completed"
	StatusTerminated Status = "terminated"
)

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepCompleted StepStatus = "completed"
	StepFailed    StepStatus = "failed"
)

// WorkCase is the top-level operating-work record: one enterprise-scoped
// case of work carried out by an actor (human or agent) through zero or
// more WorkPlan revisions.
type WorkCase struct {
	ID           string     `json:"id"`
	EnterpriseID string     `json:"enterprise_id"`
	OrgScope     string     `json:"org_scope"`
	ActorRef     string     `json:"actor_ref"`
	Status       Status     `json:"status"`
	Revision     uint64     `json:"revision"`
	Plans        []WorkPlan `json:"plans,omitempty"`
}

// WorkPlan is one proposed or executing revision of a WorkCase's plan.
// Revision is immutable once the plan has been approved or execution has
// begun: a changed plan is always expressed as a new WorkPlan at a higher
// Revision, never as a mutation of an in-flight one.
type WorkPlan struct {
	Revision uint64 `json:"revision"`
	Steps    []Step `json:"steps"`
}

// Step is one unit of work within a WorkPlan. A Step with a nil Action is a
// checkpoint (for example a human confirmation) that performs no action of
// its own.
type Step struct {
	ID       string        `json:"id"`
	Status   StepStatus    `json:"status,omitempty"`
	Evidence []EvidenceRef `json:"evidence,omitempty"`
	Action   *ActionSpec   `json:"action,omitempty"`
}

// Precondition is a fact the runtime must reconfirm (by opaque value hash)
// before an ActionSpec may execute.
type Precondition struct {
	Kind      string `json:"kind"`
	ValueHash string `json:"value_hash"`
}

// EvidenceRef is an opaque, content-addressed pointer to evidence collected
// during a Step. It carries a handle and hash only, never raw content.
type EvidenceRef struct {
	Handle      string `json:"handle"`
	ContentHash string `json:"content_hash"`
	Authority   string `json:"authority"`
}

// ReceiptRef is an opaque, content-addressed pointer to the signed receipt
// produced when an ActionSpec executes. It carries a handle and hash only,
// never raw content.
type ReceiptRef struct {
	Handle         string `json:"handle"`
	ReceiptHash    string `json:"receipt_hash"`
	SignatureKeyID string `json:"signature_key_id"`
}

// ActionSpec is the frozen shape of one dispatchable action: a business
// capability invocation identified by opaque parameter and idempotency
// hashes, never a connector endpoint or credential. Kind is an open,
// business-defined verb (for example "read" or "write"), but side-effecting
// actions MUST declare Kind "write": the FailurePolicy guard (ValidatePlan
// here, and the if/then rule in action.schema.json) fires only on the
// literal "write", so any other verb bypasses it entirely.
type ActionSpec struct {
	Kind               string         `json:"kind"`
	BusinessCapability string         `json:"business_capability"`
	ParametersHash     string         `json:"parameters_hash"`
	Risk               string         `json:"risk,omitempty"`
	IdempotencyKey     string         `json:"idempotency_key"`
	Preconditions      []Precondition `json:"preconditions,omitempty"`
	// ExpiresAt is always present on the wire (a time.Time struct is never
	// "empty", so omitempty would be a silent no-op). The zero timestamp
	// means "expiry not yet bound" — Task 0D requires a bound, non-zero
	// expiry before an Action can be granted (expiry is an ELC-NEXUS-1
	// conformance dimension).
	ExpiresAt             time.Time `json:"expires_at"`
	ExpectedReceiptSchema string    `json:"expected_receipt_schema,omitempty"`
	// CompensationActionID is a pointer so "unset" (nil: no compensation
	// bound) is distinguishable from "empty string" on the wire.
	CompensationActionID *string `json:"compensation_action_id,omitempty"`
	FailurePolicy        string  `json:"failure_policy,omitempty"`
}

// ErrMissingFailurePolicy is returned by ValidatePlan when a side-effecting
// ("write") action has no FailurePolicy.
var ErrMissingFailurePolicy = errors.New("side-effect action requires failure policy")

// ValidatePlan enforces the one invariant ActionSpec cannot express by
// itself: every "write" (side-effecting) action must declare a
// FailurePolicy so the runtime knows how to recover from a failed dispatch.
func ValidatePlan(p WorkPlan) error {
	for _, s := range p.Steps {
		if s.Action != nil && s.Action.Kind == "write" && s.Action.FailurePolicy == "" {
			return ErrMissingFailurePolicy
		}
	}
	return nil
}
