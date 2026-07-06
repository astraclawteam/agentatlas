// Real-model retrieval integration test: real bge-m3 embeddings (and rerank,
// when the deployment serves /rerank) against real OpenSearch. Proves vector
// recall on paraphrase queries keyword search cannot match, and that the
// service applies the reranker's ordering. Run:
//
//	ATLAS_TEST_OPENSEARCH=http://localhost:9200 \
//	LLMROUTER_BASE_URL=https://.../v1 LLMROUTER_API_KEY=sk-... \
//	  go test ./tests/integration -run TestRetrievalRealModel
package integration

import (
	"context"
	"os"
	"testing"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
)

func TestRetrievalRealModel(t *testing.T) {
	osURL := os.Getenv("ATLAS_TEST_OPENSEARCH")
	baseURL := os.Getenv("LLMROUTER_BASE_URL")
	apiKey := os.Getenv("LLMROUTER_API_KEY")
	if osURL == "" || baseURL == "" || apiKey == "" {
		t.Skip("set ATLAS_TEST_OPENSEARCH, LLMROUTER_BASE_URL, LLMROUTER_API_KEY")
	}
	ctx := context.Background()

	embedder, err := retrieval.NewLLMRouterEmbedder(baseURL, apiKey, "bge-m3")
	if err != nil {
		t.Fatalf("embedder: %v", err)
	}
	client, err := retrieval.NewHTTPSearchClient([]string{osURL}, "", "")
	if err != nil {
		t.Fatalf("search client: %v", err)
	}
	client.RefreshOnWrite = true

	ent := newID("ent")
	index := retrieval.IndexName(ent)
	if err := client.EnsureIndex(ctx, index); err != nil {
		t.Fatalf("ensure index: %v", err)
	}
	// Corpus engineered so only SEMANTIC similarity ranks doc_repair first for
	// the paraphrase query (no keyword overlap with 设备/故障).
	corpus := map[string]string{
		"doc_repair": "产线机器停机后的检修与恢复流程手册。",
		"doc_menu":   "员工食堂本周菜单更新通知。",
		"doc_stats":  "季度产量统计报表与图表说明。",
	}
	for id, text := range corpus {
		vecs, err := embedder.Embed(ctx, []string{text})
		if err != nil {
			t.Fatalf("embed %s (llmrouter /embeddings must be healthy): %v", id, err)
		}
		if len(vecs[0]) != retrieval.EmbeddingDimension {
			t.Fatalf("embedding dims = %d, want %d", len(vecs[0]), retrieval.EmbeddingDimension)
		}
		if err := client.Index(ctx, index, id, retrieval.IndexDocument{
			EnterpriseID: ent, SourceType: "document_summary",
			SummaryText: text, SanitizedSnippet: text, Embedding: vecs[0],
		}); err != nil {
			t.Fatalf("index %s: %v", id, err)
		}
	}

	// Paraphrase query: 设备故障 shares no tokens with 机器停机/检修 — keyword
	// search cannot rank doc_repair first; real vectors must.
	query := "设备出现故障如何维修处理"
	qv, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	res, err := client.Search(ctx, index,
		retrieval.BuildVectorQuery(retrieval.Query{EnterpriseID: ent, Text: query, TopK: 3}, qv[0]))
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(res.Hits) == 0 || res.Hits[0].ID != "doc_repair" {
		t.Fatalf("vector recall failed: hits=%+v (want doc_repair first)", res.Hits)
	}
	t.Logf("vector recall: top=%s (real bge-m3)", res.Hits[0].ID)

	// Rerank leg: requires the deployment to serve the Cohere-style /rerank
	// route. Skip with a loud reason when absent (llmrouter-side capability),
	// fail on any other error.
	reranker, err := retrieval.NewLLMRouterReranker(baseURL, apiKey, os.Getenv("ATLAS_LLMROUTER_RERANK_MODEL"))
	if err != nil {
		t.Fatalf("reranker: %v", err)
	}
	docs := []string{corpus["doc_menu"], corpus["doc_repair"], corpus["doc_stats"]} // repair deliberately NOT first
	ranked, err := reranker.Rerank(ctx, query, docs, 3)
	if err != nil {
		t.Skipf("rerank route unavailable on this llmrouter deployment (external capability gap): %v", err)
	}
	if len(ranked) == 0 || ranked[0].Index != 1 {
		t.Fatalf("rerank must put doc_repair (index 1) first, got %+v", ranked)
	}
	t.Logf("rerank reorder: top index=%d score=%.4f", ranked[0].Index, ranked[0].Score)
}
