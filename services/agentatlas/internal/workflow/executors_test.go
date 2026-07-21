package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
)

type fakePipeline struct{ docID string }

func (f fakePipeline) ParseArtifact(_ context.Context, _, artifactID, _ string) (string, error) {
	if artifactID == "art_bad" {
		return "", fmt.Errorf("parse exploded")
	}
	return f.docID, nil
}

type fakeDocs struct{}

func (fakeDocs) LoadSOPDocument(context.Context, string) (agent.SOPDocument, error) {
	return agent.SOPDocument{Title: "MES SOP", Sections: []string{"第一步"}}, nil
}

func (fakeDocs) LoadParseOutput(context.Context, string) (parsergateway.ParseOutput, error) {
	return parsergateway.ParseOutput{ProviderID: "docling"}, nil
}

type fakeRetriever struct{}

func (fakeRetriever) CreatePlan(context.Context, retrieval.Query) (string, error) {
	return "rp_1", nil
}

func (fakeRetriever) Execute(context.Context, string, retrieval.Query) ([]retrieval.Result, error) {
	return []retrieval.Result{{DocID: "d1", Snippet: "MES 摘要", Authoritative: true, EvidencePointerID: "ev_1", Score: 1.5}}, nil
}

// mixedAuthorityRetriever returns one governed (authoritative) result and one
// ungrounded legacy dream_summary (non-authoritative) result.
type mixedAuthorityRetriever struct{}

func (mixedAuthorityRetriever) CreatePlan(context.Context, retrieval.Query) (string, error) {
	return "rp_mix", nil
}
func (mixedAuthorityRetriever) Execute(context.Context, string, retrieval.Query) ([]retrieval.Result, error) {
	return []retrieval.Result{
		{DocID: "m1", SourceType: "method_outline", Authoritative: true, Snippet: "治理方法：两步校验", EvidencePointerID: "ev_m", Score: 2},
		{DocID: "d1", SourceType: "dream_summary", Authoritative: false, Snippet: "梦境叙述摘要：本周整体顺利", EvidencePointerID: "ev_dream", Score: 1},
	}, nil
}

func execNode(t *testing.T, r Registry, node sdkworkflow.Node, run *RunContext) map[string]any {
	t.Helper()
	out, err := r[node.Type].Execute(context.Background(), node, run)
	if err != nil {
		t.Fatalf("execute %s: %v", node.Type, err)
	}
	return out
}

func TestExecutorsCoverAllSixteenNodeTypes(t *testing.T) {
	r := NewRegistryWithServices(Executors{
		Artifacts: fakePipeline{docID: "doc_1"},
		Documents: fakeDocs{},
		SOP: func(_ context.Context, doc agent.SOPDocument) (sdkworkflow.Workflow, error) {
			return sdkworkflow.Workflow{WorkflowID: "wf_gen", Kind: sdkworkflow.KindSOP,
				Nodes: []sdkworkflow.Node{{ID: "a", Type: sdkworkflow.NodeInputManual}},
				Edges: []sdkworkflow.Edge{}, RiskLevel: sdkworkflow.RiskLow}, nil
		},
		Retrieval: fakeRetriever{},
		Nexus:     nexusclient.NewMock(),
		Dream: func(_ context.Context, input DreamAggregateInput) (map[string]any, error) {
			return map[string]any{"display": input.OrgUnitID + fmt.Sprint(len(input.Inputs)), "source": "deterministic"}, nil
		},
		Answer: func(_ context.Context, q string, snips []string) (string, string, error) {
			return "答案:" + q, "test/model", nil
		},
	})
	for _, typ := range sdkworkflow.BuiltinNodeTypes {
		if _, ok := r[typ]; !ok {
			t.Fatalf("node type %s missing from service registry", typ)
		}
	}
	// no notWired executor remains: probing an unconfigured dep must complain
	// about configuration, never about "not wired".
	for typ, exec := range r {
		if typ == sdkworkflow.NodeHumanConfirm {
			continue // runtime pause point
		}
		_, err := exec.Execute(context.Background(), sdkworkflow.Node{ID: "probe", Type: typ}, &RunContext{
			Input: map[string]any{}, Outputs: map[string]map[string]any{},
		})
		if err != nil && strings.Contains(err.Error(), "not wired") {
			t.Fatalf("node type %s still notWired", typ)
		}
	}
}

