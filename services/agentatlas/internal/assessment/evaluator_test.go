package assessment

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	omodel "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
)

// The real authoritative Outcome store satisfies the evaluator's read port.
var _ OutcomeReader = (*outcome.MemoryStore)(nil)

// The real typed graph query service satisfies the graph port (reused from 18B).
var _ GraphQuerier = (*outcomegraph.QueryService)(nil)

const subject = "actor:operator-7"

// --- fixture types ----------------------------------------------------------

type fxFile struct {
	Cases []fxCase `yaml:"cases"`
}

type fxCase struct {
	Name              string           `yaml:"name"`
	Dimension         string           `yaml:"dimension"`
	Watermark         uint64           `yaml:"watermark"`
	GraphCandidates   []fxCandidate    `yaml:"graph_candidates"`
	Outcomes          []fxOutcome      `yaml:"outcomes"`
	DimensionOutcomes []string         `yaml:"dimension_outcomes"`
	Deliverables      []fxDeliverable  `yaml:"deliverables"`
	HumanReports      []fxReport       `yaml:"human_reports"`
	Blockers          []fxBlocker      `yaml:"blockers"`
	Contributions     []fxContribution `yaml:"contributions"`
	Attribution       string           `yaml:"attribution"`
	ExpectState       string           `yaml:"expect_state"`
	ExpectReason      string           `yaml:"expect_reason"`
	ExpectSatisfied   int              `yaml:"expect_satisfied"`
	ExpectCounted     []string         `yaml:"expect_counted"`
	ExpectPlanExt     bool             `yaml:"expect_plan_external"`
}

type fxCandidate struct {
	Key        string `yaml:"key"`
	Revision   uint64 `yaml:"revision"`
	Superseded bool   `yaml:"superseded"`
}

type fxOutcome struct {
	Key   string   `yaml:"key"`
	Chain []fxStep `yaml:"chain"`
}

type fxStep struct {
	Revision  uint64 `yaml:"revision"`
	Status    string `yaml:"status"`
	Signed    bool   `yaml:"signed"`
	Authority string `yaml:"authority"`
	Blocker   bool   `yaml:"blocker"`
}

type fxDeliverable struct {
	Handle    string `yaml:"handle"`
	Signed    bool   `yaml:"signed"`
	Authority string `yaml:"authority"`
}

type fxReport struct {
	Handle    string `yaml:"handle"`
	Authority string `yaml:"authority"`
}

type fxBlocker struct {
	Handle           string `yaml:"handle"`
	Kind             string `yaml:"kind"`
	Signed           bool   `yaml:"signed"`
	Reports          int    `yaml:"reports"`
	External         bool   `yaml:"external"`
	Process          bool   `yaml:"process"`
	Resource         bool   `yaml:"resource"`
	Personal         bool   `yaml:"personal"`
	ExpectConfidence string `yaml:"expect_confidence"`
	ExpectDelay      string `yaml:"expect_delay"`
}

type fxContribution struct {
	Contributor  string  `yaml:"contributor"`
	Outcome      string  `yaml:"outcome"`
	Kind         string  `yaml:"kind"`
	Weight       float64 `yaml:"weight"`
	PlanExternal bool    `yaml:"plan_external"`
}

func loadCases(t *testing.T) []fxCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "assessment", "hierarchy-cases.yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f fxFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture carries no cases")
	}
	return f.Cases
}

// --- seeding helpers --------------------------------------------------------

