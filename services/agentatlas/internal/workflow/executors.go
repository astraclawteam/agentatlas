package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
)

// The executor dependency surfaces. Each node type delegates to the real
// service behind a small interface; the composition root (atlas-worker /
// atlas-agent) wires the concrete implementations.

// ArtifactPipeline parses one ingested artifact synchronously and returns the
// resulting atlas document id (*artifacts.Service satisfies it).
type ArtifactPipeline interface {
	ParseArtifact(ctx context.Context, enterpriseID, artifactID, parserHint string) (string, error)
}

// DocumentLoader loads parsed document context from the sealed AtlasDocument
// (*artifacts.Service satisfies it).
type DocumentLoader interface {
	LoadSOPDocument(ctx context.Context, atlasDocumentID string) (agent.SOPDocument, error)
	LoadParseOutput(ctx context.Context, atlasDocumentID string) (parsergateway.ParseOutput, error)
}

// SOPExtractor turns document context into a schema-valid SOP draft via the
// Knowledge Agent (agent.ExtractSOP bound to a runner).
type SOPExtractor func(ctx context.Context, doc agent.SOPDocument) (sdkworkflow.Workflow, error)

// DocSummarizer produces document summaries (*artifacts.Summarizer satisfies it).
type DocSummarizer interface {
	Summarize(ctx context.Context, out parsergateway.ParseOutput) ([]atlasdocument.Summary, string, error)
}

// Retriever plans and executes hybrid retrieval (*retrieval.Service satisfies it).
type Retriever interface {
	CreatePlan(ctx context.Context, q retrieval.Query) (string, error)
	Execute(ctx context.Context, planID string, q retrieval.Query) ([]retrieval.Result, error)
}

type DreamResolvedInput struct {
	SourceType        string   `json:"source_type"`
	SourceID          string   `json:"source_id"`
	OrgUnitID         string   `json:"org_unit_id"`
	EvidencePointerID string   `json:"evidence_pointer_id"`
	SanitizedText     string   `json:"sanitized_text"`
	Visibility        []string `json:"visibility"`
	ParentRunID       string   `json:"parent_run_id"`
}

type DreamAggregateInput struct {
	OrgUnitID       string               `json:"org_unit_id"`
	WindowStart     time.Time            `json:"window_start"`
	WindowEnd       time.Time            `json:"window_end"`
	Inputs          []DreamResolvedInput `json:"inputs"`
	RiskSignalRules []string             `json:"risk_signal_rules"`
	Coverage        struct {
		ExpectedChildren  int `json:"expected_children"`
		CompletedChildren int `json:"completed_children"`
		InputCount        int `json:"input_count"`
	} `json:"coverage"`
	Missing []struct {
		SourceType string `json:"source_type"`
		SourceID   string `json:"source_id"`
		Reason     string `json:"reason"`
	} `json:"missing"`
}

// DreamAggregator consumes only the typed, sanitized Dream boundary.
type DreamAggregator func(ctx context.Context, input DreamAggregateInput) (map[string]any, error)

// AnswerGenerator produces grounded answer text (llmutil-backed closure at the
// composition root; tests use a deterministic one).
type AnswerGenerator func(ctx context.Context, question string, contextSnippets []string) (answer, modelRoute string, err error)

// Executors carries the real services behind the 13 wired node types.
type Executors struct {
	Artifacts ArtifactPipeline
	Documents DocumentLoader
	SOP       SOPExtractor
	Summarize DocSummarizer
	Retrieval Retriever
	Nexus     nexus.Client
	Dream     DreamAggregator
	Answer    AnswerGenerator
	Traces    *trace.Service
}

