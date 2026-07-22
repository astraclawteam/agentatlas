// Package workcaseexec is the composition root for the WorkCase orchestrator
// (internal/workcase.Orchestrator).
//
// It exists because of the defect class this repository has been clearing:
// internal/workcase holds a complete, unit-tested, deterministic orchestrator --
// Advance, Prepare, the ActionState machine, upward-review approval gating, the
// timeout->result_unknown no-blind-retry rule -- and NOTHING constructed it. Not
// one binary, not one wiring function. The package fails closed, so nothing
// crashed; the feature simply did not exist, while the suite stayed green
// because every test built its own Orchestrator.
//
// This package is where an Orchestrator is built, and the ONLY place. It follows
// internal/app/deps_required.go: a reflective assertion that every required
// seam is non-nil, an exact optional set that forces a decision when a seam is
// added, and a startup refusal that NAMES what is missing rather than degrading
// into a silently inert orchestrator.
//
// Seam availability today (verified against the frozen AgentNexus contract; see
// gap.go for the pinned evidence):
//
//   - Service   -- available (internal/workcase.PostgresStore).
//   - Runs      -- available in memory only; see Report.DurableRunLedger.
//   - Gateway   -- NO production implementation is possible yet. See gap.go.
//   - Governor  -- NO production implementation exists yet. See gap.go.
//
// Build therefore refuses, by name, instead of returning an Orchestrator whose
// every side effect would escalate to human takeover on first contact.
package workcaseexec

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	governance "github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	workcase "github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

// optionalWorkCaseDeps names every interface-typed Deps field an Orchestrator
// may legitimately be built without. Everything else is required.
//
// workcase.NewOrchestrator itself only rejects a nil Service/Runs/Gateway/
// Governor. That check runs at the END of composition and reports the four as
// one undifferentiated message; this set is what makes the decision visible at
// the point a seam is ADDED. TestOptionalWorkCaseDepsIsExact asserts it exact in
// both directions, so a new interface field forces a choice: wire it, or declare
// it optional here and say why.
var optionalWorkCaseDeps = map[string]string{
	"Planner":  "replanning is optional; without it a replan escalates to human takeover",
	"Audit":    "the orchestrator derives a deterministic internal audit ref when unset",
	"Observer": "belongs to the Outcome path; only meaningful together with Outcome",
}

// Deps are the seams an Orchestrator is composed from. It mirrors
// workcase.Config, minus the fields Build derives itself (Now, MaxAttempts).
//
// Service is a concrete pointer rather than an interface, so MissingRequired's
// reflective sweep cannot see it; it is checked by hand below, exactly like
// AgentRouterDeps.Dependencies. The same applies to TrustedKey and
// ApprovalPolicy, which are value types whose ZERO value is precisely the
// silent misconfiguration that would make every receipt unverifiable and every
// upward review fail.
type Deps struct {
	Service  *workcase.Service
	Runs     workcase.RunStore
	Gateway  workcase.ActionGateway
	Governor workcase.Governor

	Planner  workcase.Planner
	Audit    workcase.AuditSink
	Observer workcase.ObservationGateway

	// TrustedKeyID/TrustedKey are AgentNexus's receipt-signing identity. A
	// receipt that does not verify against them is an untrusted claim and never
	// completes a step, so an unset key is not a degraded mode -- it is an
	// orchestrator that can never succeed.
	TrustedKeyID string
	TrustedKey   ed25519.PublicKey

	// ApprovalPolicy carries the authoritative current org version. Its zero
	// value fails every upward review, which would gate out every side effect
	// while looking like a policy decision.
	ApprovalPolicy governance.UpwardReviewPolicy

	// Outcome is optional (Task 0H). When nil a fully-executed plan leaves the
	// case executing, which is the documented Task 0E behaviour.
	Outcome *workcase.OutcomeConfig

	Now         func() time.Time
	MaxAttempts int
}

// MissingRequired reports the required seams this value leaves unset, sorted. An
// empty result means every seam the Orchestrator needs is present; it does NOT
// mean each one works.
//
// Only interface fields are swept reflectively. The three non-interface required
// seams are checked by hand because their zero values are the exact false-greens
// this function exists to catch.
func (d Deps) MissingRequired() []string {
	value := reflect.ValueOf(d)
	typ := value.Type()
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() || field.Type.Kind() != reflect.Interface {
			continue
		}
		if _, optional := optionalWorkCaseDeps[field.Name]; optional {
			continue
		}
		if value.Field(i).IsNil() {
			missing = append(missing, field.Name)
		}
	}
	if d.Service == nil {
		missing = append(missing, "Service")
	}
	// Not "is it set" but "is it usable": a short key can never verify an
	// ed25519 signature, so it is missing in the only sense that matters.
	if len(d.TrustedKey) != ed25519.PublicKeySize || strings.TrimSpace(d.TrustedKeyID) == "" {
		missing = append(missing, "TrustedKey")
	}
	if d.ApprovalPolicy.CurrentOrgVersion <= 0 {
		missing = append(missing, "ApprovalPolicy.CurrentOrgVersion")
	}
	sort.Strings(missing)
	return missing
}

// ErrNotComposed reports that the WorkCase orchestrator could not be composed
// because one or more required seams have no production source. It is a refusal,
// never a degraded mode: an Orchestrator missing a Gateway or Governor would
// escalate every step to human takeover, which reads on every surface like a
// governance decision rather than a wiring gap.
var ErrNotComposed = errors.New("workcaseexec: WorkCase orchestrator not composed")

// NotComposedError names exactly which seams are absent and, where known, why.
type NotComposedError struct {
	Missing []string
	// Reasons maps a missing seam to the reason it has no production source, for
	// the seams where that reason is a known contract-level fact rather than a
	// forgotten wire-up. Absent from the map means "nothing supplied it here".
	Reasons map[string]string
}

func (e *NotComposedError) Error() string {
	parts := make([]string, 0, len(e.Missing))
	for _, name := range e.Missing {
		if reason := e.Reasons[name]; reason != "" {
			parts = append(parts, name+" ("+reason+")")
			continue
		}
		parts = append(parts, name)
	}
	return fmt.Sprintf("%s: missing %s", ErrNotComposed.Error(), strings.Join(parts, "; "))
}

func (e *NotComposedError) Unwrap() error { return ErrNotComposed }

// New validates deps and constructs the Orchestrator. It refuses with a
// *NotComposedError naming every absent seam BEFORE calling
// workcase.NewOrchestrator, so a composition root reports "Gateway and Governor
// have no production implementation" instead of the constructor's single
// undifferentiated "requires Service, Runs, Gateway and Governor".
func New(deps Deps) (*workcase.Orchestrator, error) {
	if missing := deps.MissingRequired(); len(missing) > 0 {
		return nil, &NotComposedError{Missing: missing, Reasons: seamReasons()}
	}
	return workcase.NewOrchestrator(workcase.Config{
		Service:        deps.Service,
		Runs:           deps.Runs,
		Gateway:        deps.Gateway,
		Governor:       deps.Governor,
		TrustedKeyID:   deps.TrustedKeyID,
		TrustedKey:     deps.TrustedKey,
		ApprovalPolicy: deps.ApprovalPolicy,
		Audit:          deps.Audit,
		Planner:        deps.Planner,
		Outcome:        deps.Outcome,
		Now:            deps.Now,
		MaxAttempts:    deps.MaxAttempts,
	})
}
