package outcome_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/outcome"
)

// validObservation returns a signed, authoritative ObservationRef: a
// handle+hash reference to a nexus.ObservationReceipt, never the receipt body.
func validObservation() outcome.ObservationRef {
	return outcome.ObservationRef{
		Handle:          "obs-handle-1",
		ObservationHash: "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		Authority:       "system_of_record",
		SignatureKeyID:  "nexus-key-1",
		ObservedAt:      time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC),
	}
}

// validClaim returns a satisfied OutcomeClaim justified by one authoritative
// signed ObservationRef.
func validClaim() outcome.OutcomeClaim {
	return outcome.OutcomeClaim{
		Goal:         outcome.GoalRef{Tenant: "ent-1", GoalKey: "mes.close_anomaly", GoalVersion: 3},
		Status:       outcome.OutcomeSatisfied,
		RuleVersion:  "outcome-policy-rev-7",
		Evidence:     []outcome.EvidenceRef{{Handle: "evd-1", ContentHash: "sha256:abc", Authority: "agentnexus"}},
		Observations: []outcome.ObservationRef{validObservation()},
		Confidence: []outcome.ConfidenceComponent{
			{Kind: "authority", Score: 0.9}, {Kind: "freshness", Score: 0.8},
		},
	}
}

// validOutcome returns a well-formed revision-1 satisfied Outcome binding a
// tenant, exact goal/WorkCase/WorkPlan/Operating Map/org versions and a rule
// version, justified by an authoritative ObservationRef.
func validOutcome() outcome.Outcome {
	return outcome.Outcome{
		Tenant:              "ent-1",
		OutcomeKey:          "case-42.goal.close_anomaly",
		Revision:            1,
		Claim:               validClaim(),
		WorkCaseID:          "case-42",
		WorkCaseRevision:    5,
		WorkPlanRevision:    2,
		OperatingMapVersion: 4,
		OrgVersion:          1,
		ActionReceipts:      []outcome.ReceiptRef{{Handle: "rcpt-1", ReceiptHash: "sha256:def", SignatureKeyID: "nexus-key-1"}},
		Contributions:       []outcome.ContributionRef{{ContributorID: "agent:planner", Kind: "method", Weight: 0.5}},
		DecidedAt:           time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
	}
}

func TestValidOutcomeValidates(t *testing.T) {
	if err := validOutcome().Validate(); err != nil {
		t.Fatalf("expected a valid outcome, got %v", err)
	}
}

// --- Constraint 1: OutcomeStatus is a closed enum; Action success is NOT an
// Outcome status; an ActionReceipt can never be coerced into an Outcome. ------

func TestOutcomeStatusIsClosedEnum(t *testing.T) {
	for _, s := range outcome.OutcomeStatuses() {
		if !s.Valid() {
			t.Fatalf("declared status %q must be valid", s)
		}
	}
	// The exact closed set, nothing more.
	want := map[outcome.OutcomeStatus]bool{
		outcome.OutcomeUnverified: true, outcome.OutcomeSatisfied: true, outcome.OutcomeUnsatisfied: true,
		outcome.OutcomeDisputed: true, outcome.OutcomeBlocked: true, outcome.OutcomeUnknown: true,
	}
	if len(outcome.OutcomeStatuses()) != len(want) {
		t.Fatalf("OutcomeStatuses() has %d entries, want %d", len(outcome.OutcomeStatuses()), len(want))
	}
	for _, s := range outcome.OutcomeStatuses() {
		if !want[s] {
			t.Fatalf("unexpected status %q in the closed set", s)
		}
	}
}

// TestActionSuccessIsNotAnOutcomeStatus proves the frozen nexus ActionStatus
// value "succeeded" (technical execution success) is NOT a valid OutcomeStatus,
// and an Outcome carrying it is rejected.
func TestActionSuccessIsNotAnOutcomeStatus(t *testing.T) {
	if outcome.OutcomeStatus("succeeded").Valid() {
		t.Fatal(`"succeeded" (an ActionStatus) must not be a valid OutcomeStatus`)
	}
	o := validOutcome()
	o.Claim.Status = outcome.OutcomeStatus("succeeded")
	if err := o.Validate(); !errors.Is(err, outcome.ErrUnknownOutcomeStatus) {
		t.Fatalf("outcome with an ActionStatus as status: got %v, want ErrUnknownOutcomeStatus", err)
	}
}

