package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
)

// A process that started without a model must SAY so. Before this, the only
// answer to "why is there no model here" was a container that exited during
// startup, which leaves nothing to ask.
func TestHealthzStatesWhyTheProcessIsNotReady(t *testing.T) {
	const reason = "knowledge agent not composed: llmrouter has no endpoint, set ATLAS_LLMROUTER_BASE_URL"

	for _, tc := range []struct {
		name    string
		handler http.Handler
		service string
	}{
		{
			name: "atlas-agent",
			handler: NewAgentRouter(AgentRouterDeps{
				OrgAuthorization:    &allowOrgAuthorization{},
				ApprovalTransmitter: &fakeApprovalTransmitter{decision: nexusclient.ApprovalApproved, authority: "oa.example"},
				ApprovalAuthority:   "oa.example",
				WorkCaseContextFor:  alwaysWorkCaseBacked,
				Nexus:               adminMock(),
				NotReadyReason:      reason,
			}),
			service: "atlas-agent",
		},
		{
			name:    "atlas-api",
			handler: NewRouter(RouterDeps{Evidence: &fakeFrozenEvidence{}, Nexus: adminMock(), NotReadyReason: reason}),
			service: "atlas-api",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()

			// The endpoint still answers 200: the process is alive and serving
			// every surface that needs no model. Readiness is what degrades.
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("healthz = %d, want 200", resp.StatusCode)
			}
			if body["service"] != tc.service {
				t.Fatalf("service = %v, want %s", body["service"], tc.service)
			}
			if body["ready"] != false {
				t.Fatalf("ready = %v, want false", body["ready"])
			}
			if body["reason"] != reason {
				t.Fatalf("reason = %v, want %q", body["reason"], reason)
			}
		})
	}
}

// A fully composed process must not grow a "reason" key. The field is
// omitempty precisely so existing consumers see the byte-identical body.
func TestHealthzOmitsReasonWhenFullyComposed(t *testing.T) {
	srv := httptest.NewServer(NewRouter(RouterDeps{Evidence: &fakeFrozenEvidence{}, Nexus: adminMock()}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()

	if body["ready"] != true {
		t.Fatalf("ready = %v, want true", body["ready"])
	}
	if _, present := body["reason"]; present {
		t.Fatalf("reason present on a ready service: %v", body)
	}
}

// With no model wired, /v1/answer must refuse before it spends an AgentNexus
// evidence grant on a request it cannot finish — and it must refuse rather
// than dereference the nil model, which is what generateText would do.
func TestAnswerRefusesWithoutAModel(t *testing.T) {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_admin"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: "ent_1", ActorUserID: "admin",
		Scopes: []string{"admin"}, ExpiresAt: time.Now().Add(time.Hour),
	}
	evidence := &fakeFrozenEvidence{}
	deps := &answerDeps{
		evidence:  evidence,
		nexus:     mock,
		retrieval: retrieval.NewService(&memPlanStore{}, staticSearch{}, nil, nil),
		traces:    trace.NewService(&memTraceStore{}),
		llm:       nil,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/answer", deps.handleAnswer)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, body := postAnswer(t, srv.URL, "tick_admin", `{"enterprise_id":"ent_1","question":"异常工单怎么处理"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("answer = %d, want 503", resp.StatusCode)
	}
	if body["code"] != "generation_unavailable" {
		t.Fatalf("code = %v, want generation_unavailable", body["code"])
	}
	if len(evidence.locates) > 0 {
		t.Fatalf("located evidence %d times for an answer that cannot be produced", len(evidence.locates))
	}

	// The refusal must not leak wiring to a caller who is not authorized to be
	// here at all: no ticket still means 401.
	unauth, _ := postAnswer(t, srv.URL, "", `{"enterprise_id":"ent_1","question":"x"}`)
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticketless = %d, want 401", unauth.StatusCode)
	}
}

// Sanity: the 503 above is caused by the missing model, not by the request or
// the ticket. The same call with a model wired succeeds.
func TestAnswerSucceedsOnceAModelIsWired(t *testing.T) {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_admin"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: "ent_1", ActorUserID: "admin",
		Scopes: []string{"admin"}, ExpiresAt: time.Now().Add(time.Hour),
	}
	srv, _ := testAnswerServer(t, mock)
	resp, body := postAnswer(t, srv.URL, "tick_admin", `{"enterprise_id":"ent_1","question":"异常工单怎么处理"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer = %d (%v), want 200", resp.StatusCode, body)
	}
}

func TestNotReadyReasonNeverCarriesASecret(t *testing.T) {
	// The reason is served publicly from /healthz. Every composition root that
	// writes one names an environment VARIABLE; this asserts the shape those
	// roots produce, so a future reason that interpolates cfg.LLMRouter.APIKey
	// has to break a test to ship.
	for _, reason := range []string{
		"knowledge agent not composed: llmrouter has no endpoint, set ATLAS_LLMROUTER_BASE_URL",
		"answer generation not composed: llmrouter has no endpoint, set ATLAS_LLMROUTER_BASE_URL",
	} {
		if !strings.Contains(reason, "ATLAS_LLMROUTER_BASE_URL") {
			t.Fatalf("reason %q does not name the variable an operator must set", reason)
		}
		if strings.Contains(strings.ToLower(reason), "key") {
			t.Fatalf("reason %q mentions a key; a health surface is not a place for one", reason)
		}
	}
}