// resolveString finds a node parameter: node config wins, then run input,
// then any upstream node output carrying the key.
func resolveString(node sdkworkflow.Node, run *RunContext, key string) (string, bool) {
	if v, ok := node.Config[key].(string); ok && v != "" {
		return v, true
	}
	if v, ok := run.Input[key].(string); ok && v != "" {
		return v, true
	}
	for _, out := range run.Outputs {
		if v, ok := out[key].(string); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// gatherStrings collects every upstream output value under any of the keys.
func gatherStrings(run *RunContext, keys ...string) []string {
	var out []string
	for _, nodeOut := range run.Outputs {
		for _, key := range keys {
			switch v := nodeOut[key].(type) {
			case string:
				if v != "" {
					out = append(out, v)
				}
			case []string:
				out = append(out, v...)
			case []any:
				for _, item := range v {
					if s, ok := item.(string); ok && s != "" {
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

func stringsFromConfig(node sdkworkflow.Node, run *RunContext, key string) []string {
	collect := func(v any) []string {
		switch t := v.(type) {
		case []string:
			return t
		case []any:
			var out []string
			for _, item := range t {
				if s, ok := item.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		return nil
	}
	if v, ok := node.Config[key]; ok {
		return collect(v)
	}
	if v, ok := run.Input[key]; ok {
		return collect(v)
	}
	return nil
}

func requireDep(name string, configured bool) error {
	if !configured {
		return fmt.Errorf("workflow executor dependency %s not configured on this binary", name)
	}
	return nil
}

// NewRegistryWithServices registers real executors for all sixteen node types.
// input.manual / input.evidence_pointer / human.confirm keep the built-in
// behavior; the 13 previously not-wired types delegate to their services.
// Executor errors fail the run loudly (Runtime records a failed event).
func NewRegistryWithServices(e Executors) Registry {
	r := NewRegistry()

	parserExec := func(t sdkworkflow.NodeType) Executor {
		return executorFunc(func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("artifacts", e.Artifacts != nil); err != nil {
				return nil, err
			}
			artifactID, ok := resolveString(node, run, "artifact_id")
			if !ok {
				return nil, fmt.Errorf("node %s (%s): artifact_id missing from config/input/upstream", node.ID, t)
			}
			hint, _ := resolveString(node, run, "parser_hint")
			docID, err := e.Artifacts.ParseArtifact(ctx, run.EnterpriseID, artifactID, hint)
			if err != nil {
				return nil, fmt.Errorf("node %s (%s): %w", node.ID, t, err)
			}
			return map[string]any{"atlas_document_id": docID, "artifact_id": artifactID}, nil
		})
	}
	for _, t := range []sdkworkflow.NodeType{
		sdkworkflow.NodeParserDocument, sdkworkflow.NodeParserImage,
		sdkworkflow.NodeParserLongImage, sdkworkflow.NodeParserAudio,
		sdkworkflow.NodeParserVideo,
	} {
		r.Register(t, parserExec(t))
	}

	r.Register(sdkworkflow.NodeTransformExtractSOP, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("sop extractor", e.SOP != nil && e.Documents != nil); err != nil {
				return nil, err
			}
			docID, ok := resolveString(node, run, "atlas_document_id")
			if !ok {
				return nil, fmt.Errorf("node %s: atlas_document_id missing (parse the document first)", node.ID)
			}
			doc, err := e.Documents.LoadSOPDocument(ctx, docID)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			wf, err := e.SOP(ctx, doc)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			return map[string]any{"workflow": wf, "workflow_id": wf.WorkflowID}, nil
		}))

	r.Register(sdkworkflow.NodeTransformSummarize, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("summarizer", e.Summarize != nil && e.Documents != nil); err != nil {
				return nil, err
			}
			docID, ok := resolveString(node, run, "atlas_document_id")
			if !ok {
				return nil, fmt.Errorf("node %s: atlas_document_id missing (parse the document first)", node.ID)
			}
			po, err := e.Documents.LoadParseOutput(ctx, docID)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			sums, source, err := e.Summarize.Summarize(ctx, po)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			outSums := make([]any, 0, len(sums))
			var display string
			for _, s := range sums {
				outSums = append(outSums, map[string]any{"level": string(s.Level), "text": s.Text})
				if s.Level == atlasdocument.SummaryDisplay {
					display = s.Text
				}
			}
			return map[string]any{"summaries": outSums, "summary": display, "source": source}, nil
		}))

	r.Register(sdkworkflow.NodeRetrievalSearch, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("retrieval", e.Retrieval != nil); err != nil {
				return nil, err
			}
			query, ok := resolveString(node, run, "query")
			if !ok {
				return nil, fmt.Errorf("node %s: query missing from config/input/upstream", node.ID)
			}
			q := retrieval.Query{EnterpriseID: run.EnterpriseID, Text: query}
			if topK, ok := node.Config["top_k"].(float64); ok {
				q.TopK = int(topK)
			}
			planID, err := e.Retrieval.CreatePlan(ctx, q)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			results, err := e.Retrieval.Execute(ctx, planID, q)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			hits := make([]any, 0, len(results))
			snippets := make([]string, 0, len(results))
			pointers := make([]string, 0, len(results))
			for _, res := range results {
				hits = append(hits, map[string]any{
					"doc_id": res.DocID, "snippet": res.Snippet,
					"evidence_pointer_id": res.EvidencePointerID, "score": res.Score,
				})
				if res.Snippet != "" {
					snippets = append(snippets, res.Snippet)
				}
				if res.EvidencePointerID != "" {
					pointers = append(pointers, res.EvidencePointerID)
				}
			}
			return map[string]any{
				"plan_id": planID, "hits": hits,
				"snippets": snippets, "evidence_pointer_ids": pointers,
			}, nil
		}))

	r.Register(sdkworkflow.NodeNexusLocate, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("nexus", e.Nexus != nil); err != nil {
				return nil, err
			}
			ticketID, ok := resolveString(node, run, "ticket_id")
			if !ok {
				return nil, fmt.Errorf("node %s: ticket_id required for nexus.locate (fail closed)", node.ID)
			}
			pointerID, _ := resolveString(node, run, "evidence_pointer_id")
			if pointerID == "" {
				return nil, fmt.Errorf("node %s: evidence_pointer_id missing", node.ID)
			}
			resp, err := e.Nexus.LocateEvidence(ctx, nexus.LocateEvidenceRequest{
				TicketID: ticketID, EnterpriseID: run.EnterpriseID, EvidencePointerID: pointerID,
			})
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			return map[string]any{"resource_uri": resp.ResourceURI, "source_system": resp.SourceSystem}, nil
		}))

	r.Register(sdkworkflow.NodeNexusRead, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("nexus", e.Nexus != nil); err != nil {
				return nil, err
			}
			ticketID, ok := resolveString(node, run, "ticket_id")
			if !ok {
				return nil, fmt.Errorf("node %s: ticket_id required for nexus.read (fail closed)", node.ID)
			}
			uri, ok := resolveString(node, run, "resource_uri")
			if !ok {
				return nil, fmt.Errorf("node %s: resource_uri missing (locate first)", node.ID)
			}
			pointerID, _ := resolveString(node, run, "evidence_pointer_id")
			resp, err := e.Nexus.ReadEvidence(ctx, nexus.ReadEvidenceRequest{
				TicketID: ticketID, EnterpriseID: run.EnterpriseID,
				ResourceURI: uri, EvidencePointerID: pointerID,
			})
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			return map[string]any{
				"grant_id": resp.GrantID, "sanitized_excerpt": resp.SanitizedExcerpt,
				"content_hash": resp.ContentHash,
			}, nil
		}))

	r.Register(sdkworkflow.NodeDreamAggregate, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if run.Dream == nil || run.Dream.WorkflowID == "" || run.Dream.WorkflowVersion < 1 {
				return nil, fmt.Errorf("node %s: verified Dream execution context required", node.ID)
			}
			if err := requireDep("dream aggregator", e.Dream != nil); err != nil {
				return nil, err
			}
			input, err := decodeDreamAggregateInput(run.Input)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			if input.OrgUnitID != run.Dream.OrgUnitID {
				return nil, fmt.Errorf("node %s: Dream org input disagrees with verified context", node.ID)
			}
			var evidence, parents []string
			for _, item := range input.Inputs {
				if item.EvidencePointerID != "" {
					evidence = append(evidence, item.EvidencePointerID)
				}
				if item.ParentRunID != "" {
					parents = append(parents, item.ParentRunID)
				}
			}
			if !slices.Equal(evidence, run.Dream.EvidencePointerIDs) || !slices.Equal(parents, run.Dream.ParentDreamRunIDs) {
				return nil, fmt.Errorf("node %s: Dream lineage input disagrees with verified context", node.ID)
			}
			out, err := e.Dream(ctx, input)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			if out == nil {
				return nil, fmt.Errorf("node %s: dream aggregator returned nil output", node.ID)
			}
			out["model_route"] = "workflow/" + run.Dream.WorkflowID
			out["model_version"] = fmt.Sprintf("v%d", run.Dream.WorkflowVersion)
			if err := validateDreamAggregateOutput(out); err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			return out, nil
		}))

	r.Register(sdkworkflow.NodeAnswerGenerate, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("answer generator", e.Answer != nil); err != nil {
				return nil, err
			}
			question, ok := resolveString(node, run, "question")
			if !ok {
				return nil, fmt.Errorf("node %s: question missing from config/input/upstream", node.ID)
			}
			snippets := gatherStrings(run, "sanitized_excerpt", "snippets", "summary", "display")
			answer, route, err := e.Answer(ctx, question, snippets)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			return map[string]any{"answer": answer, "model_route": route}, nil
		}))

	r.Register(sdkworkflow.NodeTraceAppend, executorFunc(
		func(ctx context.Context, node sdkworkflow.Node, run *RunContext) (map[string]any, error) {
			if err := requireDep("trace service", e.Traces != nil); err != nil {
				return nil, err
			}
			record, err := traceRecord(node, run)
			if err != nil {
				return nil, err
			}
			row, err := e.Traces.Create(ctx, record)
			if err != nil {
				return nil, fmt.Errorf("node %s: %w", node.ID, err)
			}
			return map[string]any{"trace_id": row.ID}, nil
		}))

	return r
}