// TestActionReceiptCannotJustifySatisfiedOutcome proves a satisfied Outcome
// backed ONLY by an ActionReceipt (technical execution proof) and no
// authoritative ObservationRef is rejected: Action success is never Outcome
// evidence.
func TestActionReceiptCannotJustifySatisfiedOutcome(t *testing.T) {
	o := validOutcome()
	o.Claim.Observations = nil // strip the observation; only ActionReceipts remain
	if len(o.ActionReceipts) == 0 {
		t.Fatal("fixture must carry an action receipt")
	}
	if err := o.Validate(); !errors.Is(err, outcome.ErrActionReceiptNotOutcomeEvidence) {
		t.Fatalf("satisfied outcome justified only by an ActionReceipt: got %v, want ErrActionReceiptNotOutcomeEvidence", err)
	}
}

// --- Constraint 2: satisfied requires authoritative ObservationReceipt evidence.

func TestSatisfiedRequiresObservation(t *testing.T) {
	o := validOutcome()
	o.Claim.Observations = nil
	o.ActionReceipts = nil
	if err := o.Validate(); !errors.Is(err, outcome.ErrSatisfiedNeedsObservation) {
		t.Fatalf("satisfied outcome with no observation: got %v, want ErrSatisfiedNeedsObservation", err)
	}
}

func TestSatisfiedRequiresAuthoritativeObservation(t *testing.T) {
	o := validOutcome()
	o.Claim.Observations = []outcome.ObservationRef{{
		Handle: "obs-x", ObservationHash: "sha256:1", Authority: "", SignatureKeyID: "nexus-key-1",
		ObservedAt: time.Now(),
	}} // missing authority => not authoritative
	if err := o.Validate(); err == nil {
		t.Fatal("expected rejection: a satisfied outcome needs an AUTHORITATIVE observation")
	}
}

// TestObservationRefValidateRequiresAuthority aligns SDK Validate with the
// schema, which marks authority required. Before the fix Validate omitted the
// authority check, so the SDK was weaker than the schema.
func TestObservationRefValidateRequiresAuthority(t *testing.T) {
	o := validObservation()
	o.Authority = ""
	if err := o.Validate(); err == nil {
		t.Fatal("ObservationRef.Validate must require authority (schema marks authority required)")
	}
	if err := validObservation().Validate(); err != nil {
		t.Fatalf("a well-formed observation must validate: %v", err)
	}
}

func TestNonSatisfiedStatusNeedsNoObservation(t *testing.T) {
	o := validOutcome()
	o.Claim.Status = outcome.OutcomeUnsatisfied
	o.Claim.Observations = nil
	o.ActionReceipts = nil
	if err := o.Validate(); err != nil {
		t.Fatalf("unsatisfied outcome without observation should be allowed, got %v", err)
	}
}

// --- Constraint: a goal/rule version is mandatory. -------------------------

func TestOutcomeWithoutGoalVersionRejected(t *testing.T) {
	o := validOutcome()
	o.Claim.Goal.GoalVersion = 0
	if err := o.Validate(); !errors.Is(err, outcome.ErrMissingGoalVersion) {
		t.Fatalf("outcome without a goal version: got %v, want ErrMissingGoalVersion", err)
	}
}

func TestOutcomeWithoutRuleVersionRejected(t *testing.T) {
	o := validOutcome()
	o.Claim.RuleVersion = ""
	if err := o.Validate(); !errors.Is(err, outcome.ErrMissingRuleVersion) {
		t.Fatalf("outcome without a rule version: got %v, want ErrMissingRuleVersion", err)
	}
}

// --- Constraint 3: immutable revisions + explicit supersession/correction. ---

func TestRevisionOneMustNotSupersede(t *testing.T) {
	o := validOutcome()
	o.Revision = 1
	o.Supersedes = &outcome.OutcomeRevisionRef{OutcomeKey: o.OutcomeKey, Revision: 0, OutcomeID: "x", Reason: "correction"}
	if err := o.Validate(); !errors.Is(err, outcome.ErrUnexpectedSupersession) {
		t.Fatalf("revision 1 with a supersession pointer: got %v, want ErrUnexpectedSupersession", err)
	}
}

func TestCorrectionRevisionMustSupersede(t *testing.T) {
	o := validOutcome()
	o.Revision = 2
	o.Supersedes = nil
	if err := o.Validate(); !errors.Is(err, outcome.ErrMissingSupersession) {
		t.Fatalf("revision 2 without a supersession pointer: got %v, want ErrMissingSupersession", err)
	}
}

