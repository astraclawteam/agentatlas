package app

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
)

// echoLLM answers deterministically from the prompt (no network).
type echoLLM struct{}

func (echoLLM) Name() string { return "test/echo" }
func (echoLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var user string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				user += p.Text
			}
		}
		answer := "基于证据的回答：" + user[:min(60, len(user))]
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{genai.NewPartFromText(answer)}},
			ModelVersion: "test/echo",
			TurnComplete: true,
		}, nil)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// staticSearch returns one canned hit.
type staticSearch struct{}

func (staticSearch) EnsureIndex(context.Context, string) error        { return nil }
func (staticSearch) Index(context.Context, string, string, any) error { return nil }
func (staticSearch) Search(context.Context, string, any) (retrieval.SearchResult, error) {
	src, _ := json.Marshal(retrieval.IndexDocument{
		EnterpriseID: "ent_1", SpaceID: "spc_emp", SourceType: "work_brief",
		SummaryText: "完成分拣规则联调", SanitizedSnippet: "完成分拣规则联调",
		EvidencePointerID: "ev_1",
	})
	return retrieval.SearchResult{Total: 1, Hits: []retrieval.Hit{{ID: "d1", Score: 1, Source: src}}}, nil
}

type memPlanStore struct{ results int }

func (m *memPlanStore) CreateRetrievalPlan(_ context.Context, arg db.CreateRetrievalPlanParams) (db.RetrievalPlan, error) {
	return db.RetrievalPlan{ID: arg.ID}, nil
}
func (m *memPlanStore) InsertRetrievalPlanStep(context.Context, db.InsertRetrievalPlanStepParams) error {
	return nil
}
func (m *memPlanStore) InsertRetrievalResult(context.Context, db.InsertRetrievalResultParams) error {
	m.results++
	return nil
}

type memTraceStore struct {
	traces []db.CreateAnswerTraceParams
	refs   []db.InsertAnswerTraceAuditRefParams
}

func (m *memTraceStore) CreateAnswerTrace(_ context.Context, arg db.CreateAnswerTraceParams) (db.AnswerTrace, error) {
	m.traces = append(m.traces, arg)
	return db.AnswerTrace{ID: arg.ID, EnterpriseID: arg.EnterpriseID, QuestionHash: arg.QuestionHash, AnswerHash: arg.AnswerHash}, nil
}
func (m *memTraceStore) GetAnswerTrace(context.Context, string) (db.AnswerTrace, error) {
	return db.AnswerTrace{}, nil
}
func (m *memTraceStore) InsertAnswerTraceStep(context.Context, db.InsertAnswerTraceStepParams) error {
	return nil
}
func (m *memTraceStore) InsertAnswerTraceEvidence(context.Context, db.InsertAnswerTraceEvidenceParams) error {
	return nil
}
func (m *memTraceStore) InsertAnswerTraceModelEvent(context.Context, db.InsertAnswerTraceModelEventParams) error {
	return nil
}
func (m *memTraceStore) InsertAnswerTraceAuditRef(_ context.Context, arg db.InsertAnswerTraceAuditRefParams) error {
	m.refs = append(m.refs, arg)
	return nil
}

