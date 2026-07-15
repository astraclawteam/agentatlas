package assessment

import (
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// --- fixtures ---------------------------------------------------------------

func goodGraphSchema() GraphSchema {
	return GraphSchema{Provider: "apache-age", SchemaVersion: "1", ProtocolVersion: "1", GraphName: "outcomegraph", Watermark: 42}
}

func tier1Evidence() Evidence {
	return Evidence{Tier: TierVerifiedOutcome, Handle: "outcome-1", Kind: "outcome", Revision: 3, Authority: "mes.official", SignatureKeyID: "key-1", Verified: true, Summary: "satisfied throughput outcome"}
}

func assessedDimension(key DimensionKey) DimensionResult {
	return DimensionResult{
		Dimension:          key,
		State:              StateAssessed,
		Attribution:        AttrSharedOnce,
		CountedOutcomeKeys: []string{"outcome-1"},
		SatisfiedOutcomes:  1,
		Evidence:           []Evidence{tier1Evidence()},
		Confidence:         goodConfidence(),
	}
}

func goodConfidence() ConfidenceExplanation {
	return ConfidenceExplanation{
		Level: ConfidenceHigh,
		Components: []ConfidenceComponent{
			{Kind: ConfCoverage, Score: 1},
			{Kind: ConfAuthority, Score: 1},
			{Kind: ConfFreshness, Score: 1},
			{Kind: ConfConsistency, Score: 1},
			{Kind: ConfAttributionClarity, Score: 1},
			{Kind: ConfProjectionFreshness, Score: 1},
			{Kind: ConfRuleClarity, Score: 1},
		},
	}
}

func goodAssessment() WorkAssessment {
	return WorkAssessment{
		Tenant:         "ent-b2",
		Org:            "org-line-1",
		Subject:        "actor:operator-7",
		Level:          LevelIndividual,
		PolicyKey:      "assess.assembly.operator",
		PolicyRevision: 1,
		Version:        1,
		Formal:         true,
		OrgVersion:     3,
		Sources: SourceBinding{
			OrgVersion:        3,
			JobResponsibility: VersionedRef{Key: "resp.assembly.operator", Version: 4},
			OrgGoal:           VersionedRef{Key: "goal.assembly.throughput", Version: 2},
			AcceptanceReview:  VersionedRef{Key: "review.assembly.qc", Version: 1},
		},
		Period:     Period{Start: fixed().Add(-720 * time.Hour), End: fixed()},
		Graph:      goodGraphSchema(),
		Dimensions: []DimensionResult{assessedDimension(DimOutcomeCompletion)},
		Manager:    ManagerConfirmation{Confirmed: true, Manager: "mgr:line-lead", ConfirmedAt: fixed()},
		CreatedAt:  fixed(),
	}
}

// --- evidence tiers (invariant 2) -------------------------------------------

func TestEvidenceTierRequiresVerifiedSignedForOutcome(t *testing.T) {
	// Tier 1 (verified outcome) must be verified AND carry a signing authority +
	// key: it is backed by a business-system fact and a SIGNED ObservationReceipt.
	e := tier1Evidence()
	if err := e.Validate(); err != nil {
		t.Fatalf("valid tier-1 evidence rejected: %v", err)
	}
	unsigned := e
	unsigned.SignatureKeyID = ""
	if err := unsigned.Validate(); err == nil {
		t.Fatal("tier-1 verified_outcome without a signature must be rejected")
	}
	unverified := e
	unverified.Verified = false
	if err := unverified.Validate(); err == nil {
		t.Fatal("tier-1 verified_outcome that is not verified must be rejected")
	}
}

func TestEvidenceTierDeliverableRequiresSignedExecution(t *testing.T) {
	e := Evidence{Tier: TierAcceptedDeliverable, Handle: "wc-1", Kind: "deliverable", SignatureKeyID: "key-1", Verified: true, Summary: "accepted deliverable"}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid tier-2 evidence rejected: %v", err)
	}
	unsigned := e
	unsigned.SignatureKeyID = ""
	if err := unsigned.Validate(); err == nil {
		t.Fatal("tier-2 accepted_deliverable without signed execution evidence must be rejected")
	}
}