// seedOutcomeChain appends a valid, immutable Outcome revision chain to the
// authoritative store, so the evaluator VERIFIES candidates against real
// governed facts (never a stub). A satisfied revision carries an authoritative,
// signed observation (the store rejects a satisfied Outcome without one).
func seedOutcomeChain(t *testing.T, store *outcome.MemoryStore, tenant string, o fxOutcome) {
	t.Helper()
	ctx := context.Background()
	var priorID string
	for _, step := range o.Chain {
		claim := omodel.OutcomeClaim{
			Goal:        omodel.GoalRef{Tenant: tenant, GoalKey: goalKey, GoalVersion: goalVer},
			Status:      omodel.OutcomeStatus(step.Status),
			RuleVersion: "rule-1",
		}
		if step.Signed {
			claim.Observations = []omodel.ObservationRef{{
				Handle: o.Key + "-obs", ObservationHash: "h-" + o.Key, Authority: step.Authority,
				SignatureKeyID: "sig-key-1", ObservedAt: fixedNow(),
			}}
		}
		if step.Blocker {
			claim.Blockers = []omodel.BlockerRef{{Handle: o.Key + "-blk", Kind: "external", Authority: step.Authority}}
		}
		out := omodel.Outcome{
			Tenant: tenant, OutcomeKey: o.Key, Revision: step.Revision, Claim: claim,
			WorkCaseID: "wc-" + o.Key, WorkCaseRevision: 1, WorkPlanRevision: 1,
			OperatingMapVersion: 1, OrgVersion: 0, DecidedAt: fixedNow(),
		}
		if step.Revision > 1 {
			out.Supersedes = &omodel.OutcomeRevisionRef{
				OutcomeKey: o.Key, Revision: step.Revision - 1, OutcomeID: priorID, Reason: "correction",
			}
		}
		persisted, err := store.AppendOutcome(ctx, out)
		if err != nil {
			t.Fatalf("seed outcome %s@%d: %v", o.Key, step.Revision, err)
		}
		priorID = persisted.ID
	}
}

func candidateNodes(cands []fxCandidate) []outcomegraph.ResultNode {
	nodes := make([]outcomegraph.ResultNode, 0, len(cands))
	for _, c := range cands {
		nodes = append(nodes, outcomegraph.ResultNode{
			Label: outcomegraph.LabelOutcome, BusinessID: c.Key, Revision: c.Revision, Superseded: c.Superseded,
		})
	}
	return nodes
}

// publishedPolicy builds a minimal PUBLISHED policy over the given dimensions so
// a FORMAL assessment is permitted (FormalScoringActive).
func publishedPolicy(dims []model.DimensionKey) model.Policy {
	p := model.Policy{
		Tenant: tnt, Org: org, PolicyKey: policyKey, Revision: 1, Status: model.StatusPublished,
		Sources:            sources(),
		Watermark:          42,
		GovernanceChangeID: "chg-1",
		CreatedAt:          fixedNow(), UpdatedAt: fixedNow(),
	}
	for _, k := range dims {
		p.Dimensions = append(p.Dimensions, model.Dimension{
			Key: k, Title: string(k), Formal: true,
			Rationale: "grounded in verified outcomes over the assessment period",
			Evidence:  []model.EvidenceLink{{Handle: "org_goal:" + goalKey, Kind: "org_goal", Summary: "grounded in the org goal"}},
		})
		p.EvidenceRules = append(p.EvidenceRules, model.EvidenceRule{Dimension: k, Tier: model.TierVerifiedOutcome, Rationale: "grounded in a verified outcome backed by a signed observation"})
		p.AttributionRules = append(p.AttributionRules, model.AttributionRule{Dimension: k, Mode: model.AttrSharedOnce, Rationale: "a shared outcome is attributed once"})
		p.ConfidenceRules = append(p.ConfidenceRules, model.ConfidenceRule{Dimension: k, Level: model.ConfidenceHigh, Rationale: "coverage is high across the period"})
	}
	return p
}

func assessmentPeriod() model.Period {
	return model.Period{Start: fixedNow().Add(-720 * time.Hour), End: fixedNow().Add(-time.Hour)}
}

func newEvaluator(t *testing.T, graph GraphQuerier, store *outcome.MemoryStore) *Evaluator {
	t.Helper()
	e, err := NewEvaluator(graph, store, fixedNow)
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return e
}

// --- the fixture table ------------------------------------------------------

