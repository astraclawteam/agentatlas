package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/model"

	"github.com/astraclawteam/agentatlas/sdk/go/workflow"
)

// staticLLM satisfies model.LLM for wiring tests.
type staticLLM struct{}

func (staticLLM) Name() string { return "static" }
func (staticLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{TurnComplete: true}, nil)
	}
}

func TestNewKnowledgeAgentWiring(t *testing.T) {
	a, err := NewKnowledgeAgent(staticLLM{})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	if a.Name() != "knowledge_agent" {
		t.Fatalf("name = %q", a.Name())
	}
}

func TestDraftWorkflowValid(t *testing.T) {
	res, err := DraftWorkflow(DraftWorkflowArgs{
		WorkflowID: "wf_mes",
		Kind:       "sop",
		Steps: []DraftWorkflowStep{
			{ID: "in", Type: "input.evidence_pointer"},
			{ID: "parse", Type: "parser.document"},
			{ID: "extract", Type: "transform.extract_sop"},
			{ID: "confirm", Type: "human.confirm", RequiresConfirmation: true},
			{ID: "trace", Type: "trace.append"},
		},
		Edges: []DraftWorkflowEdge{
			{From: "in", To: "parse"}, {From: "parse", To: "extract"},
			{From: "extract", To: "confirm"}, {From: "confirm", To: "trace", Condition: "approved"},
		},
		RiskLevel: "medium",
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	w := res.Workflow
	if w.Version != 0 || w.Kind != workflow.KindSOP || len(w.Nodes) != 5 || len(w.Edges) != 4 {
		t.Fatalf("workflow = %+v", w)
	}
	for _, n := range w.Nodes {
		if !workflow.IsBuiltinNodeType(n.Type) {
			t.Fatalf("non-builtin node leaked: %s", n.Type)
		}
	}
}

func TestDraftWorkflowRejectsUnknownNodeType(t *testing.T) {
	_, err := DraftWorkflow(DraftWorkflowArgs{
		WorkflowID: "wf_bad", Kind: "sop", RiskLevel: "low",
		Steps: []DraftWorkflowStep{{ID: "x", Type: "custom.hack"}},
	})
	if err == nil || !strings.Contains(err.Error(), "custom.hack") {
		t.Fatalf("unknown node type must fail loud, got %v", err)
	}
}

func TestDraftWorkflowRejectsDanglingEdge(t *testing.T) {
	_, err := DraftWorkflow(DraftWorkflowArgs{
		WorkflowID: "wf_bad", Kind: "sop", RiskLevel: "low",
		Steps: []DraftWorkflowStep{{ID: "a", Type: "input.manual"}},
		Edges: []DraftWorkflowEdge{{From: "a", To: "ghost"}},
	})
	if err == nil {
		t.Fatal("dangling edge must fail")
	}
}

func TestDraftRetrievalPlanPipeline(t *testing.T) {
	res, err := DraftRetrievalPlan(DraftRetrievalPlanArgs{
		Query: "我的工作内容是什么", SpaceIDs: []string{"spc_1"}, NeedEvidence: true,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	kinds := make([]string, len(res.Steps))
	for i, s := range res.Steps {
		kinds[i] = s.Kind
	}
	want := []string{"keyword", "vector", "filter", "rerank", "nexus_locate", "nexus_read"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("pipeline = %v", kinds)
	}
	if _, err := DraftRetrievalPlan(DraftRetrievalPlanArgs{Query: "  "}); err == nil {
		t.Fatal("empty query must fail")
	}
}

func TestExplainTraceUsesOnlyProvidedSources(t *testing.T) {
	res, err := ExplainTrace(ExplainTraceArgs{
		TraceID: "tr_1", QuestionSummary: "员工询问工作内容",
		SpaceIDs: []string{"spc_1"}, EvidencePointerIDs: []string{"ev_1"},
		GrantIDs: []string{"grant_1"}, ModelRoute: "llmrouter/deepseek-v4",
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for _, must := range []string{"tr_1", "spc_1", "ev_1", "grant_1", "llmrouter/deepseek-v4"} {
		if !strings.Contains(res.Explanation, must) {
			t.Fatalf("explanation missing %q: %s", must, res.Explanation)
		}
	}
	empty, err := ExplainTrace(ExplainTraceArgs{TraceID: "tr_2"})
	if err != nil {
		t.Fatalf("explain empty: %v", err)
	}
	if !strings.Contains(empty.Explanation, "未读取原文证据") {
		t.Fatalf("no-evidence case must say so: %s", empty.Explanation)
	}
}