func testAnswerServer(t *testing.T, mock *nexusclient.Mock) (*httptest.Server, *memTraceStore) {
	t.Helper()
	traceStore := &memTraceStore{}
	deps := &answerDeps{
		evidence:  &fakeFrozenEvidence{},
		nexus:     mock,
		retrieval: retrieval.NewService(&memPlanStore{}, staticSearch{}, nil, nil),
		traces:    trace.NewService(traceStore),
		llm:       echoLLM{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/answer", deps.handleAnswer)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, traceStore
}

func postAnswer(t *testing.T, url, ticket, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/answer", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ticket != "" {
		req.Header.Set("X-Nexus-Ticket", ticket)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func TestAnswerFailsClosedWithoutTicket(t *testing.T) {
	mock := nexusclient.NewMock()
	srv, _ := testAnswerServer(t, mock)

	resp, _ := postAnswer(t, srv.URL, "", `{"enterprise_id":"ent_1","question":"我的工作内容是什么？"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing ticket: status %d", resp.StatusCode)
	}
	resp, _ = postAnswer(t, srv.URL, "tick_bogus", `{"enterprise_id":"ent_1","question":"我的工作内容是什么？"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid ticket: status %d", resp.StatusCode)
	}
}

func TestAnswerCompletesWithEvidenceAndAudit(t *testing.T) {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_ok"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "u_zhang"}
	mock.Locations["ev_1"] = nexus.LocateEvidenceResponse{ResourceURI: "fs://briefs/1.md", SourceSystem: "filesystem"}
	mock.Reads["fs://briefs/1.md"] = nexus.ReadEvidenceResponse{
		GrantID: "grant_1", ContentType: "text/plain",
		SanitizedExcerpt: "今日完成分拣规则联调。", ContentHash: "sha256:x",
	}
	srv, traceStore := testAnswerServer(t, mock)

	resp, out := postAnswer(t, srv.URL, "tick_ok", `{"enterprise_id":"ent_1","question":"我的工作内容是什么？"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if out["status"] != "completed" || out["trace_id"] == "" {
		t.Fatalf("response: %v", out)
	}
	if !strings.Contains(out["answer"].(string), "基于证据的回答") {
		t.Fatalf("answer: %v", out["answer"])
	}
	evidence := out["evidence"].([]any)
	if len(evidence) != 1 {
		t.Fatalf("evidence: %v", evidence)
	}
	if len(traceStore.traces) != 1 || len(traceStore.refs) != 1 {
		t.Fatalf("trace persisted %d, audit refs %d", len(traceStore.traces), len(traceStore.refs))
	}
	if len(mock.AuditLog) != 1 || mock.AuditLog[0].Action != nexus.AuditAnswerTraceCreated {
		t.Fatalf("audit log: %+v", mock.AuditLog)
	}
}

func TestAnswerEnterpriseMismatchForbidden(t *testing.T) {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_b"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_B"}
	srv, _ := testAnswerServer(t, mock)
	resp, _ := postAnswer(t, srv.URL, "tick_b", `{"enterprise_id":"ent_A","question":"跨租户提问"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-tenant must be 403, got %d", resp.StatusCode)
	}
}

// capturingLLM records the exact prompt the answer path builds, so a test can
// prove which retrieved documents grounded the answer.
type capturingLLM struct{ prompt string }

func (c *capturingLLM) Name() string { return "test/capture" }
func (c *capturingLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	for _, ct := range req.Contents {
		for _, p := range ct.Parts {
			c.prompt += p.Text
		}
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{genai.NewPartFromText("已依据授权证据作答")}},
			ModelVersion: "test/capture",
			TurnComplete: true,
		}, nil)
	}
}

// mixedAuthoritySearch returns one governed method_outline hit and one ungrounded
// legacy dream_summary hit in the same index.
type mixedAuthoritySearch struct{}

func (mixedAuthoritySearch) EnsureIndex(context.Context, string) error { return nil }
func (mixedAuthoritySearch) Index(context.Context, string, string, any) error {
	return nil
}
func (mixedAuthoritySearch) Search(context.Context, string, any) (retrieval.SearchResult, error) {
	method, _ := json.Marshal(retrieval.IndexDocument{
		EnterpriseID: "ent_1", SpaceID: "spc_kb", SourceType: "method_outline",
		SummaryText: "治理方法：两步校验流程", SanitizedSnippet: "治理方法：两步校验流程",
	})
	dream, _ := json.Marshal(retrieval.IndexDocument{
		EnterpriseID: "ent_1", SpaceID: "spc_kb", SourceType: "dream_summary",
		SummaryText: "梦境叙述摘要：本周整体顺利无异常", SanitizedSnippet: "梦境叙述摘要：本周整体顺利无异常",
	})
	return retrieval.SearchResult{Total: 2, Hits: []retrieval.Hit{
		{ID: "m1", Score: 2, Source: method},
		{ID: "d1", Score: 1, Source: dream},
	}}, nil
}

// TestAnswerExcludesNonAuthoritativeDreamSummary proves the live answer/citation
// path (Task 18A Part A) grounds the answer ONLY on governed, authoritative
// documents and never presents the legacy ungrounded dream_summary digest as
// governed knowledge.
func TestAnswerExcludesNonAuthoritativeDreamSummary(t *testing.T) {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_ok"] = nexus.VerifyTicketResponse{Valid: true, EnterpriseID: "ent_1", ActorUserID: "u"}
	capLLM := &capturingLLM{}
	deps := &answerDeps{
		evidence:  &fakeFrozenEvidence{},
		nexus:     mock,
		retrieval: retrieval.NewService(&memPlanStore{}, mixedAuthoritySearch{}, nil, nil),
		traces:    trace.NewService(&memTraceStore{}),
		llm:       capLLM,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/answer", deps.handleAnswer)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, out := postAnswer(t, srv.URL, "tick_ok", `{"enterprise_id":"ent_1","question":"本周工作如何"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if !strings.Contains(capLLM.prompt, "治理方法：两步校验流程") {
		t.Fatalf("the governed method_outline snippet must ground the answer:\n%s", capLLM.prompt)
	}
	if strings.Contains(capLLM.prompt, "梦境叙述摘要") {
		t.Fatalf("a non-authoritative dream_summary must NEVER be cited as governed knowledge:\n%s", capLLM.prompt)
	}
}