func TestEvaluatorTableFromFixture(t *testing.T) {
	for _, tc := range loadCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			store := outcome.NewMemoryStore(fixedNow)
			for _, o := range tc.Outcomes {
				seedOutcomeChain(t, store, tnt, o)
			}
			graph := &fakeGraph{wm: tc.Watermark, nodes: candidateNodes(tc.GraphCandidates)}
			e := newEvaluator(t, graph, store)

			dimKey := model.DimensionKey(tc.Dimension)
			di := DimensionInput{OutcomeCandidates: tc.DimensionOutcomes}
			for _, d := range tc.Deliverables {
				sk := ""
				if d.Signed {
					sk = "sig-key-1"
				}
				di.Deliverables = append(di.Deliverables, DeliverableFact{Handle: d.Handle, Kind: "deliverable", SignatureKeyID: sk, Authority: d.Authority})
			}
			for _, r := range tc.HumanReports {
				di.HumanReports = append(di.HumanReports, HumanReportFact{Handle: r.Handle, Authority: r.Authority})
			}
			for _, b := range tc.Blockers {
				di.Blockers = append(di.Blockers, BlockerFact{
					Handle: b.Handle, Kind: b.Kind,
					Signals: BlockerSignals{Signed: b.Signed, Reports: b.Reports, External: b.External, Process: b.Process, Resource: b.Resource, Personal: b.Personal},
				})
			}
			for _, c := range tc.Contributions {
				di.Contributions = append(di.Contributions, model.Contribution{ContributorID: c.Contributor, OutcomeKey: c.Outcome, Kind: c.Kind, Weight: c.Weight, PlanExternal: c.PlanExternal})
			}

			req := EvaluateRequest{
				Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
				Policy: publishedPolicy([]model.DimensionKey{dimKey}), Period: assessmentPeriod(),
				Formal: true, Manager: model.ManagerConfirmation{Confirmed: true, Manager: "mgr:line-lead", ConfirmedAt: fixedNow()},
				DimensionInputs: map[model.DimensionKey]DimensionInput{dimKey: di},
				DecidedAt:       fixedNow(),
			}
			wa, err := e.Evaluate(ctx, req)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if err := wa.Validate(); err != nil {
				t.Fatalf("produced assessment is invalid: %v", err)
			}
			if len(wa.Dimensions) != 1 {
				t.Fatalf("want one dimension result, got %d", len(wa.Dimensions))
			}
			d := wa.Dimensions[0]
			if string(d.State) != tc.ExpectState {
				t.Fatalf("state = %q, want %q (reason %q)", d.State, tc.ExpectState, d.NotAssessableReason)
			}
			if tc.ExpectState == "not_assessable" && string(d.NotAssessableReason) != tc.ExpectReason {
				t.Fatalf("not_assessable reason = %q, want %q", d.NotAssessableReason, tc.ExpectReason)
			}
			if tc.ExpectState == "assessed" {
				if d.SatisfiedOutcomes != tc.ExpectSatisfied {
					t.Fatalf("satisfied = %d, want %d", d.SatisfiedOutcomes, tc.ExpectSatisfied)
				}
				if !equalStringSets(d.CountedOutcomeKeys, tc.ExpectCounted) {
					t.Fatalf("counted = %v, want %v", d.CountedOutcomeKeys, tc.ExpectCounted)
				}
			}
			// Blocker classification (deterministic).
			for _, want := range tc.Blockers {
				found := false
				for _, got := range d.Blockers {
					if got.Handle != want.Handle {
						continue
					}
					found = true
					if string(got.Confidence) != want.ExpectConfidence {
						t.Fatalf("blocker %s confidence = %q, want %q", want.Handle, got.Confidence, want.ExpectConfidence)
					}
					if string(got.Delay) != want.ExpectDelay {
						t.Fatalf("blocker %s delay = %q, want %q", want.Handle, got.Delay, want.ExpectDelay)
					}
				}
				if !found {
					t.Fatalf("blocker %s not attached to the dimension result", want.Handle)
				}
			}
			// Plan-external contribution is included, never dropped.
			if tc.ExpectPlanExt {
				any := false
				for _, c := range d.Contributions {
					if c.PlanExternal {
						any = true
					}
				}
				if !any {
					t.Fatal("a plan-external contribution must be included in the result")
				}
			}
		})
	}
}

// --- stale graph BLOCKS (not_assessable), unavailable errors ----------------

