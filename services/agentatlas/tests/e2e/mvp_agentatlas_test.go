// End-to-end MVP scenario against the production-standard stack:
// real PostgreSQL + OpenSearch(smartcn) + MinIO from deploy/compose, mock
// AgentNexus (proposal contract), httptest docling stand-in, deterministic
// in-process LLM.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres opensearch minio minio-init
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable \
//	ATLAS_TEST_OPENSEARCH=http://localhost:9200 \
//	ATLAS_TEST_OBJECT_ENDPOINT=http://localhost:9000 \
//	  go test ./tests/e2e -run TestAgentAtlasMVP -v
package e2e

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	agenttools "github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/spaces"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

// enterpriseID is unique per run so the e2e is re-runnable against a
// persistent database (fixture source hashes dedup per enterprise).
var enterpriseID = newID("ent_e2e")

func newID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// --- deterministic embedder (1024-dim, char-hash based) ---------------------

type embedder struct{}

func (embedder) Dimension() int { return retrieval.EmbeddingDimension }
func (embedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, retrieval.EmbeddingDimension)
		for _, r := range t {
			h := fnv.New32a()
			_, _ = h.Write([]byte(string(r)))
			vec[h.Sum32()%retrieval.EmbeddingDimension]++
		}
		var sum float64
		for _, x := range vec {
			sum += float64(x) * float64(x)
		}
		if sum == 0 {
			vec[0] = 1
		} else {
			z := sum
			for j := 0; j < 20; j++ {
				z = (z + sum/z) / 2
			}
			inv := float32(1 / z)
			for j := range vec {
				vec[j] *= inv
			}
		}
		out[i] = vec
	}
	return out, nil
}

// --- deterministic LLM -------------------------------------------------------

type e2eLLM struct{}

func (e2eLLM) Name() string { return "e2e/deterministic" }
func (e2eLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var prompt string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				prompt += p.Text
			}
		}
		answer := "你当前的主要工作是 MES 异常工单专项：已完成分拣规则联调与版本复核，遗留风险为接口限流方案未定。"
		if !strings.Contains(prompt, "授权证据片段") {
			answer = "根据检索摘要：你近期在推进 MES 工单相关工作。"
		}
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: "model", Parts: []*genai.Part{genai.NewPartFromText(answer)}},
			ModelVersion: "e2e/deterministic",
			TurnComplete: true,
		}, nil)
	}
}

