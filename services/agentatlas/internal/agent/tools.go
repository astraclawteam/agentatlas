// Package agent hosts the Knowledge Agent: an ADK llmagent wired to the
// llmrouter model adapter and the knowledge tools (workflow drafts, retrieval
// plan drafts, answer trace explanation).
package agent

import (
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/astraclawteam/agentatlas/sdk/go/workflow"
)

// --- draft_workflow ---------------------------------------------------------

type DraftWorkflowStep struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	Name                 string `json:"name,omitempty"`
	RequiresConfirmation bool   `json:"requires_confirmation,omitempty"`
}

type DraftWorkflowEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

type DraftWorkflowArgs struct {
	WorkflowID string              `json:"workflow_id"`
	Kind       string              `json:"kind"`
	Steps      []DraftWorkflowStep `json:"steps"`
	Edges      []DraftWorkflowEdge `json:"edges"`
	RiskLevel  string              `json:"risk_level"`
}

type DraftWorkflowResult struct {
	Workflow workflow.Workflow `json:"workflow"`
}

// DraftWorkflow builds a schema-shaped workflow draft from agent-provided
// steps. Unknown node types fail loud — the agent must stay inside the
// sixteen built-in types.
func DraftWorkflow(args DraftWorkflowArgs) (DraftWorkflowResult, error) {
	if args.WorkflowID == "" {
		return DraftWorkflowResult{}, fmt.Errorf("workflow_id is required")
	}
	kind := workflow.Kind(args.Kind)
	switch kind {
	case workflow.KindSOP, workflow.KindDream, workflow.KindIngestion, workflow.KindAnswer:
	default:
		return DraftWorkflowResult{}, fmt.Errorf("unknown workflow kind %q", args.Kind)
	}
	risk := workflow.RiskLevel(args.RiskLevel)
	switch risk {
	case workflow.RiskLow, workflow.RiskMedium, workflow.RiskHigh:
	default:
		return DraftWorkflowResult{}, fmt.Errorf("unknown risk level %q", args.RiskLevel)
	}
	if len(args.Steps) == 0 {
		return DraftWorkflowResult{}, fmt.Errorf("at least one step is required")
	}

	nodeIDs := map[string]bool{}
	nodes := make([]workflow.Node, 0, len(args.Steps))
	for _, s := range args.Steps {
		t := workflow.NodeType(s.Type)
		if !workflow.IsBuiltinNodeType(t) {
			return DraftWorkflowResult{}, fmt.Errorf("unknown node type %q (node %s)", s.Type, s.ID)
		}
		if s.ID == "" || nodeIDs[s.ID] {
			return DraftWorkflowResult{}, fmt.Errorf("node id %q missing or duplicated", s.ID)
		}
		nodeIDs[s.ID] = true
		nodes = append(nodes, workflow.Node{
			ID: s.ID, Type: t, Name: s.Name, RequiresConfirmation: s.RequiresConfirmation,
		})
	}
	edges := make([]workflow.Edge, 0, len(args.Edges))
	for _, e := range args.Edges {
		if !nodeIDs[e.From] || !nodeIDs[e.To] {
			return DraftWorkflowResult{}, fmt.Errorf("edge %s->%s references unknown node", e.From, e.To)
		}
		edges = append(edges, workflow.Edge{From: e.From, To: e.To, Condition: e.Condition})
	}

	return DraftWorkflowResult{Workflow: workflow.Workflow{
		WorkflowID: args.WorkflowID,
		Version:    0, // draft
		Kind:       kind,
		Nodes:      nodes,
		Edges:      edges,
		RiskLevel:  risk,
	}}, nil
}

// --- draft_retrieval_plan -----------------------------------------------------

type DraftRetrievalPlanArgs struct {
	Query        string   `json:"query"`
	SpaceIDs     []string `json:"space_ids,omitempty"`
	OrgScopes    []string `json:"org_scopes,omitempty"`
	SourceTypes  []string `json:"source_types,omitempty"`
	MaxRiskLevel string   `json:"max_risk_level,omitempty"`
	NeedEvidence bool     `json:"need_evidence,omitempty"`
}

type RetrievalPlanStep struct {
	StepNo int            `json:"step_no"`
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params"`
}

type DraftRetrievalPlanResult struct {
	Steps []RetrievalPlanStep `json:"steps"`
}