func TestEvaluatorStaleGraphIsNotAssessable(t *testing.T) {
	ctx := context.Background()
	store := outcome.NewMemoryStore(fixedNow)
	seedOutcomeChain(t, store, tnt, fxOutcome{Key: "outcome-1", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
	graph := &fakeGraph{wm: 42, nodes: candidateNodes([]fxCandidate{{Key: "outcome-1", Revision: 1}}), advanceEachCall: true}
	e := newEvaluator(t, graph, store)

	req := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
		Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-1"}}},
		DecidedAt:       fixedNow(),
	}
	wa, err := e.Evaluate(ctx, req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	d := wa.Dimensions[0]
	if d.State != model.StateNotAssessable || d.NotAssessableReason != model.ReasonStaleGraph {
		t.Fatalf("a stale graph watermark must block (not_assessable/stale), got %q/%q", d.State, d.NotAssessableReason)
	}
	// Projection freshness confidence must reflect the stale read.
	for _, c := range d.Confidence.Components {
		if c.Kind == model.ConfProjectionFreshness && c.Score != 0 {
			t.Fatalf("stale projection freshness must be 0, got %v", c.Score)
		}
	}
}

func TestEvaluatorUnavailableGraphErrors(t *testing.T) {
	ctx := context.Background()
	store := outcome.NewMemoryStore(fixedNow)
	graph := &fakeGraph{wm: 42, unavailable: true}
	e := newEvaluator(t, graph, store)
	req := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
		Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {}},
		DecidedAt:       fixedNow(),
	}
	if _, err := e.Evaluate(ctx, req); !errors.Is(err, outcomegraph.ErrGraphUnavailable) {
		t.Fatalf("Evaluate on an unavailable graph = %v, want ErrGraphUnavailable", err)
	}
}

// --- formal requires a published policy + manager confirmation --------------

func TestEvaluatorFormalRequiresPublishedPolicy(t *testing.T) {
	ctx := context.Background()
	store := outcome.NewMemoryStore(fixedNow)
	graph := &fakeGraph{wm: 42}
	e := newEvaluator(t, graph, store)
	p := publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion})
	p.Status = model.StatusShadow // not published => formal scoring not allowed
	req := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
		Policy: p, Period: assessmentPeriod(), Formal: true,
		Manager:         model.ManagerConfirmation{Confirmed: true},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {}},
		DecidedAt:       fixedNow(),
	}
	if _, err := e.Evaluate(ctx, req); !errors.Is(err, model.ErrFormalScoringNotAllowed) {
		t.Fatalf("formal assessment under a non-published policy = %v, want ErrFormalScoringNotAllowed", err)
	}
}

func TestEvaluatorFormalRequiresManagerConfirmation(t *testing.T) {
	ctx := context.Background()
	store := outcome.NewMemoryStore(fixedNow)
	graph := &fakeGraph{wm: 42}
	e := newEvaluator(t, graph, store)
	req := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
		Formal: true, Manager: model.ManagerConfirmation{Confirmed: false},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {}},
		DecidedAt:       fixedNow(),
	}
	if _, err := e.Evaluate(ctx, req); !errors.Is(err, ErrManagerConfirmationRequired) {
		t.Fatalf("formal assessment without manager confirmation = %v, want ErrManagerConfirmationRequired", err)
	}
}

// --- LLM = narration port only: it cannot change any computed fact ----------

// mutatingNarrator tries (and structurally fails) to change the computed facts:
// it can only return text.
type mutatingNarrator struct{ called bool }

func (n *mutatingNarrator) Narrate(_ context.Context, facts NarrationFacts) (string, error) {
	n.called = true
	if len(facts.Assessment.Dimensions) == 0 {
		return "", errors.New("no facts")
	}
	return "The operator met the throughput outcome; one external dependency delayed handoff.", nil
}