func TestDreamExecutorConsumesOnlyTypedBoundedSanitizedInput(t *testing.T) {
	var captured DreamAggregateInput
	r := NewRegistryWithServices(Executors{Dream: func(_ context.Context, input DreamAggregateInput) (map[string]any, error) {
		captured = input
		return map[string]any{
			"display": "display", "retrieval": "retrieval", "sealed_detail": "sealed",
			"facts": []any{}, "themes": []any{}, "trends": []any{}, "risks": []any{}, "todos": []any{},
			"source": "deterministic", "model_route": "workflow/dream", "model_version": "v3",
		}, nil
	}})
	run := &RunContext{EnterpriseID: "ent-1", Dream: &VerifiedDreamContext{DreamRunID: "dr-1", PolicyID: "policy-1", PolicyVersion: 1, WorkflowID: "wf-dream", WorkflowVersion: 3, OrgUnitID: "department:rd"}, Input: map[string]any{
		"org_unit_id": "department:rd", "window_start": "2026-07-11T00:00:00Z", "window_end": "2026-07-12T00:00:00Z",
		"inputs":            []any{map[string]any{"source_type": "work_brief", "source_id": "brief-1", "org_unit_id": "department:rd", "sanitized_text": "safe", "visibility": []any{"managers"}}},
		"coverage":          map[string]any{"expected_children": float64(0), "completed_children": float64(0), "input_count": float64(1)},
		"missing":           []any{},
		"risk_signal_rules": []any{},
	}, Outputs: map[string]map[string]any{}}
	out := execNode(t, r, sdkworkflow.Node{ID: "dream", Type: sdkworkflow.NodeDreamAggregate}, run)
	if captured.OrgUnitID != "department:rd" || len(captured.Inputs) != 1 || captured.Inputs[0].SanitizedText != "safe" {
		t.Fatalf("captured = %+v", captured)
	}
	if out["sealed_detail"] != "sealed" {
		t.Fatalf("out = %v", out)
	}

	run.Input["inputs"] = make([]any, 1001)
	if _, err := r[sdkworkflow.NodeDreamAggregate].Execute(context.Background(), sdkworkflow.Node{ID: "dream", Type: sdkworkflow.NodeDreamAggregate}, run); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("unbounded typed input must fail before aggregation: %v", err)
	}
}