func TestCorrectionSupersedesImmediatePriorRevision(t *testing.T) {
	o := validOutcome()
	o.Revision = 3
	o.Supersedes = &outcome.OutcomeRevisionRef{OutcomeKey: o.OutcomeKey, Revision: 1, OutcomeID: "x", Reason: "correction"}
	if err := o.Validate(); err == nil {
		t.Fatal("expected rejection: a correction must supersede the immediately prior revision (2), not an older one")
	}
}

// --- Constraint 4: deterministic IDs; no graph-provider identifiers. --------

func TestStableIDDeterministicAndRevisionScoped(t *testing.T) {
	a := outcome.StableID("ent-1", outcome.NodeOutcome, "case-42.goal", 1)
	b := outcome.StableID("ent-1", outcome.NodeOutcome, "case-42.goal", 1)
	if a != b || a == "" {
		t.Fatalf("same inputs must yield the same non-empty id: %q vs %q", a, b)
	}
	if a == outcome.StableID("ent-1", outcome.NodeOutcome, "case-42.goal", 2) {
		t.Fatal("a different revision must yield a different id")
	}
	if a == outcome.StableID("ent-2", outcome.NodeOutcome, "case-42.goal", 1) {
		t.Fatal("a different tenant must yield a different id")
	}
	if a == outcome.StableID("ent-1", outcome.NodeGoal, "case-42.goal", 1) {
		t.Fatal("a different node type must yield a different id")
	}
}

func TestOutcomeDerivedIDMatchesStableID(t *testing.T) {
	o := validOutcome()
	if got, want := o.DerivedID(), outcome.StableID(o.Tenant, outcome.NodeOutcome, o.OutcomeKey, o.Revision); got != want {
		t.Fatalf("Outcome.DerivedID()=%q, want StableID=%q (id must derive only from tenant/type/business-id/revision)", got, want)
	}
}

// --- Constraint 5: tenant isolation on lineage edges. ----------------------

func validEndpoint(tenant string, t outcome.NodeType, id string, rev uint64) outcome.LineageEndpoint {
	return outcome.LineageEndpoint{Tenant: tenant, Type: t, BusinessID: id, Revision: rev}
}

func TestCrossTenantEdgeRejected(t *testing.T) {
	e := outcome.LineageEdge{
		Tenant: "ent-1", Type: outcome.EdgeContributesTo,
		From: validEndpoint("ent-1", outcome.NodeContributor, "agent:x", 1),
		To:   validEndpoint("ent-2", outcome.NodeOutcome, "case-42.goal", 1), // different tenant
	}
	if err := e.Validate(); !errors.Is(err, outcome.ErrCrossTenantEdge) {
		t.Fatalf("cross-tenant edge: got %v, want ErrCrossTenantEdge", err)
	}
}

func TestSameTenantEdgeValidates(t *testing.T) {
	e := outcome.LineageEdge{
		Tenant: "ent-1", Type: outcome.EdgeContributesTo,
		From: validEndpoint("ent-1", outcome.NodeContributor, "agent:x", 1),
		To:   validEndpoint("ent-1", outcome.NodeOutcome, "case-42.goal", 1),
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("same-tenant edge should validate, got %v", err)
	}
}

// --- Constraint: lineage endpoints are always versioned. -------------------

func TestUnversionedLineageEndpointRejected(t *testing.T) {
	n := outcome.LineageNode{Tenant: "ent-1", Type: outcome.NodeOutcome, BusinessID: "case-42.goal", Revision: 0}
	if err := n.Validate(); !errors.Is(err, outcome.ErrUnversionedEndpoint) {
		t.Fatalf("node with revision 0: got %v, want ErrUnversionedEndpoint", err)
	}
	e := outcome.LineageEdge{
		Tenant: "ent-1", Type: outcome.EdgeConcerns,
		From: validEndpoint("ent-1", outcome.NodeOutcome, "case-42.goal", 1),
		To:   outcome.LineageEndpoint{Tenant: "ent-1", Type: outcome.NodeGoal, BusinessID: "g", Revision: 0}, // unversioned
	}
	if err := e.Validate(); !errors.Is(err, outcome.ErrUnversionedEndpoint) {
		t.Fatalf("edge with an unversioned endpoint: got %v, want ErrUnversionedEndpoint", err)
	}
}

