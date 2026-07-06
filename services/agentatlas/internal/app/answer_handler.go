package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
)

const answerInstruction = `你是 AgentAtlas 的回答引擎。只依据下面提供的检索摘要与授权证据回答；引用不到的内容明确说不知道。回答用中文，简洁、落在证据上。`

type AnswerRequest struct {
	EnterpriseID string   `json:"enterprise_id"`
	Question     string   `json:"question"`
	ActorUserID  string   `json:"actor_user_id,omitempty"`
	SpaceHints   []string `json:"space_hints,omitempty"`
}

type AnswerEvidence struct {
	EvidencePointerID string `json:"evidence_pointer_id"`
	Summary           string `json:"summary,omitempty"`
}

type AnswerResponse struct {
	Status          string           `json:"status"`
	Answer          string           `json:"answer,omitempty"`
	RejectionReason string           `json:"rejection_reason,omitempty"`
	TraceID         string           `json:"trace_id"`
	SpaceIDs        []string         `json:"space_ids,omitempty"`
	Evidence        []AnswerEvidence `json:"evidence,omitempty"`
}

// answerDeps wires the runtime answer path.
type answerDeps struct {
	nexus     nexus.Client
	retrieval *retrieval.Service
	traces    *trace.Service
	llm       model.LLM
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

// verifyTicket enforces the single auth path: every runtime call carries an
// AgentNexus-issued ticket. Missing/invalid fails closed.
func verifyTicket(ctx context.Context, client nexus.Client, r *http.Request) (string, nexus.VerifyTicketResponse, error) {
	ticket := r.Header.Get("X-Nexus-Ticket")
	if ticket == "" {
		return "", nexus.VerifyTicketResponse{}, fmt.Errorf("missing X-Nexus-Ticket")
	}
	resp, err := client.VerifyTicket(ctx, nexus.VerifyTicketRequest{TicketID: ticket})
	if err != nil {
		return "", nexus.VerifyTicketResponse{}, fmt.Errorf("verify ticket: %w", err)
	}
	if !resp.Valid {
		return "", nexus.VerifyTicketResponse{}, fmt.Errorf("invalid ticket")
	}
	return ticket, resp, nil
}

func (d *answerDeps) handleAnswer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ticket, identity, err := verifyTicket(ctx, d.nexus, r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	var req AnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.EnterpriseID == "" || strings.TrimSpace(req.Question) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "enterprise_id and question are required")
		return
	}
	if identity.EnterpriseID != "" && identity.EnterpriseID != req.EnterpriseID {
		writeError(w, http.StatusForbidden, "forbidden", "ticket enterprise does not match request")
		return
	}
	actor := req.ActorUserID
	if actor == "" {
		actor = identity.ActorUserID
	}

	steps := []trace.Step{{Kind: "verify_ticket", Detail: map[string]any{"actor_user_id": actor}}}

	// retrieval plan + execution
	q := retrieval.Query{
		EnterpriseID: req.EnterpriseID, Text: req.Question,
		SpaceIDs: req.SpaceHints, TopK: 8,
	}
	planID, err := d.retrieval.CreatePlan(ctx, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "retrieval_plan_failed", err.Error())
		return
	}
	results, err := d.retrieval.Execute(ctx, planID, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "retrieval_failed", err.Error())
		return
	}
	steps = append(steps, trace.Step{Kind: "retrieve", Detail: map[string]any{
		"retrieval_plan_id": planID, "hits": len(results),
	}})

	// authorized evidence reads for the top hits that carry pointers
	var (
		evidenceIDs []string
		grantIDs    []string
		excerpts    []string
		denied      int
	)
	for _, res := range results {
		if res.EvidencePointerID == "" || len(evidenceIDs) >= 3 {
			continue
		}
		loc, err := d.nexus.LocateEvidence(ctx, nexus.LocateEvidenceRequest{
			TicketID: ticket, EnterpriseID: req.EnterpriseID, EvidencePointerID: res.EvidencePointerID,
		})
		if err != nil {
			if errors.Is(err, nexusclient.ErrDenied) {
				denied++
				continue
			}
			writeError(w, http.StatusBadGateway, "nexus_locate_failed", err.Error())
			return
		}
		read, err := d.nexus.ReadEvidence(ctx, nexus.ReadEvidenceRequest{
			TicketID: ticket, EnterpriseID: req.EnterpriseID,
			ResourceURI: loc.ResourceURI, EvidencePointerID: res.EvidencePointerID,
		})
		if err != nil {
			if errors.Is(err, nexusclient.ErrDenied) {
				denied++
				continue
			}
			writeError(w, http.StatusBadGateway, "nexus_read_failed", err.Error())
			return
		}
		evidenceIDs = append(evidenceIDs, res.EvidencePointerID)
		grantIDs = append(grantIDs, read.GrantID)
		excerpts = append(excerpts, read.SanitizedExcerpt)
	}
	steps = append(steps, trace.Step{Kind: "evidence_read", Detail: map[string]any{
		"granted": len(grantIDs), "denied": denied,
	}})

	// generate the answer strictly from the bundle
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "问题：%s\n\n检索摘要：\n", req.Question)
	spaceSet := map[string]bool{}
	for i, res := range results {
		fmt.Fprintf(&prompt, "%d. %s\n", i+1, res.Snippet)
		if res.SpaceID != "" {
			spaceSet[res.SpaceID] = true
		}
	}
	if len(excerpts) > 0 {
		prompt.WriteString("\n授权证据片段：\n")
		for i, e := range excerpts {
			fmt.Fprintf(&prompt, "[%s] %s\n", evidenceIDs[i], e)
		}
	}
	start := time.Now()
	answer, modelRoute, err := generateText(ctx, d.llm, prompt.String())
	if err != nil {
		writeError(w, http.StatusBadGateway, "generation_failed", err.Error())
		return
	}
	steps = append(steps, trace.Step{Kind: "generate", Detail: map[string]any{"model_route": modelRoute}})

	spaceIDs := make([]string, 0, len(spaceSet))
	for id := range spaceSet {
		spaceIDs = append(spaceIDs, id)
	}

	traceRow, err := d.traces.Create(ctx, trace.Record{
		EnterpriseID: req.EnterpriseID, CaseTicketID: ticket, ActorUserID: actor,
		Question: req.Question, SanitizedQuestionSummary: req.Question,
		SpaceIDs: spaceIDs, RetrievalPlanID: planID,
		EvidencePointerIDs: evidenceIDs, ReadGrantIDs: grantIDs,
		ModelRoute: modelRoute, Answer: answer, Steps: steps,
		ModelLatencyMS: time.Since(start).Milliseconds(),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "trace_failed", err.Error())
		return
	}
	// audit append is mandatory: failure fails the whole answer (fail closed)
	if _, err := d.traces.AppendAudit(ctx, d.nexus, ticket, traceRow); err != nil {
		writeError(w, http.StatusInternalServerError, "audit_failed", err.Error())
		return
	}

	evidence := make([]AnswerEvidence, len(evidenceIDs))
	for i, id := range evidenceIDs {
		evidence[i] = AnswerEvidence{EvidencePointerID: id, Summary: truncate(excerpts[i], 200)}
	}
	writeJSON(w, http.StatusOK, AnswerResponse{
		Status: "completed", Answer: answer, TraceID: traceRow.ID,
		SpaceIDs: spaceIDs, Evidence: evidence,
	})
}

// generateText runs one non-streaming completion over the model.LLM contract
// (production: adk-llmrouter-model; tests: deterministic in-process LLM).
func generateText(ctx context.Context, llm model.LLM, userPrompt string) (string, string, error) {
	req := &model.LLMRequest{
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText(answerInstruction)}},
		},
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{genai.NewPartFromText(userPrompt)}},
		},
	}
	var text strings.Builder
	route := llm.Name()
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", "", err
		}
		if resp.ModelVersion != "" {
			route = resp.ModelVersion
		}
		if resp.Content != nil {
			for _, p := range resp.Content.Parts {
				text.WriteString(p.Text)
			}
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", "", fmt.Errorf("model returned empty answer")
	}
	return text.String(), route, nil
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
