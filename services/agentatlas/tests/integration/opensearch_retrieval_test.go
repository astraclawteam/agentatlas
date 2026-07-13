// Retrieval integration test against the real OpenSearch (smartcn image)
// from deploy/compose, plus real PostgreSQL for the indexer path.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres opensearch
//	ATLAS_TEST_POSTGRES_DSN=... ATLAS_TEST_OPENSEARCH=http://localhost:9200 \
//	  go test ./tests/integration -run TestOpenSearchRetrieval
package integration

import (
	"context"
	"hash/fnv"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// deterministicEmbedder maps texts to stable 1024-dim vectors; texts sharing
// a keyword land near each other so nearest-neighbor order is predictable.
type deterministicEmbedder struct{}

func (deterministicEmbedder) Dimension() int { return retrieval.EmbeddingDimension }

func (deterministicEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, retrieval.EmbeddingDimension)
		for _, token := range strings.Fields(tokenize(t)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(token))
			vec[h.Sum32()%retrieval.EmbeddingDimension] += 1
		}
		out[i] = normalize(vec)
	}
	return out, nil
}

func tokenize(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
		b.WriteRune(' ')
	}
	return b.String()
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		v[0] = 1
		return v
	}
	inv := float32(1.0 / sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

func sqrt(x float64) float64 {
	z := x
	for i := 0; i < 20; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func TestOpenSearchRetrieval(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	osURL := os.Getenv("ATLAS_TEST_OPENSEARCH")
	if dsn == "" || osURL == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN and ATLAS_TEST_OPENSEARCH (compose postgres+opensearch)")
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

	client, err := retrieval.NewHTTPSearchClient([]string{osURL}, "", "", nil)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	client.RefreshOnWrite = true
	embedder := deterministicEmbedder{}

	entA, entB := newID("ent"), newID("ent")
	for _, e := range []string{entA, entB} {
		if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: e, Name: e}); err != nil {
			t.Fatalf("enterprise: %v", err)
		}
	}

	// --- indexer path from real PG rows (timeline node) -------------------
	spaceID := newID("spc")
	if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
		ID: spaceID, EnterpriseID: entA, Kind: "employee", Name: "张予安",
		OrgScope: "employee:u1", OrgVersion: 1,
	}); err != nil {
		t.Fatalf("space: %v", err)
	}
	epID := newID("ev")
	if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
		ID: epID, EnterpriseID: entA, ResourceType: "work_brief",
		ResourceRef: "fs://briefs/1.md", SourceSystem: "filesystem",
		RequiredScopes: []string{},
	}); err != nil {
		t.Fatalf("pointer: %v", err)
	}
	nodeID := newID("tl")
	if _, err := q.InsertTimelineNode(ctx, db.InsertTimelineNodeParams{
		ID: nodeID, EnterpriseID: entA, SpaceID: spaceID, OrgScope: "employee:u1",
		NodeTime: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		SourceType: "work_brief", SummaryText: "完成 MES 异常工单分拣规则联调，遗留接口限流风险。",
		Tags: []string{"MES"}, EvidencePointerID: pgtype.Text{String: epID, Valid: true},
	}); err != nil {
		t.Fatalf("timeline: %v", err)
	}
	idxJob, err := q.CreateIndexJob(ctx, db.CreateIndexJobParams{
		ID: newID("idx"), EnterpriseID: entA, SourceType: "timeline_node",
		SourceID: nodeID, Status: "pending",
	})
	if err != nil {
		t.Fatalf("index job: %v", err)
	}
	runner := tasks.NewRunner(tasks.NewMemBus())
	indexer := retrieval.NewIndexer(q, client, embedder)
	if err := indexer.RegisterJobHandler(runner); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := runner.Process(ctx, retrieval.JobTypeIndex, idxJob.ID); err != nil {
		t.Fatalf("index run: %v", err)
	}
	final, _ := q.GetIndexJob(ctx, idxJob.ID)
	if final.Status != "succeeded" {
		t.Fatalf("index job status = %s (%s)", final.Status, final.Error)
	}

	// extra corpus for both enterprises
	seed := func(ent, id, text string) {
		vecs, _ := embedder.Embed(ctx, []string{text})
		index := retrieval.IndexName(ent)
		if err := client.EnsureIndex(ctx, index); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if err := client.Index(ctx, index, id, retrieval.IndexDocument{
			EnterpriseID: ent, SourceType: "document_summary",
			SummaryText: text, SanitizedSnippet: text, Embedding: vecs[0],
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(entA, "doc_sop", "MES 异常工单排查 SOP：第一步确认工单编号。")
	seed(entA, "doc_hr", "入职第一周工作方法：熟悉部门制度。")
	seed(entB, "doc_secret", "B 企业异常工单密档摘要。")

	svc := retrieval.NewService(q, client, embedder, nil)
	query := retrieval.Query{EnterpriseID: entA, Text: "异常工单排查", TopK: 5}
	planID, err := svc.CreatePlan(ctx, query)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	results, err := svc.Execute(ctx, planID, query)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("results = %d", len(results))
	}

	// smartcn keyword + vector fusion must surface both 工单 docs above the HR doc
	top2 := results[0].DocID + "|" + results[1].DocID
	if !strings.Contains(top2, "doc_sop") || !strings.Contains(top2, nodeID) {
		t.Fatalf("hybrid top2 = %s (want doc_sop and timeline node)", top2)
	}
	// tenant isolation: enterprise B's doc must never appear
	for _, r := range results {
		if r.DocID == "doc_secret" {
			t.Fatal("cross-tenant leak: enterprise B document returned for enterprise A")
		}
	}
	// evidence pointers travel with hits
	var foundPointer bool
	for _, r := range results {
		if r.EvidencePointerID == epID {
			foundPointer = true
		}
	}
	if !foundPointer {
		t.Fatal("timeline hit lost its evidence pointer")
	}

	steps, err := q.ListRetrievalPlanSteps(ctx, planID)
	if err != nil || len(steps) < 3 {
		t.Fatalf("plan steps: %v n=%d", err, len(steps))
	}
}
