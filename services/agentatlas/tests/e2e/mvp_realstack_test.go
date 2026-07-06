// Real-stack MVP e2e (Goal Z2): the DEPLOYED four binaries + real datastores +
// real docling + real llmrouter run the upgraded scenario end to end. The test
// hosts the mock AgentNexus over real HTTP (the containers' NEXUS_BASE_URL
// points at host.docker.internal:8100).
//
//	cd deploy/compose && LLMROUTER_BASE_URL=... LLMROUTER_API_KEY=... \
//	  ATLAS_ORG_SYNC_ENTERPRISES=ent_realstack docker compose up -d
//	ATLAS_E2E_REALSTACK=1 ATLAS_TEST_POSTGRES_DSN=... \
//	  go test ./tests/e2e -run TestAgentAtlasMVPRealStack -v
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

const realstackEnterprise = "ent_realstack"

func TestAgentAtlasMVPRealStack(t *testing.T) {
	if os.Getenv("ATLAS_E2E_REALSTACK") != "1" {
		t.Skip("set ATLAS_E2E_REALSTACK=1 with the full compose stack up")
	}
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (assertions read the stack's database)")
	}
	apiURL := envOr("ATLAS_API_URL", "http://localhost:8080")
	agentURL := envOr("ATLAS_AGENT_URL", "http://localhost:8081")
	nexusAddr := envOr("ATLAS_MOCK_NEXUS_ADDR", "0.0.0.0:8100")
	ctx := context.Background()

	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)

	// ---- mock AgentNexus over real HTTP for the whole stack ----
	backing := nexusclient.NewMock()
	backing.Tickets["tick_rs_employee"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: realstackEnterprise, ActorUserID: "u_zhang",
		Scopes: []string{"space.read"}, ExpiresAt: time.Now().Add(2 * time.Hour),
	}
	backing.Tickets["tick_rs_admin"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: realstackEnterprise, ActorUserID: "admin",
		Scopes: []string{"admin"}, ExpiresAt: time.Now().Add(2 * time.Hour),
	}
	events := loadOrgEvents(t)
	for i := range events {
		events[i].EnterpriseID = realstackEnterprise
	}
	startMockNexusServer(t, nexusAddr, backing, events)

	call := func(base, method, path, ticket string, body io.Reader, contentType string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(method, base+path, body)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
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
	postJSONCall := func(base, path, ticket string, payload any) (*http.Response, map[string]any) {
		raw, _ := json.Marshal(payload)
		return call(base, http.MethodPost, path, ticket, bytes.NewReader(raw), "application/json")
	}

	// ---- 1: worker's org-sync subscription creates the six spaces ----
	deadline := time.Now().Add(90 * time.Second)
	var spaceCount int
	for time.Now().Before(deadline) {
		rows, err := q.ListKnowledgeSpacesByEnterprise(ctx, realstackEnterprise)
		if err == nil {
			spaceCount = len(rows)
		}
		if spaceCount >= 6 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if spaceCount < 6 {
		t.Fatalf("org sync did not create spaces (got %d) — is atlas-worker up with ATLAS_ORG_SYNC_ENTERPRISES=%s?", spaceCount, realstackEnterprise)
	}
	t.Logf("UPGRADE(org sync via deployed worker): %d spaces", spaceCount)

	// ---- 2: work-brief ingestion through the deployed atlas-api ----
	briefs := loadBriefs(t)
	for _, b := range briefs {
		resp, out := postJSONCall(apiURL, "/v1/work-briefs", "tick_rs_employee", map[string]any{
			"enterprise_id": realstackEnterprise, "employee_user_id": b.EmployeeUserID,
			"brief_date": b.BriefDate, "summary": b.Summary, "topics": b.Topics,
			"source_hash": "sha256:rs:" + b.ResourceRef,
			"evidence_pointer": map[string]any{
				"resource_type": "work_brief", "resource_ref": b.ResourceRef, "source_system": "filesystem",
			},
		})
		// 201 first run; 200 duplicate on reruns — both healthy.
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("brief ingest: %d %v", resp.StatusCode, out)
		}
	}

	// ---- 3: artifact upload -> worker parses via REAL docling ----
	pdf, err := os.ReadFile(filepath.Join("..", "fixtures", "documents", "mes_work_order.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	var mp bytes.Buffer
	w := multipart.NewWriter(&mp)
	_ = w.WriteField("content_type", "application/pdf")
	_ = w.WriteField("parser_hint", "docling")
	part, _ := w.CreateFormFile("file", "mes_work_order.pdf")
	_, _ = part.Write(pdf)
	_ = w.Close()
	resp, out := call(apiURL, http.MethodPost, "/v1/artifacts/jobs", "tick_rs_admin", &mp, w.FormDataContentType())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("artifact job: %d %v", resp.StatusCode, out)
	}
	jobID, _ := out["job_id"].(string)
	jobDeadline := time.Now().Add(3 * time.Minute)
	var jobStatus string
	for time.Now().Before(jobDeadline) {
		row, err := q.GetArtifactJob(ctx, jobID)
		if err == nil {
			jobStatus = row.Status
		}
		if jobStatus == "succeeded" || jobStatus == "failed" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if jobStatus != "succeeded" {
		t.Fatalf("UPGRADE 3 failed: artifact job %s = %q (worker + real docling)", jobID, jobStatus)
	}
	t.Log("UPGRADE 3: real docling parsed the PDF through the deployed worker")

	// ---- 4: REAL agent run with a tool call through deployed atlas-agent ----
	resp, out = postJSONCall(agentURL, "/v1/agent/runs", "tick_rs_admin", map[string]any{
		"message": "请把下面三步整理成一个 SOP 工作流草稿（调用 draft_workflow，workflow_id 用 wf_rs_demo，kind 用 sop）：1) 接收 MES 异常工单；2) 排查异常原因；3) 管理员确认后归档。",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("agent run: %d %v", resp.StatusCode, out)
	}
	toolCalls, _ := out["tool_calls"].([]any)
	if len(toolCalls) == 0 {
		t.Fatalf("UPGRADE 2 failed: deployed agent run recorded no tool calls: %v", out)
	}
	t.Logf("UPGRADE 2: deployed ADK agent recorded %d tool call(s)", len(toolCalls))

	// ---- 5: workflow create -> publish -> run through the worker ----
	resp, out = postJSONCall(agentURL, "/v1/workflows", "tick_rs_admin", map[string]any{
		"name": "realstack 确认链",
		"definition": map[string]any{
			"workflow_id": "", "version": 0, "kind": "sop", "risk_level": "low",
			"nodes": []any{
				map[string]any{"id": "in", "type": "input.manual"},
				map[string]any{"id": "tr", "type": "trace.append"},
			},
			"edges": []any{map[string]any{"from": "in", "to": "tr"}},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("workflow create: %d %v", resp.StatusCode, out)
	}
	wfID, _ := out["workflow_id"].(string)
	resp, out = postJSONCall(agentURL, "/v1/workflows/"+wfID+"/publish", "tick_rs_admin", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workflow publish: %d %v", resp.StatusCode, out)
	}
	resp, out = postJSONCall(agentURL, "/v1/workflows/"+wfID+"/runs", "tick_rs_admin", map[string]any{
		"version": 1,
		"input":   map[string]any{"ticket_id": "tick_rs_admin", "question": "realstack 工作流", "answer": "已执行"},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("workflow run: %d %v", resp.StatusCode, out)
	}
	runID, _ := out["run_id"].(string)
	runDeadline := time.Now().Add(2 * time.Minute)
	var runStatus string
	for time.Now().Before(runDeadline) {
		row, err := q.GetWorkflowRun(ctx, runID)
		if err == nil {
			runStatus = row.Status
		}
		if runStatus == workflow.RunSucceeded || runStatus == workflow.RunFailed {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if runStatus != workflow.RunSucceeded {
		t.Fatalf("UPGRADE 7 failed: workflow run %s = %q through the deployed worker", runID, runStatus)
	}
	t.Log("UPGRADE 7: published workflow ran to succeeded through the deployed worker")

	// ---- 6: the employee asks through the deployed answer path ----
	resp, out = postJSONCall(apiURL, "/v1/answer", "tick_rs_employee", map[string]any{
		"enterprise_id": realstackEnterprise, "question": "我的工作内容是什么？",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer: %d %v", resp.StatusCode, out)
	}
	answer, _ := out["answer"].(string)
	traceID, _ := out["trace_id"].(string)
	if strings.TrimSpace(answer) == "" || traceID == "" {
		t.Fatalf("UPGRADE 1 failed: empty real-LLM answer or trace: %v", out)
	}
	t.Logf("UPGRADE 1: real llmrouter answer: %s", answer)

	resp, tr := call(apiURL, http.MethodGet, "/v1/traces/"+traceID, "tick_rs_employee", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trace: %d", resp.StatusCode)
	}
	for _, key := range []string{"question_hash", "answer_hash", "retrieval_plan_id", "model_route"} {
		if v, ok := tr[key].(string); !ok || v == "" {
			t.Fatalf("trace missing %s: %v", key, tr)
		}
	}

	// audit chain (mock server) received events from the deployed stack
	if len(backing.AuditLog) == 0 {
		t.Fatal("UPGRADE 10 failed: deployed stack appended no audit evidence to the mock AgentNexus")
	}
	t.Logf("UPGRADE 10: %d audit events appended over HTTP", len(backing.AuditLog))

	// ---- 7: observability — the deployed metrics endpoints expose series ----
	for _, target := range []string{apiURL + "/metrics", envOr("ATLAS_WORKER_METRICS_URL", "http://localhost:9091") + "/metrics"} {
		mResp, err := http.Get(target)
		if err != nil {
			t.Fatalf("metrics %s: %v", target, err)
		}
		body, _ := io.ReadAll(mResp.Body)
		mResp.Body.Close()
		if !bytes.Contains(body, []byte("agentatlas_")) {
			t.Fatalf("UPGRADE 8: no agentatlas_ series at %s", target)
		}
	}
	t.Log("UPGRADE 8: deployed metrics endpoints expose agentatlas_ series")
	fmt.Println("REALSTACK OK")
}