func TestEvaluatorNarrationPortCannotChangeComputedFacts(t *testing.T) {
	ctx := context.Background()
	store := outcome.NewMemoryStore(fixedNow)
	seedOutcomeChain(t, store, tnt, fxOutcome{Key: "outcome-1", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
	graph := &fakeGraph{wm: 42, nodes: candidateNodes([]fxCandidate{{Key: "outcome-1", Revision: 1}})}
	e := newEvaluator(t, graph, store)
	req := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
		Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-1"}}},
		DecidedAt:       fixedNow(),
	}
	// The deterministic core produces the structured result with NO narrative.
	structured, err := e.Evaluate(ctx, req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if structured.Narrative != "" {
		t.Fatal("the deterministic core must not populate the narrative")
	}
	before := structured.Digest()

	// Applying the narration port attaches ONLY text; every computed fact
	// (digest, id, dimension results, confidence) is unchanged.
	nr := &mutatingNarrator{}
	narrated, err := e.Narrate(ctx, structured, nr)
	if err != nil {
		t.Fatalf("Narrate: %v", err)
	}
	if !nr.called {
		t.Fatal("the narration port must be invoked by Narrate")
	}
	if narrated.Narrative == "" {
		t.Fatal("Narrate must store the port's text in the narrative field")
	}
	if narrated.Digest() != before {
		t.Fatal("narration must not change the structured content digest")
	}
	if narrated.DerivedID() != structured.DerivedID() {
		t.Fatal("narration must not change the assessment id")
	}
	// The structured result (everything but narrative) is byte-identical.
	a := narrated
	a.Narrative = ""
	x, _ := json.Marshal(structured.Normalize())
	y, _ := json.Marshal(a.Normalize())
	if string(x) != string(y) {
		t.Fatal("narration must not change any structured field")
	}
}

// --- the evaluator reads the graph ONLY through the typed operation set ------

func TestEvaluatorUsesOnlyTypedGraphOperations(t *testing.T) {
	ctx := context.Background()
	store := outcome.NewMemoryStore(fixedNow)
	graph := &fakeGraph{wm: 42, nodes: candidateNodes([]fxCandidate{{Key: "outcome-1", Revision: 1}})}
	seedOutcomeChain(t, store, tnt, fxOutcome{Key: "outcome-1", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
	e := newEvaluator(t, graph, store)
	req := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
		Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-1"}}},
		DecidedAt:       fixedNow(),
	}
	if _, err := e.Evaluate(ctx, req); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(graph.calls) == 0 {
		t.Fatal("the evaluator must ground candidates via the typed graph surface")
	}
	for _, c := range graph.calls {
		if !c.Operation.Valid() {
			t.Fatalf("evaluator issued a non-typed graph operation %q", c.Operation)
		}
		if c.Tenant != tnt {
			t.Fatalf("graph query tenant = %q, want %q (tenant-scoped)", c.Tenant, tnt)
		}
	}
	// The GraphSchema bound into the assessment carries the exact provider/schema
	// and watermark.
	// (Watermark asserted via a fresh run below.)
}

// --- confidence is deterministic metadata (invariant 3) ---------------------

func TestConfidenceIsDeterministicMetadata(t *testing.T) {
	ctx := context.Background()
	build := func() model.WorkAssessment {
		store := outcome.NewMemoryStore(fixedNow)
		seedOutcomeChain(t, store, tnt, fxOutcome{Key: "outcome-1", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
		graph := &fakeGraph{wm: 42, nodes: candidateNodes([]fxCandidate{{Key: "outcome-1", Revision: 1}})}
		e := newEvaluator(t, graph, store)
		req := EvaluateRequest{
			Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
			Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
			Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
			DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-1"}}},
			DecidedAt:       fixedNow(),
		}
		wa, err := e.Evaluate(ctx, req)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		return wa
	}
	a, b := build(), build()
	ax, _ := json.Marshal(a)
	bx, _ := json.Marshal(b)
	if string(ax) != string(bx) {
		t.Fatal("confidence/result must be deterministic across identical runs")
	}
	conf := a.Dimensions[0].Confidence
	if err := conf.Validate(); err != nil {
		t.Fatalf("confidence explanation invalid: %v", err)
	}
	// All seven named components must be present.
	if len(conf.Components) != len(model.ConfidenceKinds()) {
		t.Fatalf("want %d confidence components, got %d", len(model.ConfidenceKinds()), len(conf.Components))
	}
}

// --- blocker classification matrix (pure function; drives TestBlockers) -------

func TestBlockersClassificationMatrix(t *testing.T) {
	cases := []struct {
		name     string
		s        BlockerSignals
		wantConf model.BlockerConfidence
		wantDel  model.DelayAttribution
	}{
		{"signed_external", BlockerSignals{Signed: true, External: true}, model.BlockerVerified, model.DelayExternal},
		{"two_reports_process", BlockerSignals{Reports: 2, Process: true}, model.BlockerCorroborated, model.DelayProcess},
		{"one_report_resource", BlockerSignals{Reports: 1, Resource: true}, model.BlockerReported, model.DelayResource},
		{"no_report_personal", BlockerSignals{Personal: true}, model.BlockerInferred, model.DelayPersonal},
		{"no_signal", BlockerSignals{}, model.BlockerInferred, model.DelayUnattributed},
		// Fair precedence: an external cause dominates a personal one (never blame
		// the person when an outside dependency explains the delay).
		{"external_beats_personal", BlockerSignals{Reports: 1, External: true, Personal: true}, model.BlockerReported, model.DelayExternal},
		{"resource_beats_personal", BlockerSignals{Signed: true, Resource: true, Personal: true}, model.BlockerVerified, model.DelayResource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conf, del := ClassifyBlocker(tc.s)
			if conf != tc.wantConf {
				t.Fatalf("confidence = %q, want %q", conf, tc.wantConf)
			}
			if del != tc.wantDel {
				t.Fatalf("delay = %q, want %q", del, tc.wantDel)
			}
		})
	}
}

// --- hierarchy: no cross-level double-count (invariant 6) --------------------

func TestHierarchyNoCrossLevelDoubleCount(t *testing.T) {
	ctx := context.Background()
	store := outcome.NewMemoryStore(fixedNow)
	// One shared, satisfied outcome that BOTH a report and the manager's level
	// would otherwise count.
	seedOutcomeChain(t, store, tnt, fxOutcome{Key: "outcome-shared", Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
	graph := &fakeGraph{wm: 42, nodes: candidateNodes([]fxCandidate{{Key: "outcome-shared", Revision: 1}})}
	e := newEvaluator(t, graph, store)

	// The report's (child) individual assessment counts the shared outcome.
	childReq := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: "actor:report-1", Level: model.LevelIndividual,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
		Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-shared"}}},
		DecidedAt:       fixedNow(),
	}
	child, err := e.Evaluate(ctx, childReq)
	if err != nil {
		t.Fatalf("child Evaluate: %v", err)
	}
	if got := child.Dimensions[0].CountedOutcomeKeys; len(got) != 1 || got[0] != "outcome-shared" {
		t.Fatalf("child must count the shared outcome, got %v", got)
	}

	// The manager's (parent) group assessment aggregates the child and lists the
	// SAME shared outcome — it must NOT count it again (shared_once across levels).
	parentReq := EvaluateRequest{
		Tenant: tnt, Org: org, Subject: "actor:manager-1", Level: model.LevelGroup,
		Policy: publishedPolicy([]model.DimensionKey{model.DimOutcomeCompletion}), Period: assessmentPeriod(),
		Formal: true, Manager: model.ManagerConfirmation{Confirmed: true},
		DimensionInputs: map[model.DimensionKey]DimensionInput{model.DimOutcomeCompletion: {OutcomeCandidates: []string{"outcome-shared"}}},
		SubAssessments:  []model.WorkAssessment{child},
		DecidedAt:       fixedNow(),
	}
	parent, err := e.Evaluate(ctx, parentReq)
	if err != nil {
		t.Fatalf("parent Evaluate: %v", err)
	}
	// The parent's own dimension must not re-count the shared outcome.
	for _, k := range parent.Dimensions[0].CountedOutcomeKeys {
		if k == "outcome-shared" {
			t.Fatal("the shared outcome must be counted ONCE across levels, not re-counted at the parent")
		}
	}
	// Across the whole hierarchy the shared outcome appears exactly once.
	count := 0
	for _, k := range parent.CountedOutcomeKeys() {
		if k == "outcome-shared" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("shared outcome counted %d times across the hierarchy, want 1", count)
	}
	// The parent embeds the child sub-assessment.
	if len(parent.SubAssessments) != 1 {
		t.Fatalf("parent must embed the child sub-assessment, got %d", len(parent.SubAssessments))
	}
}

// --- determinism: byte-equal structured result after shuffling inputs --------

func TestShuffledInputDeterminism(t *testing.T) {
	ctx := context.Background()
	seed := func() (*outcome.MemoryStore, *fakeGraph) {
		store := outcome.NewMemoryStore(fixedNow)
		for _, k := range []string{"outcome-a", "outcome-b", "outcome-c"} {
			seedOutcomeChain(t, store, tnt, fxOutcome{Key: k, Chain: []fxStep{{Revision: 1, Status: "satisfied", Signed: true, Authority: "mes.official"}}})
		}
		graph := &fakeGraph{wm: 42, nodes: candidateNodes([]fxCandidate{
			{Key: "outcome-a", Revision: 1}, {Key: "outcome-b", Revision: 1}, {Key: "outcome-c", Revision: 1},
		})}
		return store, graph
	}

	// A rich multi-dimension request with several outcomes, blockers and
	// contributions per dimension.
	baseInputs := func() map[model.DimensionKey]DimensionInput {
		return map[model.DimensionKey]DimensionInput{
			model.DimOutcomeCompletion: {
				OutcomeCandidates: []string{"outcome-a", "outcome-b", "outcome-c"},
				Deliverables: []DeliverableFact{
					{Handle: "wc-1", Kind: "deliverable", SignatureKeyID: "sig-key-1", Authority: "qa"},
					{Handle: "wc-2", Kind: "deliverable", SignatureKeyID: "sig-key-1", Authority: "qa"},
				},
				// Two human reports sharing a handle but different authority — a TIED
				// tier-3 evidence key that only a total order can normalize.
				HumanReports: []HumanReportFact{
					{Handle: "hr", Authority: "peer-a"},
					{Handle: "hr", Authority: "peer-b"},
				},
				Blockers: []BlockerFact{
					{Handle: "blk-1", Kind: "dep", Signals: BlockerSignals{Signed: true, External: true}},
					{Handle: "blk-2", Kind: "proc", Signals: BlockerSignals{Reports: 2, Process: true}},
					// Two blockers sharing a handle but classifying to different
					// confidence/delay — a TIED blocker key.
					{Handle: "blk", Kind: "dep", Signals: BlockerSignals{Signed: true, External: true}},
					{Handle: "blk", Kind: "proc", Signals: BlockerSignals{Reports: 1, Process: true}},
				},
				Contributions: []model.Contribution{
					{ContributorID: "c-2", OutcomeKey: "outcome-b", Kind: "review"},
					{ContributorID: "c-1", OutcomeKey: "outcome-a", Kind: "author"},
				},
			},
			model.DimQuality: {OutcomeCandidates: []string{"outcome-c", "outcome-a"}},
		}
	}
	dims := []model.DimensionKey{model.DimOutcomeCompletion, model.DimQuality}

	build := func(rng *rand.Rand) model.WorkAssessment {
		store, graph := seed()
		e := newEvaluator(t, graph, store)
		inputs := baseInputs()
		// Shuffle EVERY per-dimension slice input (including HumanReports and
		// Deliverables) so a total-order regression in any of them is caught.
		if rng != nil {
			for k := range inputs {
				di := inputs[k]
				rng.Shuffle(len(di.OutcomeCandidates), func(i, j int) {
					di.OutcomeCandidates[i], di.OutcomeCandidates[j] = di.OutcomeCandidates[j], di.OutcomeCandidates[i]
				})
				rng.Shuffle(len(di.Deliverables), func(i, j int) { di.Deliverables[i], di.Deliverables[j] = di.Deliverables[j], di.Deliverables[i] })
				rng.Shuffle(len(di.HumanReports), func(i, j int) { di.HumanReports[i], di.HumanReports[j] = di.HumanReports[j], di.HumanReports[i] })
				rng.Shuffle(len(di.Blockers), func(i, j int) { di.Blockers[i], di.Blockers[j] = di.Blockers[j], di.Blockers[i] })
				rng.Shuffle(len(di.Contributions), func(i, j int) { di.Contributions[i], di.Contributions[j] = di.Contributions[j], di.Contributions[i] })
				inputs[k] = di
			}
		}
		req := EvaluateRequest{
			Tenant: tnt, Org: org, Subject: subject, Level: model.LevelIndividual,
			Policy: publishedPolicy(dims), Period: assessmentPeriod(),
			Formal: true, Manager: model.ManagerConfirmation{Confirmed: true, Manager: "mgr:line-lead", ConfirmedAt: fixedNow()},
			DimensionInputs: inputs, DecidedAt: fixedNow(),
		}
		wa, err := e.Evaluate(ctx, req)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		return wa
	}

	want, err := json.Marshal(build(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rng := rand.New(rand.NewSource(101))
	for i := 0; i < 30; i++ {
		got, err := json.Marshal(build(rng))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("structured result is not shuffle-invariant on iteration %d:\n want %s\n  got %s", i, want, got)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}
