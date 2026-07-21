// Full answer-path integration test: real PostgreSQL + real OpenSearch,
// mock AgentNexus, deterministic in-process LLM.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres opensearch
//	ATLAS_TEST_POSTGRES_DSN=... ATLAS_TEST_OPENSEARCH=http://localhost:9200 \
//	  go test ./tests/integration -run TestAnswerRuntime
package integration

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
)

type evidenceLLM struct{}

func (evidenceLLM) Name() string { return "test/evidence" }
func (evidenceLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var prompt string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				prompt += p.Text
			}
		}
		answer := "根据简报证据：你当前的主要工作是 MES 异常工单专项。"
		if !strings.Contains(prompt, "授权证据片段") {
			answer = "仅依据检索摘要：你近期在做 MES 工单相关工作。"
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{genai.NewPartFromText(answer)}},
			ModelVersion: "test/evidence",
			TurnComplete: true,
		}, nil)
	}
}

func TestAnswerRuntime(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	osURL := os.Getenv("ATLAS_TEST_OPENSEARCH")
	if dsn == "" || osURL == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN and ATLAS_TEST_OPENSEARCH")
	}
	ctx := context.Background()

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	q := db.New(pool)

	search, err := retrieval.NewHTTPSearchClient([]string{osURL}, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	search.RefreshOnWrite = true
	embedder := deterministicEmbedder{}

	// enterprise + employee space + one brief timeline node indexed
	entID := newID("ent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: entID, Name: "测试企业"}); err != nil {
		t.Fatal(err)
	}
	spaceID := newID("spc")
	if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
		ID: spaceID, EnterpriseID: entID, Kind: "employee", Name: "张予安",
		OrgScope: "employee:u_zhang", OrgVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}

	runner := tasks.NewRunner(tasks.NewMemBus())
	indexer := retrieval.NewIndexer(q, search, embedder)
	if err := indexer.RegisterJobHandler(runner); err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}

	mock := nexusclient.NewMock()
	mock.Tickets["tick_e2e"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: entID, ActorUserID: "u_zhang",
		Scopes: []string{"space.read"}, ExpiresAt: time.Now().Add(time.Hour),
	}

	router := app.NewRouter(app.RouterDeps{
		Nexus:     mock,
		Retrieval: retrieval.NewService(q, search, embedder, nil),
		Traces:    trace.NewService(q),
		LLM:       evidenceLLM{},
		Store:     q,
		Runner:    runner,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	do := func(method, path, ticket, body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if ticket != "" {
			req.Header.Set("X-Nexus-Ticket", ticket)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp, out
	}

	// 1) ingest a work brief through the real API
	briefBody := `{
		"enterprise_id": "` + entID + `",
		"employee_user_id": "u_zhang",
		"brief_date": "2026-07-06",
		"summary": "推进 MES 异常工单专项：完成分拣规则联调，风险：接口限流未定。",
		"topics": ["MES"],
		"source_hash": "` + newID("sha") + `",
		"evidence_pointer": {
			"resource_type": "work_brief",
			"resource_ref": "fs://briefs/u_zhang/2026-07-06.md",
			"source_system": "filesystem"
		}
	}`
	resp, out := do(http.MethodPost, "/v1/work-briefs", "tick_e2e", briefBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ingest status %d: %v", resp.StatusCode, out)
	}
	nodeID := out["timeline_node_id"].(string)

	// no-ticket ingestion fails closed
	resp, _ = do(http.MethodPost, "/v1/work-briefs", "", briefBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-ticket ingest: %d", resp.StatusCode)
	}

	// authorize evidence reads on the brief pointer
	node, err := q.GetTimelineNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	pointerID := node.EvidencePointerID.String
	// Authorized evidence is keyed by the declared NEED; the frozen contract has
	// no resource URI for a consumer to register.
	mock.SetEvidence(pointerID, "今日完成 MES 分拣规则联调，遗留接口限流风险。")

	// 2) answer with full chain
	resp, out = do(http.MethodPost, "/v1/answer", "tick_e2e",
		`{"enterprise_id":"`+entID+`","question":"我的工作内容是什么？"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer status %d: %v", resp.StatusCode, out)
	}
	if out["status"] != "completed" || !strings.Contains(out["answer"].(string), "MES 异常工单专项") {
		t.Fatalf("answer: %v", out)
	}
	traceID := out["trace_id"].(string)

	// 3) trace has the full provenance
	resp, traceOut := do(http.MethodGet, "/v1/traces/"+traceID, "tick_e2e", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trace status %d", resp.StatusCode)
	}
	if !strings.HasPrefix(traceOut["question_hash"].(string), "sha256:") ||
		!strings.HasPrefix(traceOut["answer_hash"].(string), "sha256:") {
		t.Fatalf("trace hashes: %v", traceOut)
	}
	grants := traceOut["agentnexus_read_grant_ids"].([]any)
	if len(grants) != 1 || grants[0] != "grant_e2e" {
		t.Fatalf("grants: %v", grants)
	}
	evidence := traceOut["evidence_pointer_ids"].([]any)
	if len(evidence) != 1 || evidence[0] != pointerID {
		t.Fatalf("evidence: %v", evidence)
	}
	if len(mock.AuditLog) == 0 {
		t.Fatal("audit chain untouched")
	}

	// 4) timeline endpoint returns the brief node
	resp, tl := do(http.MethodGet, "/v1/spaces/"+spaceID+"/timeline", "tick_e2e", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("timeline status %d", resp.StatusCode)
	}
	nodes := tl["nodes"].([]any)
	if len(nodes) == 0 {
		t.Fatal("timeline empty")
	}

	_ = pgtype.Text{} // keep pgtype linked for the build tag-free file
}