func TestClosedNodeAndEdgeEnums(t *testing.T) {
	if outcome.NodeType("cypher_internal").Valid() {
		t.Fatal("node types are a closed enum")
	}
	if outcome.EdgeType("mystery").Valid() {
		t.Fatal("edge types are a closed enum")
	}
	n := outcome.LineageNode{Tenant: "ent-1", Type: outcome.NodeType("not_a_type"), BusinessID: "x", Revision: 1}
	if err := n.Validate(); err == nil {
		t.Fatal("a node with an unknown type must be rejected")
	}
}

// --- Constraint 6: opaque payloads only (no connector/credential/raw content).

func TestOutcomeRejectsConnectorShapedContent(t *testing.T) {
	o := validOutcome()
	o.Claim.Evidence = []outcome.EvidenceRef{{Handle: "jdbc:sqlserver://mes-db:1433;user id=sa;password=secret", ContentHash: "sha256:x", Authority: "a"}}
	if err := o.Validate(); !errors.Is(err, outcome.ErrRawContentLeak) {
		t.Fatalf("connector/credential-shaped content in an outcome: got %v, want ErrRawContentLeak", err)
	}
}

func TestValidOutcomeHasNoConnectorShapes(t *testing.T) {
	raw, err := json.Marshal(validOutcome())
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"http://", "https://", "jdbc:", "sqlserver://", "SELECT ", "api/", "password="} {
		if strings.Contains(string(raw), shape) {
			t.Fatalf("valid outcome fixture leaks connector-shaped content %q", shape)
		}
	}
}

// --- Constraint 8: no duplicate credit. ------------------------------------

func TestDuplicateContributionRejected(t *testing.T) {
	o := validOutcome()
	o.Contributions = []outcome.ContributionRef{
		{ContributorID: "agent:planner", Kind: "method"},
		{ContributorID: "agent:planner", Kind: "method"}, // same contributor + kind twice
	}
	if err := o.Validate(); !errors.Is(err, outcome.ErrDuplicateContribution) {
		t.Fatalf("duplicate contribution: got %v, want ErrDuplicateContribution", err)
	}
}

func TestDistinctContributionsAllowed(t *testing.T) {
	o := validOutcome()
	o.Contributions = []outcome.ContributionRef{
		{ContributorID: "agent:planner", Kind: "method"},
		{ContributorID: "agent:planner", Kind: "observation"}, // same contributor, different kind
		{ContributorID: "human:reviewer", Kind: "method"},
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("distinct contributions should be allowed, got %v", err)
	}
}

// --- Constraint 7: ProjectionEvent + Watermark shape. ----------------------

func TestProjectionEventValidates(t *testing.T) {
	ev := outcome.ProjectionEvent{
		Tenant: "ent-1", Sequence: 1, Kind: outcome.ProjectionOutcomeRevision,
		SubjectType: outcome.NodeOutcome, SubjectID: "ol_abc", SubjectRevision: 1,
		PayloadHash: "sha256:1", RecordedAt: time.Now().UTC(),
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("valid projection event rejected: %v", err)
	}
	if outcome.ProjectionEventKind("delete_in_place").Valid() {
		t.Fatal("projection event kinds are a closed enum (no in-place edit kind)")
	}
}

func TestProjectionEventUnknownKindRejected(t *testing.T) {
	ev := outcome.ProjectionEvent{
		Tenant: "ent-1", Sequence: 1, Kind: outcome.ProjectionEventKind("mutate"),
		SubjectType: outcome.NodeOutcome, SubjectID: "ol_abc", SubjectRevision: 1,
		PayloadHash: "sha256:1", RecordedAt: time.Now().UTC(),
	}
	if err := ev.Validate(); err == nil {
		t.Fatal("projection event with an unknown kind must be rejected")
	}
}

func TestProjectionWatermarkValidates(t *testing.T) {
	w := outcome.ProjectionWatermark{Tenant: "ent-1", LastSequence: 7, UpdatedAt: time.Now().UTC()}
	if err := w.Validate(); err != nil {
		t.Fatalf("valid watermark rejected: %v", err)
	}
}

func TestOutcomeStatusTerminalClassification(t *testing.T) {
	if !outcome.OutcomeSatisfied.IsTerminal() || !outcome.OutcomeUnsatisfied.IsTerminal() {
		t.Fatal("satisfied and unsatisfied are terminal conclusions")
	}
	for _, s := range []outcome.OutcomeStatus{outcome.OutcomeUnverified, outcome.OutcomeDisputed, outcome.OutcomeBlocked, outcome.OutcomeUnknown} {
		if s.IsTerminal() {
			t.Fatalf("%q retains a forward path and is not terminal", s)
		}
	}
}