func validateDreamAggregateOutput(out map[string]any) error {
	allowed := map[string]bool{"display": true, "retrieval": true, "sealed_detail": true, "source": true, "model_route": true, "model_version": true, "facts": true, "themes": true, "trends": true, "risks": true, "todos": true}
	if len(out) != len(allowed) {
		return fmt.Errorf("Dream output contains unknown or missing fields")
	}
	for key := range out {
		if !allowed[key] {
			return fmt.Errorf("Dream output contains unknown field %s", key)
		}
	}
	for key, max := range map[string]int{"display": 4000, "retrieval": 4000, "sealed_detail": 100000, "source": 128, "model_route": 128, "model_version": 128} {
		value, ok := out[key].(string)
		if !ok || strings.TrimSpace(value) == "" || len([]rune(value)) > max {
			return fmt.Errorf("Dream output %s is missing or exceeds bound %d", key, max)
		}
	}
	for _, key := range []string{"facts", "themes", "trends", "risks", "todos"} {
		values, ok := out[key].([]any)
		if !ok || values == nil || len(values) > 500 {
			return fmt.Errorf("Dream output %s must be a non-null bounded collection", key)
		}
		for _, value := range values {
			item, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("Dream output %s contains malformed signal", key)
			}
			if len(item) != 5 {
				return fmt.Errorf("Dream output %s signal contains unknown or missing fields", key)
			}
			for field, max := range map[string]int{"id": 128, "title": 200, "detail": 2000, "evidence_pointer_id": 256} {
				text, ok := item[field].(string)
				if !ok || strings.TrimSpace(text) == "" || len([]rune(text)) > max {
					return fmt.Errorf("Dream output %s signal has invalid %s", key, field)
				}
			}
			severity, _ := item["severity"].(string)
			if severity != "info" && severity != "warning" && severity != "critical" {
				return fmt.Errorf("Dream output %s signal has invalid severity", key)
			}
		}
	}
	return nil
}