// DraftRetrievalPlan produces the deterministic hybrid retrieval pipeline:
// keyword -> vector -> filter -> rerank, plus nexus locate/read when original
// evidence excerpts are required.
func DraftRetrievalPlan(args DraftRetrievalPlanArgs) (DraftRetrievalPlanResult, error) {
	if strings.TrimSpace(args.Query) == "" {
		return DraftRetrievalPlanResult{}, fmt.Errorf("query is required")
	}
	scope := map[string]any{
		"space_ids":  args.SpaceIDs,
		"org_scopes": args.OrgScopes,
	}
	steps := []RetrievalPlanStep{
		{StepNo: 1, Kind: "keyword", Params: map[string]any{"query": args.Query, "scope": scope}},
		{StepNo: 2, Kind: "vector", Params: map[string]any{"query": args.Query, "scope": scope}},
		{StepNo: 3, Kind: "filter", Params: map[string]any{
			"source_types":   args.SourceTypes,
			"max_risk_level": defaultString(args.MaxRiskLevel, "medium"),
		}},
		{StepNo: 4, Kind: "rerank", Params: map[string]any{"query": args.Query, "top_k": 10}},
	}
	if args.NeedEvidence {
		steps = append(steps,
			RetrievalPlanStep{StepNo: 5, Kind: "nexus_locate", Params: map[string]any{"top_k": 3}},
			RetrievalPlanStep{StepNo: 6, Kind: "nexus_read", Params: map[string]any{"max_bytes": 65536}},
		)
	}
	return DraftRetrievalPlanResult{Steps: steps}, nil
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// --- explain_answer_trace -----------------------------------------------------

type ExplainTraceArgs struct {
	TraceID            string   `json:"trace_id"`
	QuestionSummary    string   `json:"question_summary,omitempty"`
	SpaceIDs           []string `json:"space_ids,omitempty"`
	EvidencePointerIDs []string `json:"evidence_pointer_ids,omitempty"`
	GrantIDs           []string `json:"grant_ids,omitempty"`
	ModelRoute         string   `json:"model_route,omitempty"`
}

type ExplainTraceResult struct {
	Explanation string `json:"explanation"`
}

// ExplainTrace renders a deterministic, human-readable provenance summary the
// agent can elaborate on. It never fabricates sources: only the ids passed in
// appear in the output.
func ExplainTrace(args ExplainTraceArgs) (ExplainTraceResult, error) {
	if args.TraceID == "" {
		return ExplainTraceResult{}, fmt.Errorf("trace_id is required")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "回答追溯 %s：", args.TraceID)
	if args.QuestionSummary != "" {
		fmt.Fprintf(&b, "问题摘要「%s」。", args.QuestionSummary)
	}
	if len(args.SpaceIDs) > 0 {
		fmt.Fprintf(&b, "使用知识空间 %s。", strings.Join(args.SpaceIDs, "、"))
	}
	if len(args.EvidencePointerIDs) > 0 {
		fmt.Fprintf(&b, "引用证据指针 %s。", strings.Join(args.EvidencePointerIDs, "、"))
	} else {
		b.WriteString("未读取原文证据。")
	}
	if len(args.GrantIDs) > 0 {
		fmt.Fprintf(&b, "原文读取经 AgentNexus 授权（grant %s）。", strings.Join(args.GrantIDs, "、"))
	}
	if args.ModelRoute != "" {
		fmt.Fprintf(&b, "生成模型路由 %s。", args.ModelRoute)
	}
	return ExplainTraceResult{Explanation: b.String()}, nil
}

// Tools wraps the handlers as ADK function tools.
func Tools() ([]tool.Tool, error) {
	draftWorkflow, err := functiontool.New(functiontool.Config{
		Name:        "draft_workflow",
		Description: "把步骤列表整理成合法的 AgentAtlas 工作流草稿（仅允许 16 种内置节点类型）",
	}, func(_ adkagent.ToolContext, args DraftWorkflowArgs) (DraftWorkflowResult, error) {
		return DraftWorkflow(args)
	})
	if err != nil {
		return nil, err
	}
	draftPlan, err := functiontool.New(functiontool.Config{
		Name:        "draft_retrieval_plan",
		Description: "为问题生成混合检索计划（keyword/vector/filter/rerank，可选 AgentNexus 证据读取步骤）",
	}, func(_ adkagent.ToolContext, args DraftRetrievalPlanArgs) (DraftRetrievalPlanResult, error) {
		return DraftRetrievalPlan(args)
	})
	if err != nil {
		return nil, err
	}
	explain, err := functiontool.New(functiontool.Config{
		Name:        "explain_answer_trace",
		Description: "把 Answer Trace 的字段整理成可读的证据来源说明（不得虚构来源）",
	}, func(_ adkagent.ToolContext, args ExplainTraceArgs) (ExplainTraceResult, error) {
		return ExplainTrace(args)
	})
	if err != nil {
		return nil, err
	}
	return []tool.Tool{draftWorkflow, draftPlan, explain}, nil
}
