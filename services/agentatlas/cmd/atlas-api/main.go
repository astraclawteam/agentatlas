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
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/auditrefs"
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

	var embedder retrieval.Embedder
	var reranker retrieval.Reranker
	if cfg.LLMRouter.APIKey != "" {
		embedder, err = retrieval.NewLLMRouterEmbedder(cfg.LLMRouter.BaseURL, cfg.LLMRouter.APIKey, "bge-m3")
		if err != nil {
			return err
		}
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
	runner := tasks.NewRunner(bus)
	indexer := retrieval.NewIndexer(queries, search, embedder)
	if err := indexer.RegisterJobHandler(runner); err != nil {
		return err
	}
	if err := runner.Start(ctx); err != nil {
		return err
	}
	defer runner.Stop()

	metrics := observability.NewMetrics()
	shutdownTracing, err := observability.InitTracing(ctx, "atlas-api")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// audit appends are counted and fail closed on the answer path
	nexusClient := auditrefs.New(nexusclient.New(cfg.AgentNexus.BaseURL, 30*time.Second), metrics)
	retrievalSvc := retrieval.NewService(queries, search, embedder, reranker)
	traceSvc := trace.NewService(queries)

	router := app.NewRouter(app.RouterDeps{
		Nexus: nexusClient, Retrieval: retrievalSvc, Traces: traceSvc,
		LLM: llm, Store: queries, Runner: runner, Metrics: metrics,
	})

	addr := os.Getenv("ATLAS_API_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	logger.Info("atlas-api serving",
		zap.String("addr", addr),
		zap.String("version", app.Version),
		zap.String("agentnexus", cfg.AgentNexus.BaseURL),
		zap.Bool("vector_retrieval", embedder != nil),
	)
	server := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}