func TestDreamExecutorRejectsCallerControlledGenericWorkflowInput(t *testing.T) {
	called := false
	r := NewRegistryWithServices(Executors{Dream: func(context.Context, DreamAggregateInput) (map[string]any, error) {
		called = true
		return validExecutorDreamOutput(), nil
	}})
	run := &RunContext{EnterpriseID: "ent-1", Input: map[string]any{"dream_run_id": "dr-forged", "org_unit_id": "department:rd"}, Outputs: map[string]map[string]any{}}
	_, err := r[sdkworkflow.NodeDreamAggregate].Execute(context.Background(), sdkworkflow.Node{ID: "dream", Type: sdkworkflow.NodeDreamAggregate}, run)
	if err == nil || !strings.Contains(err.Error(), "verified Dream") || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func validExecutorDreamOutput() map[string]any {
	return map[string]any{"display": "d", "retrieval": "r", "sealed_detail": "s", "facts": []any{}, "themes": []any{}, "trends": []any{}, "risks": []any{}, "todos": []any{}, "source": "deterministic", "model_route": "workflow/wf-dream", "model_version": "v3"}
}

func TestDreamOutputRejectsUnknownTopLevelAndSignalFields(t *testing.T) {
	top := validExecutorDreamOutput()
	top["unexpected"] = "x"
	if validateDreamAggregateOutput(top) == nil {
		t.Fatal("unknown top-level field accepted")
	}
	signal := validExecutorDreamOutput()
	signal["facts"] = []any{map[string]any{"id": "f", "title": "t", "detail": "d", "severity": "info", "evidence_pointer_id": "ev", "unexpected": "x"}}
	if validateDreamAggregateOutput(signal) == nil {
		t.Fatal("unknown signal field accepted")
	}
}

func TestTraceRecordPreservesDreamRunWorkflowPolicyAndEvidenceLineage(t *testing.T) {
	run := &RunContext{RunID: "wrun-1", EnterpriseID: "ent-1", Dream: &VerifiedDreamContext{DreamRunID: "dr-1", PolicyID: "policy-1", PolicyVersion: 7, WorkflowID: "wf-dream", WorkflowVersion: 3, EvidencePointerIDs: []string{"ev-1", "ev-2"}, ParentDreamRunIDs: []string{"dr-child"}}, Input: map[string]any{
		"dream_run_id": "dr-1", "dream_policy_id": "policy-1", "dream_policy_version": float64(7),
		"workflow_id": "wf-dream", "workflow_version": float64(3),
		"evidence_pointer_ids": []any{"ev-1", "ev-2"}, "parent_dream_run_ids": []any{"dr-child"},
	}, Outputs: map[string]map[string]any{}}
	rec, err := traceRecord(sdkworkflow.Node{ID: "trace", Type: sdkworkflow.NodeTraceAppend}, run)
	if err != nil {
		t.Fatal(err)
	}
	if rec.CaseTicketID != "dream-run/dr-1" || rec.WorkflowRunID != "wrun-1" || len(rec.EvidencePointerIDs) != 2 || len(rec.Steps) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	detail := rec.Steps[0].Detail
	if detail["dream_run_id"] != "dr-1" || detail["dream_policy_id"] != "policy-1" || detail["workflow_id"] != "wf-dream" {
		t.Fatalf("lineage detail = %v", detail)
	}
}

func TestParserExecutorFlowsArtifactToDocument(t *testing.T) {
	r := NewRegistryWithServices(Executors{Artifacts: fakePipeline{docID: "doc_9"}})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"artifact_id": "art_1"}, Outputs: map[string]map[string]any{}}
	out := execNode(t, r, sdkworkflow.Node{ID: "p", Type: sdkworkflow.NodeParserDocument}, run)
	if out["atlas_document_id"] != "doc_9" {
		t.Fatalf("out = %v", out)
	}
	// failures propagate loud
	badRun := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"artifact_id": "art_bad"}, Outputs: map[string]map[string]any{}}
	if _, err := r[sdkworkflow.NodeParserDocument].Execute(context.Background(), sdkworkflow.Node{ID: "p", Type: sdkworkflow.NodeParserDocument}, badRun); err == nil {
		t.Fatal("parser failure must fail loud")
	}
}

func TestRetrievalAndAnswerExecutorsChainOutputs(t *testing.T) {
	r := NewRegistryWithServices(Executors{
		Retrieval: fakeRetriever{},
		Answer: func(_ context.Context, q string, snips []string) (string, string, error) {
			return fmt.Sprintf("基于 %d 条材料回答:%s", len(snips), q), "test/model", nil
		},
	})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"query": "我的工作", "question": "我的工作"}, Outputs: map[string]map[string]any{}}
	search := execNode(t, r, sdkworkflow.Node{ID: "s", Type: sdkworkflow.NodeRetrievalSearch}, run)
	if search["plan_id"] != "rp_1" {
		t.Fatalf("search out = %v", search)
	}
	run.Outputs["s"] = search
	answer := execNode(t, r, sdkworkflow.Node{ID: "a", Type: sdkworkflow.NodeAnswerGenerate}, run)
	if !strings.Contains(answer["answer"].(string), "基于 1 条材料") {
		t.Fatalf("answer must consume upstream snippets: %v", answer)
	}
}