func traceRecord(node sdkworkflow.Node, run *RunContext) (trace.Record, error) {
	ticketID, ok := resolveString(node, run, "ticket_id")
	if !ok {
		if run.Dream != nil && run.Dream.DreamRunID != "" {
			ticketID = "dream-run/" + run.Dream.DreamRunID
		} else {
			return trace.Record{}, fmt.Errorf("node %s: ticket_id required for trace.append", node.ID)
		}
	}
	actor, _ := resolveString(node, run, "actor_user_id")
	if actor == "" {
		actor = "workflow"
	}
	question, _ := resolveString(node, run, "question")
	answer, _ := resolveString(node, run, "answer")
	evidence := gatherStrings(run, "evidence_pointer_id", "evidence_pointer_ids")
	if run.Dream != nil {
		evidence = append(evidence, run.Dream.EvidencePointerIDs...)
	}
	record := trace.Record{
		EnterpriseID: run.EnterpriseID, CaseTicketID: ticketID, ActorUserID: actor,
		Question: question, SanitizedQuestionSummary: question, WorkflowRunID: run.RunID,
		EvidencePointerIDs: evidence, ReadGrantIDs: gatherStrings(run, "grant_id"),
		ModelRoute: firstString(run, "model_route"), Answer: answer,
	}
	if run.Dream != nil && run.Dream.DreamRunID != "" {
		record.Steps = []trace.Step{{Kind: "dream.lineage", Detail: map[string]any{
			"dream_run_id": run.Dream.DreamRunID, "dream_policy_id": run.Dream.PolicyID,
			"dream_policy_version": run.Dream.PolicyVersion, "workflow_id": run.Dream.WorkflowID,
			"workflow_version": run.Dream.WorkflowVersion, "parent_dream_run_ids": run.Dream.ParentDreamRunIDs,
		}}}
	}
	return record, nil
}

