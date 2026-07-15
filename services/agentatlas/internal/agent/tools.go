// Package agent hosts the Knowledge Agent: an ADK llmagent wired to the
// llmrouter model adapter and the knowledge tools (workflow drafts, retrieval
// plan drafts, answer trace explanation).
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	assessmentmodel "github.com/astraclawteam/agentatlas/sdk/go/assessment"
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

// builtinNodeTypeList renders the sixteen built-in node types for prompts and
// error messages — workflow.BuiltinNodeTypes stays the single source of truth.
func builtinNodeTypeList() string {
	names := make([]string, len(workflow.BuiltinNodeTypes))
	for i, t := range workflow.BuiltinNodeTypes {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
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
			return DraftWorkflowResult{}, fmt.Errorf("unknown node type %q (node %s); 只允许这些内置节点类型: %s", s.Type, s.ID, builtinNodeTypeList())
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
		Name: "draft_workflow",
		Description: "把步骤列表整理成合法的 AgentAtlas 工作流草稿。step.type 只允许这 16 种内置节点类型: " +
			builtinNodeTypeList() + "。kind 取 sop/dream/ingestion/answer，risk_level 取 low/medium/high。",
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

// --- query_work_assessment (Task 18D manager assessment-query tool) -----------
//
// This tool lets an authorized MANAGER read a subordinate's work-assessment
// detail (score/level + manager notes) through the Atlas Assistant. It calls the
// DETERMINISTIC visibility service (assessment.ManagerVisibilityService, which
// itself re-uses the Task 18C evaluation result and the exact hierarchy + policy
// authorization) and returns its projection VERBATIM: the LLM cannot broaden the
// hierarchy scope and cannot rewrite the score, attribution or confidence.
//
// It is deliberately NOT registered in the process-global, dependency-free
// Tools() set: the trusted tenant + manager identity + org scope must be bound
// per request (never taken from an LLM argument), so the management plane
// constructs this tool with a request-scoped AssessmentQueryPort. The LLM arg
// carries ONLY the assessment id to look up.

// AssessmentQueryPort is the deterministic visibility service the tool calls. Its
// implementation (assessment.ManagerVisibilityService bound to a trusted manager
// identity + tenant) enforces the exact hierarchy + policy authorization and
// returns a fail-closed error on an out-of-scope or unowned-policy read. The port
// is expressed over the frozen SDK WorkAssessment so this package never imports
// internal/assessment (which would create an import cycle through governance ->
// workflow -> agent).
type AssessmentQueryPort interface {
	ManagerView(ctx context.Context, assessmentID string) (assessmentmodel.WorkAssessment, error)
}

// ManagerAssessmentQueryArgs is the LLM-suppliable query: ONLY the assessment id.
// The trusted tenant + manager identity + org scope are bound into the port, so
// the model can never widen scope or name another tenant/subject.
type ManagerAssessmentQueryArgs struct {
	AssessmentID string `json:"assessment_id"`
}

// ManagerAssessmentQueryResult carries the authorized, deterministic assessment
// projection verbatim. The model may narrate around it but can never alter the
// score, attribution or confidence.
type ManagerAssessmentQueryResult struct {
	Assessment assessmentmodel.WorkAssessment `json:"assessment"`
}

// ManagerAssessmentQuery calls the deterministic port and returns its result
// unchanged. It fails loud on an empty id and propagates the port's fail-closed
// authorization error; it can never modify the structured result.
func ManagerAssessmentQuery(ctx context.Context, port AssessmentQueryPort, args ManagerAssessmentQueryArgs) (ManagerAssessmentQueryResult, error) {
	if port == nil {
		return ManagerAssessmentQueryResult{}, fmt.Errorf("assessment query service is not configured")
	}
	if strings.TrimSpace(args.AssessmentID) == "" {
		return ManagerAssessmentQueryResult{}, fmt.Errorf("assessment_id is required")
	}
	wa, err := port.ManagerView(ctx, args.AssessmentID)
	if err != nil {
		return ManagerAssessmentQueryResult{}, err
	}
	return ManagerAssessmentQueryResult{Assessment: wa}, nil
}

// ManagerAssessmentQueryTool wraps ManagerAssessmentQuery as an ADK function tool
// bound to a request-scoped, trusted AssessmentQueryPort.
func ManagerAssessmentQueryTool(port AssessmentQueryPort) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{
		Name:        "query_work_assessment",
		Description: "读取一名下属员工的工作评估详情（分数/等级/主管备注）。只能读取你所在管理层级范围内、且由你负责的评估策略下的评估；越权、跨汇报线或跨策略的读取会被拒绝。返回的是确定性评估投影，模型不得修改其分数、归因或置信度，也不得据此触发任何人事动作。",
		// The assessment result is a recursive type (a WorkAssessment embeds
		// sub-assessments), which the reflective schema inferencer cannot walk, so
		// the output schema is declared explicitly as an opaque object.
		OutputSchema: &jsonschema.Schema{Type: "object"},
	}, func(ctx adkagent.ToolContext, args ManagerAssessmentQueryArgs) (ManagerAssessmentQueryResult, error) {
		return ManagerAssessmentQuery(ctx, port, args)
	})
}