func TestEvidenceTierHumanReportRequiresAuthority(t *testing.T) {
	e := Evidence{Tier: TierHumanReport, Handle: "hr-1", Kind: "report", Authority: "line-lead", Summary: "verbal status"}
	if err := e.Validate(); err != nil {
		t.Fatalf("valid tier-3 evidence rejected: %v", err)
	}
	anon := e
	anon.Authority = ""
	if err := anon.Validate(); err == nil {
		t.Fatal("tier-3 human_report without a named authority must be rejected")
	}
}

// --- blocker / delay closed enums (invariant 4) -----------------------------

func TestBlockerAndDelayEnumsClosed(t *testing.T) {
	for _, c := range []BlockerConfidence{BlockerVerified, BlockerCorroborated, BlockerReported, BlockerInferred} {
		if !c.Valid() {
			t.Fatalf("blocker confidence %q must be valid", c)
		}
	}
	if BlockerConfidence("guessed").Valid() {
		t.Fatal("an unknown blocker confidence must be invalid")
	}
	for _, d := range []DelayAttribution{DelayExternal, DelayPersonal, DelayProcess, DelayResource, DelayUnattributed} {
		if !d.Valid() {
			t.Fatalf("delay attribution %q must be valid", d)
		}
	}
	if DelayAttribution("character").Valid() {
		t.Fatal("an unknown delay attribution must be invalid")
	}
	b := Blocker{Handle: "blk-1", Kind: "dependency", Confidence: BlockerVerified, Delay: DelayExternal, Summary: "upstream late"}
	if err := b.Validate(); err != nil {
		t.Fatalf("valid blocker rejected: %v", err)
	}
	bad := b
	bad.Confidence = "guessed"
	if err := bad.Validate(); err == nil {
		t.Fatal("a blocker with an invalid confidence must be rejected")
	}
}

// --- not_assessable is a first-class state (invariant 5) --------------------

func TestDimensionResultNotAssessableShape(t *testing.T) {
	na := DimensionResult{
		Dimension:           DimTimeliness,
		State:               StateNotAssessable,
		NotAssessableReason: ReasonInsufficientData,
		Attribution:         AttrSharedOnce,
		Confidence:          goodConfidence(),
	}
	if err := na.Validate(); err != nil {
		t.Fatalf("valid not_assessable dimension rejected: %v", err)
	}
	// not_assessable must NOT carry an invented score (counted outcomes / satisfied).
	bad := na
	bad.CountedOutcomeKeys = []string{"o1"}
	bad.SatisfiedOutcomes = 1
	if err := bad.Validate(); err == nil {
		t.Fatal("a not_assessable dimension must never carry counted/satisfied outcomes (an invented score)")
	}
	// not_assessable must name a reason.
	noReason := na
	noReason.NotAssessableReason = ""
	if err := noReason.Validate(); err == nil {
		t.Fatal("a not_assessable dimension must name a reason")
	}
	// an assessed dimension must NOT carry a not_assessable reason.
	assessed := assessedDimension(DimQuality)
	assessed.NotAssessableReason = ReasonStaleGraph
	if err := assessed.Validate(); err == nil {
		t.Fatal("an assessed dimension must not carry a not_assessable reason")
	}
}

// --- WorkAssessment validate + deterministic id -----------------------------

func TestWorkAssessmentValidateAndDerivedID(t *testing.T) {
	w := goodAssessment()
	if err := w.Validate(); err != nil {
		t.Fatalf("valid assessment rejected: %v", err)
	}
	id := w.DerivedID()
	if !strings.HasPrefix(id, "wa_") {
		t.Fatalf("derived id must be provider-neutral (wa_ prefix), got %q", id)
	}
	// Identity derives only from tenant/subject/policy_key/revision/period: a
	// changed narrative or manager note must NOT change the id.
	w2 := goodAssessment()
	w2.Narrative = "a human narrative"
	w2.Manager.Manager = "mgr:someone-else"
	if w2.DerivedID() != id {
		t.Fatal("derived id must not depend on narrative or manager note")
	}
	// A different period is a different assessment.
	w3 := goodAssessment()
	w3.Period.End = w3.Period.End.Add(24 * time.Hour)
	if w3.DerivedID() == id {
		t.Fatal("a different period must yield a different assessment id")
	}
}

