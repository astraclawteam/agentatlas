package assessment

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fixed() time.Time { return time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC) }

// sixBuiltins returns one non-formal candidate Dimension per built-in key with a
// clean, evidence-grounded rationale (never a forbidden category).
func sixBuiltins() []Dimension {
	titles := map[DimensionKey]string{
		DimOutcomeCompletion:       "Outcome completion",
		DimQuality:                 "Delivered quality",
		DimTimeliness:              "Timeliness",
		DimCollaboration:           "Collaboration and handoff",
		DimRiskCompliance:          "Risk and compliance",
		DimImprovementContribution: "Improvement contribution",
	}
	var out []Dimension
	for _, k := range BuiltinDimensionKeys() {
		out = append(out, Dimension{
			Key:       k,
			Title:     titles[k],
			Formal:    true,
			Rationale: "proposed from confirmed work outcomes bound at the graph watermark",
			Evidence:  []EvidenceLink{{Handle: "ol_out1", Kind: "outcome", Summary: "satisfied outcome"}},
		})
	}
	return out
}

func rulesFor(dims []Dimension, level ConfidenceLevel) ([]EvidenceRule, []AttributionRule, []ConfidenceRule) {
	var ev []EvidenceRule
	var at []AttributionRule
	var cf []ConfidenceRule
	for _, d := range dims {
		ev = append(ev, EvidenceRule{Dimension: d.Key, Tier: TierVerifiedOutcome, Rationale: "grounded in a signed observation"})
		at = append(at, AttributionRule{Dimension: d.Key, Mode: AttrSharedOnce, Rationale: "shared outcomes attributed once"})
		cf = append(cf, ConfidenceRule{Dimension: d.Key, Level: level, Rationale: "coverage is high across the period"})
	}
	return ev, at, cf
}

func goodPolicy(level ConfidenceLevel) Policy {
	dims := sixBuiltins()
	ev, at, cf := rulesFor(dims, level)
	return Policy{
		Tenant:    "ent-b2",
		Org:       "org-line-1",
		PolicyKey: "assess.assembly.operator",
		Revision:  1,
		Status:    StatusDraft,
		Sources: SourceBinding{
			OrgVersion:        3,
			JobResponsibility: VersionedRef{Key: "resp.assembly.operator", Version: 4},
			OrgGoal:           VersionedRef{Key: "goal.assembly.throughput", Version: 2},
			SOPs:              []VersionedRef{{Key: "sop.assembly.safety", Version: 7}},
			AcceptanceReview:  VersionedRef{Key: "review.assembly.qc", Version: 1},
		},
		Watermark:        42,
		Dimensions:       dims,
		EvidenceRules:    ev,
		AttributionRules: at,
		ConfidenceRules:  cf,
		CreatedAt:        fixed(),
		UpdatedAt:        fixed(),
	}
}

func TestPolicyStatusClosedSet(t *testing.T) {
	for _, s := range Statuses() {
		if !s.Valid() {
			t.Fatalf("status %q reported invalid", s)
		}
	}
	if Status("scoring").Valid() {
		t.Fatal("unknown status accepted")
	}
	if !StatusPublished.IsTerminal() || !StatusRetired.IsTerminal() {
		t.Fatal("published/retired must be terminal (frozen revision)")
	}
	if StatusDraft.IsTerminal() || StatusShadow.IsTerminal() || StatusReviewing.IsTerminal() {
		t.Fatal("non-terminal status reported terminal")
	}
}

