package assessment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// Task 18A Part B — production-conformance capstone over the outcome-grounded
// hierarchical distillation REALIZED in Task 18C (internal/assessment). It
// re-asserts the 18A production property list over synthetic MULTI-LEVEL records
// (individual → group → department → business-unit → company) against the REAL
// evaluator + result store, and adds the minimal assertions for any property not
// already covered by the 18C suites. It is TEST-ONLY: it re-architects nothing.
//
// The companion outcomelearning/production_conformance_test.go proves the same
// production property list over Task 0J (governed method/knowledge candidates).

// --- shared multi-level fixture --------------------------------------------

type prodFixture struct {
	Watermark uint64      `yaml:"watermark"`
	Hierarchy []prodLevel `yaml:"hierarchy"`
}

type prodLevel struct {
	Level    string   `yaml:"level"`
	Org      string   `yaml:"org"`
	Subject  string   `yaml:"subject"`
	Outcomes []string `yaml:"outcomes"`
}

func loadProductionCases(t *testing.T) prodFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "dream", "production-cases.yaml"))
	if err != nil {
		t.Fatalf("read production-cases fixture: %v", err)
	}
	var f prodFixture
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse production-cases fixture: %v", err)
	}
	if len(f.Hierarchy) != 5 {
		t.Fatalf("fixture must carry all five hierarchy levels, got %d", len(f.Hierarchy))
	}
	return f
}

// seedHierarchyOutcomes seeds every distinct fixture outcome as a satisfied,
// signed, authoritative Outcome and returns the matching typed graph candidates.
func seedHierarchyOutcomes(t *testing.T, store *outcome.MemoryStore, fx prodFixture) []outcomegraph.ResultNode {
	t.Helper()
	var nodes []outcomegraph.ResultNode
	seen := map[string]bool{}
	for _, lv := range fx.Hierarchy {
		for _, k := range lv.Outcomes {
			if seen[k] {
				continue
			}
			seen[k] = true
			seedOutcomeChain(t, store, tnt, fxOutcome{Key: k, Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
			nodes = append(nodes, outcomegraph.ResultNode{Label: outcomegraph.LabelOutcome, BusinessID: k, Revision: 1})
		}
	}
	return nodes
}

// buildAssessmentHierarchy evaluates each fixture level and nests it under the
// next, returning the top (company) assessment. individual is the leaf; each
// parent embeds the previous level as a sub-assessment.
func buildAssessmentHierarchy(t *testing.T, store *outcome.MemoryStore, graph *fakeGraph, fx prodFixture, shuffle *rand.Rand) model.WorkAssessment {
	t.Helper()
	ctx := context.Background()
	e := newEvaluator(t, graph, store)
	var top model.WorkAssessment
	var child *model.WorkAssessment
	for _, lv := range fx.Hierarchy {
		outcomes := append([]string(nil), lv.Outcomes...)
		if shuffle != nil {
			shuffle.Shuffle(len(outcomes), func(i, j int) { outcomes[i], outcomes[j] = outcomes[j], outcomes[i] })
		}
		req := EvaluateRequest{
			Tenant: tnt, Org: org, Subject: lv.Subject, Level: model.AssessmentLevel(lv.Level),
			Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
			Formal: true, Version: 1,
			Manager:         model.ManagerConfirmation{Confirmed: true, Manager: "mgr:line-lead", ConfirmedAt: fixedNow()},
			DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: outcomes}},
			DecidedAt:       fixedNow(),
		}
		if child != nil {
			req.SubAssessments = []model.WorkAssessment{*child}
		}
		wa, err := e.Evaluate(ctx, req)
		if err != nil {
			t.Fatalf("evaluate level %q: %v", lv.Level, err)
		}
		c := wa
		child = &c
		top = wa
	}
	return top
}

func flattenAssessments(wa model.WorkAssessment) []model.WorkAssessment {
	out := []model.WorkAssessment{wa}
	for _, sub := range wa.SubAssessments {
		out = append(out, flattenAssessments(sub)...)
	}
	return out
}

// ===========================================================================
// TestProductionConformance — the 18A Part-B property list over 18C.
// ===========================================================================