func inputStrings(value any) []string {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func firstString(run *RunContext, key string) string {
	for _, out := range run.Outputs {
		if v, ok := out[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func decodeDreamAggregateInput(input map[string]any) (DreamAggregateInput, error) {
	allowed := map[string]bool{"org_unit_id": true, "window_start": true, "window_end": true, "inputs": true, "coverage": true, "missing": true, "risk_signal_rules": true}
	if len(input) != len(allowed) {
		return DreamAggregateInput{}, fmt.Errorf("typed Dream input contains unknown or missing fields")
	}
	for key := range input {
		if !allowed[key] {
			return DreamAggregateInput{}, fmt.Errorf("typed Dream input contains unknown field %s", key)
		}
	}
	selected := map[string]any{}
	for _, key := range []string{"org_unit_id", "window_start", "window_end", "inputs", "coverage", "missing", "risk_signal_rules"} {
		selected[key] = input[key]
	}
	raw, err := json.Marshal(selected)
	if err != nil {
		return DreamAggregateInput{}, fmt.Errorf("encode typed Dream input: %w", err)
	}
	var decoded DreamAggregateInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return DreamAggregateInput{}, fmt.Errorf("decode typed Dream input: %w", err)
	}
	if strings.TrimSpace(decoded.OrgUnitID) == "" || decoded.WindowStart.IsZero() || decoded.WindowEnd.IsZero() || !decoded.WindowEnd.After(decoded.WindowStart) {
		return DreamAggregateInput{}, fmt.Errorf("typed Dream input requires org unit and increasing window")
	}
	if len(decoded.Inputs) > 1000 || len(decoded.Missing) > 1000 {
		return DreamAggregateInput{}, fmt.Errorf("typed Dream input exceeds bound 1000")
	}
	if len(decoded.RiskSignalRules) > 100 {
		return DreamAggregateInput{}, fmt.Errorf("typed Dream risk rules exceed bound 100")
	}
	if decoded.Coverage.ExpectedChildren < 0 || decoded.Coverage.CompletedChildren < 0 || decoded.Coverage.CompletedChildren > decoded.Coverage.ExpectedChildren || decoded.Coverage.InputCount != len(decoded.Inputs) {
		return DreamAggregateInput{}, fmt.Errorf("typed Dream input has invalid coverage")
	}
	for _, item := range decoded.Inputs {
		if !validDreamSource(item.SourceType) || item.SourceID == "" || len([]rune(item.SourceID)) > 256 || item.OrgUnitID == "" || len([]rune(item.OrgUnitID)) > 128 || item.SanitizedText == "" || len([]rune(item.SanitizedText)) > 4000 || len(item.EvidencePointerID) > 256 || len(item.ParentRunID) > 128 || len(item.Visibility) == 0 || len(item.Visibility) > 64 {
			return DreamAggregateInput{}, fmt.Errorf("typed Dream input contains invalid or unbounded resolved input")
		}
		for _, v := range item.Visibility {
			if strings.TrimSpace(v) == "" || len([]rune(v)) > 128 {
				return DreamAggregateInput{}, fmt.Errorf("typed Dream input contains invalid visibility")
			}
		}
	}
	for _, item := range decoded.Missing {
		if !validDreamSource(item.SourceType) || item.SourceID == "" || len([]rune(item.SourceID)) > 256 || !validDreamMissingReason(item.Reason) {
			return DreamAggregateInput{}, fmt.Errorf("typed Dream input contains invalid missing record")
		}
	}
	for _, rule := range decoded.RiskSignalRules {
		if strings.TrimSpace(rule) == "" || len([]rune(rule)) > 256 {
			return DreamAggregateInput{}, fmt.Errorf("typed Dream input contains invalid risk rule")
		}
		if _, err := regexp.Compile(rule); err != nil {
			return DreamAggregateInput{}, fmt.Errorf("typed Dream risk rule: %w", err)
		}
	}
	return decoded, nil
}

func validDreamSource(v string) bool {
	switch v {
	case "work_brief", "child_dream_summary", "project_record", "sop_update", "agent_answer", "external_evidence", "completed_task", "risk_event":
		return true
	}
	return false
}
func validDreamMissingReason(v string) bool {
	switch v {
	case "not_found", "not_completed", "not_authorized", "failed", "masked":
		return true
	}
	return false
}
