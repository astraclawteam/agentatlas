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

	"go.opentelemetry.io/otel/attribute"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
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
	nexus nexus.Client
	// evidence is the frozen-contract evidence surface.
	evidence  FrozenEvidenceClient
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
	// No model, no answer — refuse before the retrieval and the authorized
	// evidence reads below, which would otherwise spend real AgentNexus grants
	// on a request that cannot produce anything. Checked after the auth
	// decisions so an unauthorized caller still learns nothing about wiring.
	if d.llm == nil {
		writeError(w, http.StatusServiceUnavailable, "generation_unavailable", "answer generation is not configured")
		return
	}
	actor := req.ActorUserID
	if actor == "" {
		actor = identity.ActorUserID
	}

	// span covers plan/retrieve/read/generate/trace/audit (Goal B6)
	ctx, span := observability.Tracer("answer").Start(ctx, "answer.handle")
	span.SetAttributes(attribute.String("enterprise_id", req.EnterpriseID), attribute.String("actor_user_id", actor))
	defer span.End()

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
	// Task 18A Part A: only governed, evidence-grounded documents may ground an
	// answer/citation. Non-authoritative digests (e.g. the legacy Dream
	// dream_summary) are excluded from every governed-knowledge citation path —
	// they can never be presented or published as governed knowledge.
	governed, nonAuthoritative := retrieval.GovernedKnowledge(results)
	steps = append(steps, trace.Step{Kind: "retrieve", Detail: map[string]any{
		"retrieval_plan_id": planID, "hits": len(results),
		"authoritative": len(governed), "non_authoritative_excluded": len(nonAuthoritative),
	}})

	// authorized evidence reads for the top hits that carry pointers
	var (
		evidenceIDs []string
		grantIDs    []string
		excerpts    []string
		denied      int
	)
	for _, res := range governed {
		if res.EvidencePointerID == "" || len(evidenceIDs) >= 3 {
			continue
		}
		if d.evidence == nil {
			writeError(w, http.StatusServiceUnavailable, "evidence_unavailable", "AgentNexus evidence surface unavailable")
			return
		}
		located, err := d.evidence.Locate(ctx, ticket, nexusruntime.EvidenceRequest{
			RequestID: "answer-locate-" + res.EvidencePointerID,
			Purpose:   answerEvidencePurpose,
			DataNeeds: []nexusruntime.DataNeed{{
				NeedID:    res.EvidencePointerID,
				DataClass: answerEvidenceDataClass,
				Purpose:   answerEvidencePurpose,
			}},
			ExpiresAt: time.Now().Add(evidenceRequestTTL).UTC(),
		})
		if err != nil {
			if errors.Is(err, nexusclient.ErrDenied) {
				denied++
				continue
			}
			writeError(w, http.StatusBadGateway, "nexus_locate_failed", err.Error())
			return
		}
		if len(located.Evidence) == 0 {
			// Located nothing is not a denial and not a fault: the pointer
			// simply resolves to no readable evidence for this principal.
			continue
		}
		read, err := d.evidence.Read(ctx, ticket, nexusruntime.EvidenceReadRequest{
			RequestID:          "answer-read-" + res.EvidencePointerID,
			BusinessContextRef: located.BusinessContextRef,
			EvidenceRef:        located.Evidence[0].EvidenceRef,
			Purpose:            answerEvidencePurpose,
			ExpiresAt:          time.Now().Add(evidenceRequestTTL).UTC(),
		})
		if err != nil {
			if errors.Is(err, nexusclient.ErrDenied) {
				denied++
				continue
			}
			writeError(w, http.StatusBadGateway, "nexus_read_failed", err.Error())
			return
		}
		// A refusal is a 200 with {"decision":"deny"} and no data, so it never
		// reaches the ErrDenied branch above. Counting it as denied here is what
		// keeps an empty Data map from being serialized into the prompt as a
		// cited excerpt — an ungrounded claim entering the answer.
		if !read.Allowed() {
			denied++
			continue
		}
		evidenceIDs = append(evidenceIDs, res.EvidencePointerID)
		grantIDs = append(grantIDs, read.GrantRef)
		// The frozen read returns normalized, policy-masked business FIELDS
		// rather than a prose excerpt. Serialize them verbatim instead of
		// summarizing: any summarization here would be an ungrounded claim
		// entering the answer, and the Answer Trace could no longer point at
		// what the model actually saw.
		rendered, err := json.Marshal(read.Data)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "evidence_render_failed", err.Error())
			return
		}
		excerpts = append(excerpts, string(rendered))
	}
	steps = append(steps, trace.Step{Kind: "evidence_read", Detail: map[string]any{
		"granted": len(grantIDs), "denied": denied,
	}})

	// generate the answer strictly from the bundle
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "问题：%s\n\n检索摘要：\n", req.Question)
	spaceSet := map[string]bool{}
	for i, res := range governed {
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
