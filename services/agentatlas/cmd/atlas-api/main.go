// atlas-api is the stable runtime entrypoint: answer API, work-brief
// ingestion, space/timeline/trace queries. Composition root: PostgreSQL,
// OpenSearch, NATS JetStream, object storage, AgentNexus client, and the
// OpenAI-compatible model adapter.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/auditrefs"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "atlas-api:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	cfg, err := config.Load(os.Getenv("ATLAS_CONFIG"))
	if err != nil {
		return err
	}
	logger, err := observability.NewLogger("atlas-api")
	if err != nil {
		return err
	}
	defer logger.Sync()

	if err := storage.Migrate(ctx, cfg.Postgres.DSN); err != nil {
		return err
	}
	pool, err := storage.NewPool(ctx, cfg.Postgres.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	queries := db.New(pool)

	search, err := retrieval.NewHTTPSearchClient(cfg.OpenSearch.Addresses, cfg.OpenSearch.Username, cfg.OpenSearch.Password)
	if err != nil {
		return err
	}

	// Retrieval mode is explicit: a configured model route gets real bge-m3
	// embeddings AND rerank; no API key is the keyword-only degraded mode.
	var embedder retrieval.Embedder
	var reranker retrieval.Reranker
	retrievalMode := "keyword_only"
	if cfg.LLMRouter.APIKey != "" {
		embedder, err = retrieval.NewLLMRouterEmbedder(cfg.LLMRouter.BaseURL, cfg.LLMRouter.APIKey, "bge-m3")
		if err != nil {
			return err
		}
		reranker, err = retrieval.NewLLMRouterReranker(cfg.LLMRouter.BaseURL, cfg.LLMRouter.APIKey, cfg.LLMRouter.RerankModel)
		if err != nil {
			return err
		}
		retrievalMode = "vector+rerank"
	} else {
		logger.Warn("llmrouter api key not configured: retrieval runs KEYWORD-ONLY (no vectors, no rerank)")
	}

	defaultModel := cfg.LLMRouter.DefaultModel
	if defaultModel == "" {
		defaultModel = "deepseek-v4-flash"
	}
	llm, err := llmroutermodel.New(llmroutermodel.Config{
		BaseURL: cfg.LLMRouter.BaseURL, APIKey: cfg.LLMRouter.APIKey,
		DefaultModel: defaultModel, Timeout: 120 * time.Second,
	})
	if err != nil {
		return err
	}

	bus, err := tasks.NewNATSBus(ctx, cfg.NATS.URL)
	if err != nil {
		return fmt.Errorf("nats (production-standard dependency): %w", err)
	}
	// Producer/consumer split: atlas-api only ENQUEUES async jobs; atlas-worker
	// registers the handlers and consumes them off NATS JetStream.
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(retrieval.JobTypeIndex, artifacts.JobTypeArtifact, dream.JobTypeDream)

	metrics := observability.NewMetrics()
	shutdownTracing, err := observability.InitTracing(ctx, "atlas-api")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// audit appends are counted and fail closed on the answer path
	nexusClient := auditrefs.New(nexusclient.New(cfg.AgentNexus.BaseURL, 30*time.Second), metrics)
	retrievalSvc := retrieval.NewService(queries, search, embedder, reranker)
	retrievalSvc.SetMetrics(metrics)
	traceSvc := trace.NewService(queries)
	traceSvc.SetMetrics(metrics)

	// Artifact ingestion (upload + job rows + enqueue); processing runs on
	// atlas-worker, so no parser gateway or summarizer here.
	objects, err := storage.NewObjectStore(cfg.ObjectStorage)
	if err != nil {
		return err
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		return err
	}
	artifactSvc := artifacts.NewService(queries, objects, nil, runner, nil)

	router := app.NewRouter(app.RouterDeps{
		Nexus: nexusClient, Retrieval: retrievalSvc, Traces: traceSvc,
		LLM: llm, Store: queries, Runner: runner, Artifacts: artifactSvc, Metrics: metrics,
	})

	addr := os.Getenv("ATLAS_API_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	logger.Info("atlas-api serving",
		zap.String("addr", addr),
		zap.String("version", app.Version),
		zap.String("agentnexus", cfg.AgentNexus.BaseURL),
		zap.String("retrieval_mode", retrievalMode),
	)
	server := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}
