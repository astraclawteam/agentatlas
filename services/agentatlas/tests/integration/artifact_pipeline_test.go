// Artifact pipeline integration test: real PostgreSQL + real MinIO from
// deploy/compose; the docling sidecar is an httptest server (per the plan,
// sidecar HTTP endpoints are mocked at this stage).
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres minio minio-init
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable \
//	ATLAS_TEST_OBJECT_ENDPOINT=http://localhost:9000 \
//	  go test ./tests/integration -run TestArtifactPipeline
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

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// pipelineSummaryLLM: deterministic in-process model for the summarizer —
// section calls echo a grounded digest, the rollup call returns grounded JSON.
type pipelineSummaryLLM struct{}

func (pipelineSummaryLLM) Name() string { return "test/pipeline-summary" }

func (pipelineSummaryLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	system := ""
	if req.Config != nil && req.Config.SystemInstruction != nil {
		for _, p := range req.Config.SystemInstruction.Parts {
			system += p.Text
		}
	}
	reply := "节摘要：MES 工单排查步骤与风险点。"
	if strings.Contains(system, "JSON") {
		reply = `{"display":"MES 异常工单排查文档：确认工单编号，注意接口限流风险。","retrieval":"MES 异常工单排查：第一步确认工单编号；风险点为接口限流未定。"}`
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      genai.NewContentFromText(reply, "model"),
			TurnComplete: true,
		}, nil)
	}
}

func TestArtifactPipeline(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	objEndpoint := os.Getenv("ATLAS_TEST_OBJECT_ENDPOINT")
	if dsn == "" || objEndpoint == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN and ATLAS_TEST_OBJECT_ENDPOINT (compose postgres+minio)")
	}
	ctx := context.Background()

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	q := db.New(pool)

	objects, err := storage.NewObjectStore(config.ObjectStorage{
		Endpoint: objEndpoint, Bucket: "agentatlas",
		AccessKey: envOr("ATLAS_TEST_OBJECT_ACCESS_KEY", "atlas"),
		SecretKey: envOr("ATLAS_TEST_OBJECT_SECRET_KEY", "atlas-secret-key"),
		Region:    "us-east-1",
	})
	if err != nil {
		t.Fatalf("object store: %v", err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	// docling sidecar stand-in
	docling := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1alpha/convert/source" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"document": map[string]any{
				"md_content": "# MES 异常工单排查\n\n第一步：确认工单编号。\n\n## 风险点\n\n接口限流未定。",
			},
		})
	}))
	defer docling.Close()

	registry, err := parsergateway.NewRegistry(parsergateway.NewDoclingProvider(docling.URL))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	runner := tasks.NewRunner(tasks.NewMemBus())
	svc := artifacts.NewService(q, objects, parsergateway.NewGateway(registry), runner,
		artifacts.NewSummarizer(pipelineSummaryLLM{}))
	if err := svc.RegisterJobHandler(); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("runner start: %v", err)
	}

	entID := newID("ent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: entID, Name: "测试企业"}); err != nil {
		t.Fatalf("enterprise: %v", err)
	}

	artifact, err := svc.IngestUpload(ctx, entID, "mes_sop.pdf", "application/pdf", []byte("%PDF-1.4 fake sop bytes"))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	job, err := svc.CreateJob(ctx, entID, artifact.ID, "auto") // MemBus executes synchronously
	if err != nil {
		t.Fatalf("job: %v", err)
	}

	final, err := q.GetArtifactJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if final.Status != "succeeded" {
		t.Fatalf("job status = %s (error: %s)", final.Status, final.Error)
	}

	// document row exists and the FULL document lives in object storage only
	docRow, err := q.GetAtlasDocumentByArtifact(ctx, artifact.ID)
	if err != nil {
		t.Fatalf("document row: %v", err)
	}
	raw, err := objects.Get(ctx, docRow.ObjectKey)
	if err != nil {
		t.Fatalf("full doc from object storage: %v", err)
	}
	var fullDoc atlasdocument.AtlasDocument
	if err := json.Unmarshal(raw, &fullDoc); err != nil {
		t.Fatalf("decode full doc: %v", err)
	}
	if len(fullDoc.Blocks) < 3 || fullDoc.Provider != "docling" {
		t.Fatalf("full doc: %+v", fullDoc)
	}

	summaries, err := q.ListDocumentSummaries(ctx, docRow.ID)
	if err != nil || len(summaries) < 2 {
		t.Fatalf("summaries: %v (n=%d)", err, len(summaries))
	}
	var display string
	for _, s := range summaries {
		if s.Level == "display" {
			display = s.SummaryText
		}
	}
	if !strings.Contains(display, "MES 异常工单排查") {
		t.Fatalf("display summary = %q", display)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