func TestPolicyBuiltinDimensionsAreTheSix(t *testing.T) {
	got := BuiltinDimensionKeys()
	if len(got) != 6 {
		t.Fatalf("want 6 built-in candidate dimensions, got %d", len(got))
	}
	want := map[DimensionKey]bool{
		DimOutcomeCompletion: true, DimQuality: true, DimTimeliness: true,
		DimCollaboration: true, DimRiskCompliance: true, DimImprovementContribution: true,
	}
	for _, k := range got {
		if !k.IsBuiltin() {
			t.Fatalf("built-in %q not reported builtin", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("missing built-in dimensions: %v", want)
	}
}

func TestPolicyForbiddenDimensionRejectedBySDK(t *testing.T) {
	cases := []struct {
		name string
		dim  Dimension
	}{
		{"personality", Dimension{Key: "personality", Title: "Personality fit", Rationale: "team culture"}},
		{"loyalty", Dimension{Key: "loyalty", Title: "Company loyalty", Rationale: "retention"}},
		{"private_life", Dimension{Key: "private_life", Title: "Private life conduct", Rationale: "off hours"}},
		{"hidden_flag", Dimension{Key: DimQuality, Title: "Quality", Hidden: true, Rationale: "grounded in outcomes"}},
		{"hidden_in_rationale", Dimension{Key: "engagement", Title: "Engagement", Rationale: "secretly scores personal loyalty"}},
		{"religion", Dimension{Key: "belief", Title: "Religious observance", Rationale: "attendance"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.dim.Validate(); err == nil {
				t.Fatalf("forbidden dimension %q accepted by SDK Validate", tc.name)
			} else if !strings.Contains(err.Error(), "forbidden") {
				t.Fatalf("want forbidden error, got %v", err)
			}
		})
	}
}

func TestPolicyBuiltinAndGovernedDimensionsAccepted(t *testing.T) {
	for _, d := range sixBuiltins() {
		if err := d.Validate(); err != nil {
			t.Fatalf("built-in dimension %q rejected: %v", d.Key, err)
		}
	}
	governed := Dimension{Key: "safety_incident_followup", Title: "Safety incident follow-up", Formal: true, Rationale: "grounded in accepted corrective outcomes"}
	if err := governed.Validate(); err != nil {
		t.Fatalf("governed non-forbidden addition rejected: %v", err)
	}
}

func TestPolicyValidateAndDeterministicID(t *testing.T) {
	p := goodPolicy(ConfidenceHigh)
	if err := p.Validate(); err != nil {
		t.Fatalf("valid draft policy rejected: %v", err)
	}
	if p.DerivedID() != p.DerivedID() {
		t.Fatal("DerivedID not deterministic")
	}
	q := p
	q.Revision = 2
	if p.DerivedID() == q.DerivedID() {
		t.Fatal("different revisions must derive different ids")
	}
}

func TestPolicyRuleDigestChangesOnRuleEdit(t *testing.T) {
	p := goodPolicy(ConfidenceHigh)
	base := p.RuleDigest()
	if base == "" {
		t.Fatal("rule digest empty")
	}
	// A non-rule field (status) must NOT change the digest.
	q := p
	q.Status = StatusShadow
	if q.RuleDigest() != base {
		t.Fatal("status change must not alter the rule digest")
	}
	// Editing a confidence rule MUST change the digest (a rule edit).
	r := goodPolicy(ConfidenceLow)
	if r.RuleDigest() == base {
		t.Fatal("editing a confidence rule must change the rule digest")
	}
	// Adding a dimension MUST change the digest.
	s := goodPolicy(ConfidenceHigh)
	s.Dimensions = append(s.Dimensions, Dimension{Key: "safety", Title: "Safety", Rationale: "grounded in outcomes"})
	if s.RuleDigest() == base {
		t.Fatal("adding a dimension must change the rule digest")
	}
}

func TestPolicyFormalScoringOnlyWhenPublished(t *testing.T) {
	for _, st := range []Status{StatusDraft, StatusShadow, StatusReviewing} {
		p := goodPolicy(ConfidenceHigh)
		p.Status = st
		if p.FormalScoringActive() {
			t.Fatalf("status %q must not activate formal scoring", st)
		}
		if err := p.AssertFormalScoringAllowed(); err == nil {
			t.Fatalf("status %q must reject formal scoring", st)
		}
	}
	p := goodPolicy(ConfidenceHigh)
	p.Status = StatusPublished
	if !p.FormalScoringActive() {
		t.Fatal("published policy must activate formal scoring")
	}
	if err := p.AssertFormalScoringAllowed(); err != nil {
		t.Fatalf("published policy rejected formal scoring: %v", err)
	}
}

func completeCycle(p Policy) ShadowCycle {
	return ShadowCycle{
		Cycle:                  1,
		Revision:               p.Revision,
		RuleDigest:             p.RuleDigest(),
		PeriodStart:            fixed(),
		PeriodEnd:              fixed().Add(720 * time.Hour),
		StartedAt:              fixed(),
		CompletedAt:            fixed().Add(721 * time.Hour),
		Watermark:              p.Watermark,
		EvidenceCoverage:       0.95,
		UnresolvedCorrections:  0,
		UnresolvedCalibrations: 0,
		ManagerConfirmed:       true,
		PolicyOwnerConfirmed:   true,
	}
}

func TestPolicyPublicationGateEveryBranch(t *testing.T) {
	base := goodPolicy(ConfidenceHigh)

	// All gates satisfied -> publishable (no failing reasons).
	if got := EvaluatePublicationGate(base, completeCycle(base)); len(got) != 0 {
		t.Fatalf("all-gate-pass cycle reported failures: %v", got)
	}

	// 90% boundary: exactly 0.90 passes, 0.899 fails.
	edge := completeCycle(base)
	edge.EvidenceCoverage = MinEvidenceCoverage
	if got := EvaluatePublicationGate(base, edge); len(got) != 0 {
		t.Fatalf("coverage exactly at threshold must pass: %v", got)
	}
	edge.EvidenceCoverage = MinEvidenceCoverage - 0.001
	if !hasReason(EvaluatePublicationGate(base, edge), GateCoverageBelowThreshold) {
		t.Fatal("coverage below 90% must fail the coverage gate")
	}

	// Low confidence on a FORMAL dimension fails.
	lowPolicy := goodPolicy(ConfidenceLow)
	if !hasReason(EvaluatePublicationGate(lowPolicy, completeCycle(lowPolicy)), GateLowConfidenceFormal) {
		t.Fatal("a formal dimension with low confidence must fail the gate")
	}

	// Post-start rule change fails.
	changed := completeCycle(base)
	changed.RuleChangedAfterStart = true
	if !hasReason(EvaluatePublicationGate(base, changed), GateRuleChangedAfterStart) {
		t.Fatal("a rule change after the cycle began must fail the gate")
	}
	// A cycle bound to a stale rule digest also fails.
	staleDigest := completeCycle(base)
	staleDigest.RuleDigest = "sha256:stale"
	if !hasReason(EvaluatePublicationGate(base, staleDigest), GateRuleChangedAfterStart) {
		t.Fatal("a cycle whose bound rule digest differs must fail the rule-change gate")
	}

	// Unresolved correction fails.
	corr := completeCycle(base)
	corr.UnresolvedCorrections = 1
	if !hasReason(EvaluatePublicationGate(base, corr), GateUnresolvedCorrection) {
		t.Fatal("an unresolved correction must fail the gate")
	}
	// Unresolved calibration fails.
	cal := completeCycle(base)
	cal.UnresolvedCalibrations = 1
	if !hasReason(EvaluatePublicationGate(base, cal), GateUnresolvedCalibration) {
		t.Fatal("an unresolved calibration must fail the gate")
	}

	// Manager not confirmed fails.
	noMgr := completeCycle(base)
	noMgr.ManagerConfirmed = false
	if !hasReason(EvaluatePublicationGate(base, noMgr), GateManagerNotConfirmed) {
		t.Fatal("missing manager confirmation must fail the gate")
	}
	// Policy owner not confirmed fails.
	noOwner := completeCycle(base)
	noOwner.PolicyOwnerConfirmed = false
	if !hasReason(EvaluatePublicationGate(base, noOwner), GatePolicyOwnerNotConfirmed) {
		t.Fatal("missing policy-owner confirmation must fail the gate")
	}

	// Fail closed: an incomplete cycle never publishes.
	incomplete := completeCycle(base)
	incomplete.CompletedAt = time.Time{}
	if len(EvaluatePublicationGate(base, incomplete)) == 0 {
		t.Fatal("an incomplete shadow cycle must never satisfy the gate (fail closed)")
	}
}

func TestPolicyShadowCycleNeverFormal(t *testing.T) {
	c := completeCycle(goodPolicy(ConfidenceHigh))
	if c.IsFormal() {
		t.Fatal("shadow-cycle output must be explicitly non-formal")
	}
}

func hasReason(reasons []GateReason, want GateReason) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// --- FIX-B: hardened best-effort fairness screen ----------------------------

func TestPolicyForbiddenScreeningHardening(t *testing.T) {
	t.Run("double_separator_in_title", func(t *testing.T) {
		// "Private  life" (double space) evaded the single-separator pattern; it
		// must now be rejected after separator normalization.
		d := Dimension{Key: "engagement", Title: "Private  life", Rationale: "grounded in confirmed outcomes"}
		if err := d.Validate(); !errors.Is(err, ErrForbiddenDimension) {
			t.Fatalf("double-separator 'Private  life' title = %v, want ErrForbiddenDimension", err)
		}
	})
	t.Run("forbidden_in_rule_rationale", func(t *testing.T) {
		// A forbidden subject hiding in a rule rationale must be rejected too.
		if err := (EvidenceRule{Dimension: DimQuality, Tier: TierVerifiedOutcome, Rationale: "secretly rates personal loyalty"}).Validate(); !errors.Is(err, ErrForbiddenDimension) {
			t.Fatalf("forbidden term in evidence-rule rationale = %v, want ErrForbiddenDimension", err)
		}
		if err := (ConfidenceRule{Dimension: DimQuality, Level: ConfidenceHigh, Rationale: "weighted by religious observance"}).Validate(); !errors.Is(err, ErrForbiddenDimension) {
			t.Fatalf("forbidden term in confidence-rule rationale = %v, want ErrForbiddenDimension", err)
		}
	})
	t.Run("homoglyph_key_rejected", func(t *testing.T) {
		// A Cyrillic 'а' (U+0430) in the key is not a lowercase ASCII identifier.
		d := Dimension{Key: "quаlity", Title: "Quality", Rationale: "grounded in confirmed outcomes"}
		if err := d.Validate(); !errors.Is(err, ErrInvalidDimensionKey) {
			t.Fatalf("homoglyph key = %v, want ErrInvalidDimensionKey", err)
		}
	})
	t.Run("i18n_title_and_rationale_accepted", func(t *testing.T) {
		// A legitimate dimension with a Chinese title AND rationale (ASCII key) must
		// stay valid — the screen must not break i18n.
		d := Dimension{Key: "safety_incident_followup", Title: "安全事件跟进", Formal: true, Rationale: "依据已确认的纠正措施结果，围绕工作成效进行评估"}
		if err := d.Validate(); err != nil {
			t.Fatalf("legitimate i18n dimension rejected: %v", err)
		}
	})
}

// --- FIX-C: RuleDigest order-independence and duplicate-rule rejection -------

func TestPolicyRuleDigestOrderIndependent(t *testing.T) {
	p := goodPolicy(ConfidenceHigh)
	base := p.RuleDigest()
	// The SAME evidence-rule set in the opposite order must digest identically.
	q := p
	q.EvidenceRules = append([]EvidenceRule(nil), p.EvidenceRules...)
	for i, j := 0, len(q.EvidenceRules)-1; i < j; i, j = i+1, j-1 {
		q.EvidenceRules[i], q.EvidenceRules[j] = q.EvidenceRules[j], q.EvidenceRules[i]
	}
	if len(q.EvidenceRules) < 2 {
		t.Fatal("fixture must have at least two evidence rules to prove order-independence")
	}
	if q.RuleDigest() != base {
		t.Fatal("reordering the evidence-rule set must not change the rule digest")
	}
}

func TestPolicyValidateRejectsDuplicateRulePerDimension(t *testing.T) {
	p := goodPolicy(ConfidenceHigh)
	// A second confidence rule for an already-covered dimension is rejected (it
	// would otherwise silently last-win in confidenceByDimension).
	p.ConfidenceRules = append(p.ConfidenceRules, p.ConfidenceRules[0])
	if err := p.Validate(); err == nil {
		t.Fatal("a duplicate confidence rule for one dimension must be rejected")
	} else if !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("want duplicate-rule error, got %v", err)
	}
}