// TestRetrievalSearchExcludesNonAuthoritativeFromGeneratedAnswer proves the
// Task 18A Part A guard on the WORKFLOW answer path: a published
// retrieval.search → answer.generate workflow grounds the generated answer ONLY
// on governed (authoritative) documents; the legacy ungrounded dream_summary is
// excluded from the snippets/evidence pointers that reach answer.generate, while
// remaining searchable (demoted, labelled) in the raw hits.
func TestRetrievalSearchExcludesNonAuthoritativeFromGeneratedAnswer(t *testing.T) {
	var captured []string
	r := NewRegistryWithServices(Executors{
		Retrieval: mixedAuthorityRetriever{},
		Answer: func(_ context.Context, q string, snips []string) (string, string, error) {
			captured = append([]string(nil), snips...)
			return fmt.Sprintf("基于 %d 条材料回答:%s", len(snips), q), "test/model", nil
		},
	})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"query": "本周工作", "question": "本周工作"}, Outputs: map[string]map[string]any{}}
	search := execNode(t, r, sdkworkflow.Node{ID: "s", Type: sdkworkflow.NodeRetrievalSearch}, run)

	// The governed grounding (snippets) carries ONLY the authoritative document.
	snips, _ := search["snippets"].([]string)
	if len(snips) != 1 || snips[0] != "治理方法：两步校验" {
		t.Fatalf("snippets must exclude the non-authoritative dream_summary, got %v", snips)
	}
	// The evidence pointers used to ground the answer likewise exclude it.
	ptrs, _ := search["evidence_pointer_ids"].([]string)
	for _, p := range ptrs {
		if p == "ev_dream" {
			t.Fatalf("the dream_summary evidence pointer must not reach the governed grounding: %v", ptrs)
		}
	}
	// The dream_summary REMAINS searchable in hits (demoted, labelled), not deleted.
	hits, _ := search["hits"].([]any)
	foundDemoted := false
	for _, h := range hits {
		m := h.(map[string]any)
		if m["doc_id"] == "d1" {
			foundDemoted = true
			if m["authoritative"] != false {
				t.Fatalf("the dream_summary hit must be labelled non-authoritative")
			}
		}
	}
	if !foundDemoted || len(hits) != 2 {
		t.Fatalf("all results must remain searchable in hits (demoted, not deleted), got %d", len(hits))
	}

	// Chaining into answer.generate, the generated answer is grounded ONLY on the
	// single authoritative snippet — the dream_summary never reaches it.
	run.Outputs["s"] = search
	answer := execNode(t, r, sdkworkflow.Node{ID: "a", Type: sdkworkflow.NodeAnswerGenerate}, run)
	if !strings.Contains(answer["answer"].(string), "基于 1 条材料") {
		t.Fatalf("answer must be grounded on exactly the 1 authoritative snippet: %v", answer)
	}
	for _, s := range captured {
		if strings.Contains(s, "梦境叙述摘要") {
			t.Fatalf("a dream_summary snippet reached the answer generator: %v", captured)
		}
	}
}

// TestNexusEvidenceNodesCarryNoCallerIdentity replaces the old
// fail-closed-without-ticket test. The nodes used to require a ticket_id in
// workflow config; identity is now derived from the verified service
// credential at ingress, so a caller-supplied identity field in node config
// would be the retired model kept alive in configuration.
func TestNexusEvidenceNodesCarryNoCallerIdentity(t *testing.T) {
	evidence := &fakeWorkflowEvidence{}
	r := NewRegistryWithServices(Executors{Evidence: evidence})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"evidence_pointer_id": "ev_1"}, Outputs: map[string]map[string]any{}}

	// No ticket_id anywhere in config, and locate still succeeds.
	out := execNode(t, r, sdkworkflow.Node{ID: "l", Type: sdkworkflow.NodeNexusLocate}, run)
	if out["evidence_ref"] != "evd_0123456789abcdef0123" {
		t.Fatalf("locate out = %v", out)
	}
	// The node emits an opaque handle, never a connector location.
	if _, leaked := out["resource_uri"]; leaked {
		t.Fatalf("locate emitted a connector location: %v", out)
	}
	if len(evidence.locates) != 1 || evidence.locates[0].DataNeeds[0].NeedID != "ev_1" {
		t.Fatalf("locate request = %+v", evidence.locates)
	}
}

// TestNexusEvidenceNodesFailClosedWithoutEvidenceClient keeps the fail-closed
// property the old test protected, moved to the dependency that now matters.
func TestNexusEvidenceNodesFailClosedWithoutEvidenceClient(t *testing.T) {
	r := NewRegistryWithServices(Executors{})
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"evidence_pointer_id": "ev_1"}, Outputs: map[string]map[string]any{}}
	if _, err := r[sdkworkflow.NodeNexusLocate].Execute(context.Background(), sdkworkflow.Node{ID: "l", Type: sdkworkflow.NodeNexusLocate}, run); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nexus.locate without an evidence client must fail closed, got %v", err)
	}
}

func TestUnconfiguredExecutorDependencyFailsLoud(t *testing.T) {
	r := NewRegistryWithServices(Executors{}) // nothing configured
	run := &RunContext{EnterpriseID: "ent_1", Input: map[string]any{"artifact_id": "a"}, Outputs: map[string]map[string]any{}}
	_, err := r[sdkworkflow.NodeParserDocument].Execute(context.Background(), sdkworkflow.Node{ID: "p", Type: sdkworkflow.NodeParserDocument}, run)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured dep must fail loud, got %v", err)
	}
}