func TestProductionConformance(t *testing.T) {
	fx := loadProductionCases(t)
	ctx := context.Background()

	// --- hierarchy: individual → company, each bound to the EXACT watermark and
	//     immutable input/auth/consent (Source) versions -----------------------
	t.Run("hierarchy_all_levels_bound_to_exact_watermark_and_versions", func(t *testing.T) {
		store := outcome.NewMemoryStore(fixedNow)
		nodes := seedHierarchyOutcomes(t, store, fx)
		graph := &fakeGraph{wm: fx.Watermark, nodes: nodes}
		top := buildAssessmentHierarchy(t, store, graph, fx, nil)

		levels := flattenAssessments(top)
		if len(levels) != 5 {
			t.Fatalf("hierarchy must carry five nested levels, got %d", len(levels))
		}
		seenLevels := map[model.AssessmentLevel]bool{}
		for _, wa := range levels {
			if err := wa.Validate(); err != nil {
				t.Fatalf("level %q produced an invalid assessment: %v", wa.Level, err)
			}
			seenLevels[wa.Level] = true
			// Exact Outcome Graph watermark bound at every level.
			if wa.Graph.Watermark != fx.Watermark {
				t.Fatalf("level %q watermark = %d, want %d (exact)", wa.Level, wa.Graph.Watermark, fx.Watermark)
			}
			if wa.Graph.Provider != outcomegraph.ProviderName {
				t.Fatalf("level %q graph provider = %q, want %q", wa.Level, wa.Graph.Provider, outcomegraph.ProviderName)
			}
			// Immutable input/auth/consent versions (org + goal + responsibility).
			if wa.OrgVersion != sources().OrgVersion {
				t.Fatalf("level %q org version = %d, want %d", wa.Level, wa.OrgVersion, sources().OrgVersion)
			}
			if wa.Sources.OrgGoal.Version != goalVer || wa.Sources.JobResponsibility.Version != sources().JobResponsibility.Version {
				t.Fatalf("level %q must bind immutable source versions, got %+v", wa.Level, wa.Sources)
			}
			if wa.Dimensions[0].State != model.StateAssessed {
				t.Fatalf("level %q outcome_completion state = %q, want assessed", wa.Level, wa.Dimensions[0].State)
			}
		}
		for _, want := range model.AssessmentLevels() {
			if !seenLevels[want] {
				t.Fatalf("hierarchy is missing level %q (want individual→company)", want)
			}
		}
	})

	// --- cross-level duplicate contribution is prevented (attribute once) ------
	t.Run("cross_level_shared_outcome_counted_once", func(t *testing.T) {
		store := outcome.NewMemoryStore(fixedNow)
		nodes := seedHierarchyOutcomes(t, store, fx)
		graph := &fakeGraph{wm: fx.Watermark, nodes: nodes}
		top := buildAssessmentHierarchy(t, store, graph, fx, nil)

		// outcome-shared appears in BOTH the individual and group levels of the
		// fixture. Count its occurrences across the PER-LEVEL dimension results —
		// which are NOT de-duplicated across levels (unlike the aggregate
		// WorkAssessment.CountedOutcomeKeys() set) — so a cross-level double-count
		// is observable: the real guard (evaluator.go childCounted) must keep it
		// at exactly 1.
		count := 0
		for _, wa := range flattenAssessments(top) {
			for _, d := range wa.Dimensions {
				for _, k := range d.CountedOutcomeKeys {
					if k == "outcome-shared" {
						count++
					}
				}
			}
		}
		if count != 1 {
			t.Fatalf("shared outcome counted %d times across levels, want 1", count)
		}
	})

	// --- deterministic idempotency + shuffle-invariant replay ------------------
	t.Run("deterministic_idempotent_and_shuffle_invariant", func(t *testing.T) {
		build := func(shuffle *rand.Rand) []byte {
			store := outcome.NewMemoryStore(fixedNow)
			nodes := seedHierarchyOutcomes(t, store, fx)
			graph := &fakeGraph{wm: fx.Watermark, nodes: nodes}
			top := buildAssessmentHierarchy(t, store, graph, fx, shuffle)
			b, err := json.Marshal(top.Normalize())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			return b
		}
		want := build(nil)
		if !bytes.Equal(want, build(nil)) {
			t.Fatal("hierarchy build is not idempotent across identical runs")
		}
		rng := rand.New(rand.NewSource(20260715))
		for i := 0; i < 20; i++ {
			if got := build(rng); !bytes.Equal(want, got) {
				t.Fatalf("hierarchy is not shuffle-invariant on iteration %d", i)
			}
		}
	})

	// --- immutable, append-only versions + retention of prior versions ---------
	t.Run("immutable_append_only_versions_and_retention", func(t *testing.T) {
		store := outcome.NewMemoryStore(fixedNow)
		ind := fx.Hierarchy[0]
		for _, k := range ind.Outcomes {
			seedOutcomeChain(t, store, tnt, fxOutcome{Key: k, Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
		}
		graph := &fakeGraph{wm: fx.Watermark, nodes: candidateNodesForKeys(ind.Outcomes)}
		e := newEvaluator(t, graph, store)
		mkReq := func(version int) EvaluateRequest {
			return EvaluateRequest{
				Tenant: tnt, Org: org, Subject: ind.Subject, Level: model.LevelIndividual,
				Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
				Formal: true, Version: version,
				Manager:         model.ManagerConfirmation{Confirmed: true, Manager: "mgr:line-lead", ConfirmedAt: fixedNow()},
				DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: ind.Outcomes}},
				DecidedAt:       fixedNow(),
			}
		}
		v1wa, err := e.Evaluate(ctx, mkReq(1))
		if err != nil {
			t.Fatalf("evaluate v1: %v", err)
		}
		rs := NewMemoryResultStore(fixedNow)
		saved, err := rs.AppendAssessment(ctx, v1wa)
		if err != nil {
			t.Fatalf("append v1: %v", err)
		}
		if saved.ID != saved.DerivedID() {
			t.Fatalf("append must assign the deterministic id")
		}
		// Immutable: the same version can never be re-appended (no in-place edit).
		if _, err := rs.AppendAssessment(ctx, v1wa); !errors.Is(err, ErrRevisionExists) {
			t.Fatalf("duplicate append = %v, want ErrRevisionExists (append-only)", err)
		}
		// Re-assessment is a NEW version; the prior version is retained.
		v2wa, err := e.Evaluate(ctx, mkReq(2))
		if err != nil {
			t.Fatalf("evaluate v2: %v", err)
		}
		v2, err := rs.AppendAssessment(ctx, v2wa)
		if err != nil {
			t.Fatalf("append v2: %v", err)
		}
		if v2.ID == saved.ID {
			t.Fatal("a re-assessment must be a NEW immutable version with a new id")
		}
		got, err := rs.GetAssessment(ctx, saved.Tenant, saved.ID)
		if err != nil || got.ID != saved.ID {
			t.Fatalf("prior version must be retained and traceable, got err=%v", err)
		}
		latest, err := rs.LatestVersion(ctx, tnt, ind.Subject, policyKey, 1)
		if err != nil || latest.Version != 2 {
			t.Fatalf("latest version = %d (err %v), want 2", latest.Version, err)
		}
	})

	// --- graph outage errors; graph staleness pauses (never a silent fallback) -
	t.Run("graph_outage_errors_and_staleness_pauses", func(t *testing.T) {
		dept := fx.Hierarchy[2]
		// Outage → ErrGraphUnavailable (partial dependency failure fails closed).
		outage := newEvaluator(t, &fakeGraph{wm: fx.Watermark, unavailable: true}, outcome.NewMemoryStore(fixedNow))
		_, err := outage.Evaluate(ctx, EvaluateRequest{
			Tenant: tnt, Org: org, Subject: dept.Subject, Level: model.LevelDepartment,
			Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
			Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
			DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {}},
			DecidedAt:       fixedNow(),
		})
		if !errors.Is(err, outcomegraph.ErrGraphUnavailable) {
			t.Fatalf("graph outage = %v, want ErrGraphUnavailable", err)
		}
		// Staleness (projection advances mid-read) → not_assessable / stale.
		store := outcome.NewMemoryStore(fixedNow)
		seedOutcomeChain(t, store, tnt, fxOutcome{Key: "outcome-stale", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
		stale := newEvaluator(t, &fakeGraph{wm: fx.Watermark, nodes: candidateNodesForKeys([]string{"outcome-stale"}), advanceEachCall: true}, store)
		wa, err := stale.Evaluate(ctx, EvaluateRequest{
			Tenant: tnt, Org: org, Subject: dept.Subject, Level: model.LevelDepartment,
			Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
			Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
			DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-stale"}}},
			DecidedAt:       fixedNow(),
		})
		if err != nil {
			t.Fatalf("stale evaluate: %v", err)
		}
		if wa.Dimensions[0].State != model.StateNotAssessable || wa.Dimensions[0].NotAssessableReason != model.ReasonStaleGraph {
			t.Fatalf("stale graph must pause (not_assessable/stale), got %q/%q", wa.Dimensions[0].State, wa.Dimensions[0].NotAssessableReason)
		}
	})

	// --- poisoned / unverifiable output is quarantined (never an invented score)
	t.Run("unverifiable_output_quarantined_not_assessable", func(t *testing.T) {
		// A typed graph candidate with NO authoritative Outcome behind it.
		graph := &fakeGraph{wm: fx.Watermark, nodes: candidateNodesForKeys([]string{"outcome-ghost"})}
		e := newEvaluator(t, graph, outcome.NewMemoryStore(fixedNow))
		wa, err := e.Evaluate(ctx, EvaluateRequest{
			Tenant: tnt, Org: org, Subject: "actor:ghost", Level: model.LevelIndividual,
			Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
			Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
			DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-ghost"}}},
			DecidedAt:       fixedNow(),
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if wa.Dimensions[0].State != model.StateNotAssessable {
			t.Fatalf("unverifiable candidate must be not_assessable, got %q", wa.Dimensions[0].State)
		}
		if wa.Dimensions[0].SatisfiedOutcomes != 0 {
			t.Fatalf("an unverifiable candidate must never yield an invented score, got %d", wa.Dimensions[0].SatisfiedOutcomes)
		}
	})

	// --- hard no-silent-publication: formal scoring is gated on a governed
	//     published policy + manager confirmation, and the LLM narration port is
	//     metadata-only (it cannot fabricate or publish any scored fact) ---------
	t.Run("no_silent_publication_of_scores_or_policy", func(t *testing.T) {
		store := outcome.NewMemoryStore(fixedNow)
		seedOutcomeChain(t, store, tnt, fxOutcome{Key: "outcome-np", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
		graph := &fakeGraph{wm: fx.Watermark, nodes: candidateNodesForKeys([]string{"outcome-np"})}
		e := newEvaluator(t, graph, store)
		base := func() EvaluateRequest {
			return EvaluateRequest{
				Tenant: tnt, Org: org, Subject: "actor:np", Level: model.LevelIndividual,
				Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
				Formal: true, Manager: model.ManagerConfirmation{Confirmed: true, Manager: "mgr:x", ConfirmedAt: fixedNow()},
				DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-np"}}},
				DecidedAt:       fixedNow(),
			}
		}
		// Formal scoring is impossible without a PUBLISHED policy.
		shadowReq := base()
		shadowReq.Policy.Status = model.StatusShadow
		if _, err := e.Evaluate(ctx, shadowReq); !errors.Is(err, model.ErrFormalScoringNotAllowed) {
			t.Fatalf("formal scoring under a non-published policy = %v, want ErrFormalScoringNotAllowed", err)
		}
		// Formal scoring is impossible without manager confirmation.
		noMgr := base()
		noMgr.Manager = model.ManagerConfirmation{Confirmed: false}
		if _, err := e.Evaluate(ctx, noMgr); !errors.Is(err, ErrManagerConfirmationRequired) {
			t.Fatalf("formal scoring without manager confirmation = %v, want ErrManagerConfirmationRequired", err)
		}
		// The LLM narration port cannot change or publish any computed/scored fact.
		structured, err := e.Evaluate(ctx, base())
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if structured.Narrative != "" {
			t.Fatal("the deterministic core must not populate a narrative")
		}
		before := structured.Digest()
		narrated, err := e.Narrate(ctx, structured, &mutatingNarrator{})
		if err != nil {
			t.Fatalf("narrate: %v", err)
		}
		if narrated.Digest() != before {
			t.Fatal("narration must not change any computed/scored fact (LLM cannot silently publish)")
		}
	})
}

// candidateNodesForKeys builds satisfied revision-1 graph candidates for keys.
func candidateNodesForKeys(keys []string) []outcomegraph.ResultNode {
	nodes := make([]outcomegraph.ResultNode, 0, len(keys))
	for _, k := range keys {
		nodes = append(nodes, outcomegraph.ResultNode{Label: outcomegraph.LabelOutcome, BusinessID: k, Revision: 1})
	}
	return nodes
}