func TestWorkAssessmentRejectsRawContentLeak(t *testing.T) {
	w := goodAssessment()
	w.Dimensions[0].Evidence[0].Summary = "jdbc:postgresql://db/x"
	if err := w.Validate(); !errors.Is(err, ErrRawContentLeak) {
		t.Fatalf("a connector-shaped summary must be rejected, got %v", err)
	}
}

// --- determinism: byte-equal after shuffling every input collection ---------

func TestWorkAssessmentNormalizeShuffleInvariant(t *testing.T) {
	base := deepAssessment()
	want, err := json.Marshal(base.Normalize())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 25; i++ {
		shuffled := shuffleAssessment(base, rng)
		got, err := json.Marshal(shuffled.Normalize())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("Normalize is not shuffle-invariant:\n want %s\n  got %s", want, got)
		}
	}
}

func TestWorkAssessmentCountedOutcomeKeysUnion(t *testing.T) {
	child := goodAssessment()
	child.Subject = "actor:report-1"
	child.Dimensions[0].CountedOutcomeKeys = []string{"outcome-child"}
	parent := goodAssessment()
	parent.Dimensions[0].CountedOutcomeKeys = []string{"outcome-parent"}
	parent.SubAssessments = []WorkAssessment{child}
	keys := parent.CountedOutcomeKeys()
	if len(keys) != 2 || keys[0] != "outcome-child" || keys[1] != "outcome-parent" {
		t.Fatalf("CountedOutcomeKeys must be the sorted union across dimensions and children, got %v", keys)
	}
}

// --- deep/shuffle helpers ---------------------------------------------------

