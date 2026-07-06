package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/astraclawteam/agentatlas/sdk/go/workflow"
	"github.com/astraclawteam/agentatlas/services/agentatlas/schemas"
)

// SOPDocument is the sanitized document context handed to the agent for SOP
// extraction: summaries and sanitized section texts only — never raw unmasked
// originals (the no-raw-copy boundary holds upstream).
type SOPDocument struct {
	Title    string
	Summary  string
	Sections []string
}

// maxSOPSectionRunes bounds each section in the extraction prompt.
const maxSOPSectionRunes = 2000

var (
	wfSchemaOnce sync.Once
	wfSchema     *jsonschema.Schema
	wfSchemaErr  error
)

// workflowSchema compiles the embedded public workflow schema once. The agent
// package validates drafts directly against schemas.WorkflowSchemaJSON —
// deliberately NOT via internal/workflow, which will depend on this package
// for the transform.extract_sop executor.
func workflowSchema() (*jsonschema.Schema, error) {
	wfSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemas.WorkflowSchemaJSON))
		if err != nil {
			wfSchemaErr = fmt.Errorf("workflow schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		const name = "workflow.schema.json"
		if err := c.AddResource(name, doc); err != nil {
			wfSchemaErr = fmt.Errorf("workflow schema: %w", err)
			return
		}
		wfSchema, wfSchemaErr = c.Compile(name)
	})
	return wfSchema, wfSchemaErr
}

// ExtractSOP drives the Knowledge Agent to turn a parsed document into a
// schema-valid SOP workflow draft. The agent — not string templating — decides
// the steps and must emit them via draft_workflow; anything else fails loud.
// The draft is returned unpublished (Version 0): publish stays an admin action.
func ExtractSOP(ctx context.Context, r *Runner, doc SOPDocument) (workflow.Workflow, error) {
	if r == nil {
		return workflow.Workflow{}, fmt.Errorf("sop extraction requires the agent runner")
	}

	var msg strings.Builder
	msg.WriteString("请把下面的文档整理成 SOP 工作流草稿：必须调用 draft_workflow 提交结果；kind 用 sop；risk_level 按内容判断（low/medium/high）；步骤只能用内置节点类型；需要人工确认的环节用 human.confirm 并设 requires_confirmation。\n")
	if doc.Title != "" {
		fmt.Fprintf(&msg, "文档标题：%s\n", doc.Title)
	}
	if doc.Summary != "" {
		fmt.Fprintf(&msg, "文档摘要：%s\n", doc.Summary)
	}
	for i, s := range doc.Sections {
		fmt.Fprintf(&msg, "第 %d 节：%s\n", i+1, truncateRunes(s, maxSOPSectionRunes))
	}

	res, err := r.Run(ctx, "atlas", "sop_"+randomID(), msg.String())
	if err != nil {
		return workflow.Workflow{}, fmt.Errorf("sop agent run: %w", err)
	}
	wf, ok := latestDraftedWorkflow(res.ToolCalls)
	if !ok {
		return workflow.Workflow{}, fmt.Errorf("agent did not produce a workflow draft via draft_workflow (final text: %s)", truncateRunes(res.Text, 200))
	}

	// Defensive re-validation on top of the tool's own checks — a draft that
	// escapes these bounds must never reach the workflow service.
	if wf.Kind != workflow.KindSOP {
		return workflow.Workflow{}, fmt.Errorf("sop extraction produced kind %q, want sop", wf.Kind)
	}
	if wf.Version != 0 {
		return workflow.Workflow{}, fmt.Errorf("sop draft must stay unpublished (version 0), got %d", wf.Version)
	}
	if len(wf.Nodes) == 0 {
		return workflow.Workflow{}, fmt.Errorf("sop draft has no nodes")
	}
	for _, n := range wf.Nodes {
		if !workflow.IsBuiltinNodeType(n.Type) {
			return workflow.Workflow{}, fmt.Errorf("sop draft contains non-builtin node type %q", n.Type)
		}
	}
	switch wf.RiskLevel {
	case workflow.RiskLow, workflow.RiskMedium, workflow.RiskHigh:
	default:
		return workflow.Workflow{}, fmt.Errorf("sop draft risk level %q invalid", wf.RiskLevel)
	}

	raw, err := json.Marshal(wf)
	if err != nil {
		return workflow.Workflow{}, fmt.Errorf("encode sop draft: %w", err)
	}
	sch, err := workflowSchema()
	if err != nil {
		return workflow.Workflow{}, err
	}
	docAny, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return workflow.Workflow{}, fmt.Errorf("sop draft document: %w", err)
	}
	if err := sch.Validate(docAny); err != nil {
		return workflow.Workflow{}, fmt.Errorf("sop draft violates workflow schema: %w", err)
	}
	return wf, nil
}

// latestDraftedWorkflow scans the tool-call trace backwards for the newest
// draft_workflow call whose result decodes into a workflow (failed validation
// attempts earlier in the loop are skipped).
func latestDraftedWorkflow(calls []ToolCall) (workflow.Workflow, bool) {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name != "draft_workflow" || len(calls[i].Result) == 0 {
			continue
		}
		if wf, ok := decodeDraftWorkflow(calls[i].Result); ok {
			return wf, true
		}
	}
	return workflow.Workflow{}, false
}

func decodeDraftWorkflow(raw json.RawMessage) (workflow.Workflow, bool) {
	var direct DraftWorkflowResult
	if json.Unmarshal(raw, &direct) == nil && direct.Workflow.WorkflowID != "" {
		return direct.Workflow, true
	}
	var wrapped struct {
		Output DraftWorkflowResult `json:"output"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && wrapped.Output.Workflow.WorkflowID != "" {
		return wrapped.Output.Workflow, true
	}
	return workflow.Workflow{}, false
}

func randomID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