func TestAgentAtlasMVP(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	osURL := os.Getenv("ATLAS_TEST_OPENSEARCH")
	objURL := os.Getenv("ATLAS_TEST_OBJECT_ENDPOINT")
	if dsn == "" || osURL == "" || objURL == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN, ATLAS_TEST_OPENSEARCH, ATLAS_TEST_OBJECT_ENDPOINT")
	}
	ctx := context.Background()

	// ---- infrastructure ----
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)

	objects, err := storage.NewObjectStore(config.ObjectStorage{
		Endpoint: objURL, Bucket: "agentatlas",
		AccessKey: envOr("ATLAS_TEST_OBJECT_ACCESS_KEY", "atlas"),
		SecretKey: envOr("ATLAS_TEST_OBJECT_SECRET_KEY", "atlas-secret-key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}

	search, err := retrieval.NewHTTPSearchClient([]string{osURL}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	search.RefreshOnWrite = true
	emb := embedder{}

	runner := tasks.NewRunner(tasks.NewMemBus())
	indexer := retrieval.NewIndexer(q, search, emb)
	if err := indexer.RegisterJobHandler(runner); err != nil {
		t.Fatal(err)
	}

	// Document parsing: the REAL docling sidecar when ATLAS_PARSER_SIDECARS=1
	// (compose docling-sidecar up); the in-process stand-in stays only as the
	// no-sidecar CI fallback.
	doclingURL := ""
	if os.Getenv("ATLAS_PARSER_SIDECARS") == "1" {
		doclingURL = envOr("ATLAS_TEST_DOCLING_URL", "http://localhost:5001")
		t.Logf("parsing through REAL docling sidecar at %s", doclingURL)
	} else {
		// Mirrors the REAL docling-serve v1 contract (POST /v1/convert/source,
		// sources[] with kind=file) so the stand-in cannot drift from prod.
		docling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/convert/source" {
				http.NotFound(w, r)
				return
			}
			var req struct {
				Sources []struct {
					Base64String string `json:"base64_string"`
				} `json:"sources"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			md := ""
			if len(req.Sources) > 0 {
				raw, _ := decodeB64(req.Sources[0].Base64String)
				md = string(raw)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":   "success",
				"document": map[string]any{"md_content": md},
			})
		}))
		defer docling.Close()
		doclingURL = docling.URL
	}
	registry, err := parsergateway.NewRegistry(parsergateway.NewDoclingProvider(doclingURL))
	if err != nil {
		t.Fatal(err)
	}
	artifactSvc := artifacts.NewService(q, objects, parsergateway.NewGateway(registry), runner, nil)
	if err := artifactSvc.RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}

	// mock AgentNexus with tickets + authorized evidence from fixtures
	mock := nexusclient.NewMock()
	mock.Tickets["tick_employee"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: enterpriseID, ActorUserID: "u_zhang",
		Scopes: []string{"space.read"}, ExpiresAt: time.Now().Add(time.Hour),
	}
	mock.Tickets["tick_admin"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: enterpriseID, ActorUserID: "admin",
		Scopes: []string{"admin"}, ExpiresAt: time.Now().Add(time.Hour),
	}
	authorized := loadAuthorizedEvidence(t)
	for uri, grant := range authorized {
		mock.Reads[uri] = nexus.ReadEvidenceResponse{
			GrantID: grant.GrantID, ContentType: grant.ContentType,
			SanitizedExcerpt: grant.SanitizedExcerpt, ContentHash: "sha256:" + grant.GrantID,
		}
	}

	// ---- step 1-2: org events -> five knowledge spaces ----
	// Fixture events carry a fixed enterprise id; rebind them to this run's
	// unique enterprise so the e2e stays re-runnable against a persistent DB.
	events := loadOrgEvents(t)
	for i := range events {
		events[i].EnterpriseID = enterpriseID
	}
	spaceSvc := spaces.NewService(q)
	syncer := spaces.NewSyncer(spaceSvc, q, mock, zap.NewNop())
	for _, ev := range events {
		if err := syncer.Handle(ctx, ev); err != nil {
			t.Fatalf("org event %s: %v", ev.EventID, err)
		}
	}
	allSpaces, err := spaceSvc.ListByEnterprise(ctx, enterpriseID)
	if err != nil || len(allSpaces) != 6 {
		t.Fatalf("spaces = %d (want 6: company, BU, dept, 2 employees, project group), err=%v", len(allSpaces), err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// ---- runtime API surface ----
	router := app.NewRouter(app.RouterDeps{
		Nexus:     mock,
		Retrieval: retrieval.NewService(q, search, emb, nil),
		Traces:    trace.NewService(q),
		LLM:       e2eLLM{},
		Store:     q,
		Runner:    runner,
	})
	srv := httptest.NewServer(router)
	defer srv.Close()
	call := func(method, path, ticket, body string) (*http.Response, map[string]any) {
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

	// ---- step 3: work briefs through the ingestion API ----
	briefs := loadBriefs(t)
	for _, b := range briefs {
		payload, _ := json.Marshal(map[string]any{
			"enterprise_id": enterpriseID, "employee_user_id": b.EmployeeUserID,
			"brief_date": b.BriefDate, "summary": b.Summary, "topics": b.Topics,
			"source_hash": "sha256:" + b.ResourceRef,
			"evidence_pointer": map[string]any{
				"resource_type": "work_brief", "resource_ref": b.ResourceRef,
				"source_system": "filesystem",
			},
		})
		resp, out := call(http.MethodPost, "/v1/work-briefs", "tick_employee", string(payload))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("brief ingest %s: %d %v", b.ResourceRef, resp.StatusCode, out)
		}
		// authorize locate on the runtime-generated pointer
		nodeID := out["timeline_node_id"].(string)
		node, err := q.GetTimelineNode(ctx, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		mock.Locations[node.EvidencePointerID.String] = nexus.LocateEvidenceResponse{
			ResourceURI: b.ResourceRef, SourceSystem: "filesystem",
		}
	}

	// ---- step 4-7: SOP document -> workflow draft -> immutable publish ----
	sopMD, err := os.ReadFile(filepath.Join("..", "fixtures", "documents", "mes_work_order_sop.md"))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := artifactSvc.IngestUpload(ctx, enterpriseID, "mes_work_order_sop.md", "text/markdown", sopMD)
	if err != nil {
		t.Fatal(err)
	}
	job, err := artifactSvc.CreateJob(ctx, enterpriseID, artifact.ID, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := q.GetArtifactJob(ctx, job.ID); row.Status != "succeeded" {
		t.Fatalf("artifact job: %+v", row)
	}
	docRow, err := q.GetAtlasDocumentByArtifact(ctx, artifact.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Knowledge Agent tool builds the draft from parsed SOP structure
	draft, err := agenttools.DraftWorkflow(agenttools.DraftWorkflowArgs{
		WorkflowID: newID("wf"), Kind: "sop", RiskLevel: "medium",
		Steps: []agenttools.DraftWorkflowStep{
			{ID: "in", Type: "input.evidence_pointer", Name: "接收 SOP 文档"},
			{ID: "parse", Type: "parser.document", Name: "解析文档"},
			{ID: "extract", Type: "transform.extract_sop", Name: "抽取 SOP 结构"},
			{ID: "confirm", Type: "human.confirm", Name: "管理员确认", RequiresConfirmation: true},
			{ID: "trace", Type: "trace.append", Name: "写入追溯"},
		},
		Edges: []agenttools.DraftWorkflowEdge{
			{From: "in", To: "parse"}, {From: "parse", To: "extract"},
			{From: "extract", To: "confirm"}, {From: "confirm", To: "trace", Condition: "approved"},
		},
	})
	if err != nil {
		t.Fatalf("agent draft: %v", err)
	}
	wfSvc, err := workflow.NewService(q)
	if err != nil {
		t.Fatal(err)
	}
	wfID, err := wfSvc.CreateDraft(ctx, enterpriseID, "MES 异常工单排查 SOP", "admin", draft.Workflow)
	if err != nil {
		t.Fatalf("workflow draft: %v", err)
	}
	version, err := wfSvc.Publish(ctx, enterpriseID, wfID, "admin")
	if err != nil || version != 1 {
		t.Fatalf("publish: v=%d err=%v", version, err)
	}
	def, err := wfSvc.VersionDefinition(ctx, wfID, 1)
	if err != nil || len(def.Nodes) != 5 || def.Kind != sdkworkflow.KindSOP {
		t.Fatalf("published definition: %+v err=%v", def, err)
	}
	_ = docRow // AtlasDocument existence asserted above; not otherwise used in this scenario

	// ---- step 8-10: dream policy -> scheduled run -> layered summaries ----
	var pgSpace db.KnowledgeSpace
	for _, s := range allSpaces {
		if s.OrgScope == "project_group:pg_mes" {
			pgSpace = s
		}
	}
	if pgSpace.ID == "" {
		t.Fatal("project group space missing")
	}
	dreamWorkflowID := "legacy-direct-dream"
	if _, err := wfSvc.CreateDraft(ctx, enterpriseID, "Published Dream", "admin", workflow.Definition{
		WorkflowID: dreamWorkflowID, Kind: sdkworkflow.KindDream, RiskLevel: sdkworkflow.RiskLow,
		Nodes: []sdkworkflow.Node{{ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}, {ID: "trace", Type: sdkworkflow.NodeTraceAppend}}, Edges: []sdkworkflow.Edge{{From: "aggregate", To: "trace"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := wfSvc.Publish(ctx, enterpriseID, dreamWorkflowID, "admin"); err != nil {
		t.Fatal(err)
	}
	policySvc := dream.NewPolicyService(q)
	fixturePolicies := dream.NewPolicyFixtureService(q)
	policyID, err := fixturePolicies.CreateDraft(ctx, enterpriseID, dream.Policy(sdkdream.DreamPolicyDefinition{
		OrgUnitID: "project_group:pg_mes", Timezone: "UTC", Schedule: "0 22 * * *",
		InputSources:      []sdkdream.Source{sdkdream.SourceWorkBrief},
		Workflow:          sdkdream.WorkflowRef{ID: dreamWorkflowID, Version: 1},
		VisibilityLevel:   sdkdream.VisibilityMembers,
		MaskingRules:      []string{`1[3-9]\d{9}`},
		RiskSignalRules:   []string{`风险[:：]\S+`},
		EvidenceRetention: sdkdream.EvidencePointerPlusDisplaySummary,
		ConfirmationMode:  sdkdream.ConfirmationHighRiskOnly,
		OutputSpaceID:     pgSpace.ID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixturePolicies.Publish(ctx, policyID); err != nil {
		t.Fatal(err)
	}
	synth := dream.NewSynthesizer(nil)
	dreamWorkflowRuntime := workflow.NewRuntime(q, wfSvc, workflow.NewRegistryWithServices(workflow.Executors{Dream: synth.AggregateWorkflowInput, Traces: trace.NewService(q)}))
	dreamRunner := dream.NewRunner(q, objects, policySvc, runner, dream.NewOrchestrator(dreamWorkflowRuntime))
	if err := dreamRunner.RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	// restart runner subscriptions to include the dream handler
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler := dream.NewScheduler(q, policySvc, runner)
	if n, err := scheduler.Tick(ctx, enterpriseID, time.Date(2026, 7, 6, 22, 30, 0, 0, time.UTC)); err != nil || n != 1 {
		t.Fatalf("dream tick: n=%d err=%v", n, err)
	}
	displaySums, err := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
		SpaceID: pgSpace.ID, Layer: "display", Limit: 5,
	})
	if err != nil || len(displaySums) != 1 {
		t.Fatalf("dream display: %v n=%d", err, len(displaySums))
	}
	if !strings.Contains(displaySums[0].SummaryText, "风险信号") {
		t.Fatalf("dream display text: %s", displaySums[0].SummaryText)
	}

	// ---- step 11-15: the employee asks; full traceable answer ----
	resp, out := call(http.MethodPost, "/v1/answer", "", `{"enterprise_id":"`+enterpriseID+`","question":"我的工作内容是什么？"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticketless answer must fail closed, got %d", resp.StatusCode)
	}
	resp, out = call(http.MethodPost, "/v1/answer", "tick_employee",
		`{"enterprise_id":"`+enterpriseID+`","question":"我的工作内容是什么？"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer: %d %v", resp.StatusCode, out)
	}
	answer := out["answer"].(string)
	if !strings.Contains(answer, "MES 异常工单专项") {
		t.Fatalf("answer content: %s", answer)
	}
	traceID := out["trace_id"].(string)
	evidenceList := out["evidence"].([]any)
	if len(evidenceList) == 0 {
		t.Fatal("answer returned no evidence")
	}

	resp, tr := call(http.MethodGet, "/v1/traces/"+traceID, "tick_employee", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trace: %d", resp.StatusCode)
	}
	for _, key := range []string{"question_hash", "answer_hash", "retrieval_plan_id", "model_route"} {
		if v, ok := tr[key].(string); !ok || v == "" {
			t.Fatalf("trace missing %s: %v", key, tr)
		}
	}
	if len(tr["space_ids"].([]any)) == 0 || len(tr["evidence_pointer_ids"].([]any)) == 0 ||
		len(tr["agentnexus_read_grant_ids"].([]any)) == 0 {
		t.Fatalf("trace provenance incomplete: %v", tr)
	}

	// audit chain received the answer event
	var audited bool
	for _, entry := range mock.AuditLog {
		if entry.Action == nexus.AuditAnswerTraceCreated && entry.TraceID == traceID {
			audited = true
		}
	}
	if !audited {
		t.Fatal("audit chain missing the answer trace event")
	}

	t.Logf("MVP answer: %s", answer)
	t.Logf("trace: %s grants=%v", traceID, tr["agentnexus_read_grant_ids"])
}

// --- fixture loaders ---------------------------------------------------------

type briefFixture struct {
	EmployeeUserID string   `json:"employee_user_id"`
	BriefDate      string   `json:"brief_date"`
	Summary        string   `json:"summary"`
	Topics         []string `json:"topics"`
	ResourceRef    string   `json:"resource_ref"`
}

func loadBriefs(t *testing.T) []briefFixture {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "fixtures", "dream", "work_briefs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []briefFixture
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var b briefFixture
		if err := json.Unmarshal(sc.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func loadOrgEvents(t *testing.T) []nexus.OrgEvent {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "fixtures", "org", "agentnexus_org_events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []nexus.OrgEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var ev nexus.OrgEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

type grantFixture struct {
	GrantID          string `json:"grant_id"`
	ContentType      string `json:"content_type"`
	SanitizedExcerpt string `json:"sanitized_excerpt"`
}

func loadAuthorizedEvidence(t *testing.T) map[string]grantFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "fixtures", "nexus", "authorized_evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]grantFixture{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