// deepAssessment is a multi-dimension, multi-evidence, hierarchical assessment
// used to exercise deterministic normalization of every nested collection. It
// deliberately includes TIED sort keys — elements that share the readable partial
// key but differ in remaining content — so the total-order tiebreak is exercised:
//   - two human_report evidence items sharing Handle "hr" but different authority,
//   - two blockers sharing Handle "blk" but different confidence/delay,
//   - two contributions sharing (contributor,outcome,kind) but different weight,
//   - two sub-assessments with an EQUAL DerivedID but different content.
func deepAssessment() WorkAssessment {
	w := goodAssessment()
	d1 := DimensionResult{
		Dimension:          DimOutcomeCompletion,
		State:              StateAssessed,
		Attribution:        AttrContributionWeighted,
		CountedOutcomeKeys: []string{"o-b", "o-a", "o-c"},
		SatisfiedOutcomes:  2,
		Evidence: []Evidence{
			{Tier: TierHumanReport, Handle: "hr-2", Kind: "report", Authority: "peer"},
			{Tier: TierVerifiedOutcome, Handle: "o-a", Kind: "outcome", Revision: 1, Authority: "mes", SignatureKeyID: "k", Verified: true},
			{Tier: TierAcceptedDeliverable, Handle: "wc-9", Kind: "deliverable", SignatureKeyID: "k2", Verified: true},
			// TIED on (Tier, Handle, Revision) — differ only in Authority/Summary.
			{Tier: TierHumanReport, Handle: "hr", Kind: "report", Authority: "peer-a"},
			{Tier: TierHumanReport, Handle: "hr", Kind: "report", Authority: "peer-b", Summary: "second source"},
		},
		Blockers: []Blocker{
			{Handle: "blk-z", Kind: "dep", Confidence: BlockerReported, Delay: DelayExternal},
			{Handle: "blk-a", Kind: "proc", Confidence: BlockerInferred, Delay: DelayProcess},
			// TIED on Handle — differ in Confidence/Delay.
			{Handle: "blk", Kind: "dep", Confidence: BlockerVerified, Delay: DelayExternal},
			{Handle: "blk", Kind: "proc", Confidence: BlockerInferred, Delay: DelayResource},
		},
		Contributions: []Contribution{
			{ContributorID: "c-2", OutcomeKey: "o-b", Kind: "review"},
			{ContributorID: "c-1", OutcomeKey: "o-a", Kind: "author", Weight: 0.5},
			// TIED on (contributor,outcome,kind) — differ in weight/plan_external.
			{ContributorID: "c-3", OutcomeKey: "o-c", Kind: "review", Weight: 0.2},
			{ContributorID: "c-3", OutcomeKey: "o-c", Kind: "review", Weight: 0.9, PlanExternal: true},
		},
		Confidence: goodConfidence(),
	}
	d2 := assessedDimension(DimQuality)
	child := goodAssessment()
	child.Subject = "actor:report-1"
	child.Formal = false
	child.Level = LevelIndividual
	child2 := goodAssessment()
	child2.Subject = "actor:report-2"
	child2.Formal = false
	// Two sub-assessments with an EQUAL DerivedID (same subject/policy/version/
	// period) but DIFFERENT content — tied on DerivedID, broken by the JSON tiebreak.
	dupA := goodAssessment()
	dupA.Subject = "actor:report-dup"
	dupA.Formal = false
	dupA.Dimensions = []DimensionResult{assessedDimension(DimOutcomeCompletion)}
	dupB := goodAssessment()
	dupB.Subject = "actor:report-dup"
	dupB.Formal = false
	dupB.Dimensions = []DimensionResult{assessedDimension(DimQuality)}
	if dupA.DerivedID() != dupB.DerivedID() {
		panic("test setup: dupA and dupB must share a DerivedID to exercise the tiebreak")
	}
	w.Level = LevelGroup
	w.Dimensions = []DimensionResult{d1, d2}
	w.SubAssessments = []WorkAssessment{child, child2, dupA, dupB}
	w.Sources.SOPs = []VersionedRef{{Key: "sop.b", Version: 2}, {Key: "sop.a", Version: 1}}
	return w
}

func shuffleAssessment(w WorkAssessment, rng *rand.Rand) WorkAssessment {
	out := w.clone()
	rng.Shuffle(len(out.Dimensions), func(i, j int) { out.Dimensions[i], out.Dimensions[j] = out.Dimensions[j], out.Dimensions[i] })
	for di := range out.Dimensions {
		d := &out.Dimensions[di]
		rng.Shuffle(len(d.Evidence), func(i, j int) { d.Evidence[i], d.Evidence[j] = d.Evidence[j], d.Evidence[i] })
		rng.Shuffle(len(d.Blockers), func(i, j int) { d.Blockers[i], d.Blockers[j] = d.Blockers[j], d.Blockers[i] })
		rng.Shuffle(len(d.Contributions), func(i, j int) { d.Contributions[i], d.Contributions[j] = d.Contributions[j], d.Contributions[i] })
		rng.Shuffle(len(d.CountedOutcomeKeys), func(i, j int) {
			d.CountedOutcomeKeys[i], d.CountedOutcomeKeys[j] = d.CountedOutcomeKeys[j], d.CountedOutcomeKeys[i]
		})
		rng.Shuffle(len(d.Confidence.Components), func(i, j int) {
			d.Confidence.Components[i], d.Confidence.Components[j] = d.Confidence.Components[j], d.Confidence.Components[i]
		})
	}
	rng.Shuffle(len(out.SubAssessments), func(i, j int) {
		out.SubAssessments[i], out.SubAssessments[j] = out.SubAssessments[j], out.SubAssessments[i]
	})
	rng.Shuffle(len(out.Sources.SOPs), func(i, j int) { out.Sources.SOPs[i], out.Sources.SOPs[j] = out.Sources.SOPs[j], out.Sources.SOPs[i] })
	return out
}
