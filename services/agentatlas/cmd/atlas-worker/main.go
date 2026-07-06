// atlas-worker consumes the async plane: artifact processing, index jobs, and
// dream runs over NATS JetStream (workflow runs join with Goal B5). atlas-api
// only enqueues — this binary owns consumption. Composition root: PostgreSQL,
// object storage, parser gateway, OpenSearch, llmrouter (model + embeddings),
// and the dream synthesizer.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "atlas-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv("ATLAS_CONFIG"))
	if err != nil {
		return err
	}
	logger, err := observability.NewLogger("atlas-worker")
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

	objects, err := storage.NewObjectStore(cfg.ObjectStorage)
	if err != nil {
		return err
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		return err
	}

	registry, err := parsergateway.NewRegistry(
		parsergateway.NewDoclingProvider(cfg.Parsers.Docling.BaseURL),
		parsergateway.NewMinerUProvider(cfg.Parsers.MinerU.BaseURL),
		parsergateway.NewASRProvider(cfg.Parsers.ASR.BaseURL),
		parsergateway.NewVideoProvider(cfg.Parsers.Video.BaseURL),
	)
	if err != nil {
		return err
	}

	search, err := retrieval.NewHTTPSearchClient(cfg.OpenSearch.Addresses, cfg.OpenSearch.Username, cfg.OpenSearch.Password)
	if err != nil {
		return err
	}

	// Model-backed intelligence: embeddings for indexing, summarizer for
	// artifacts, synthesizer for dream runs. No API key = explicit
	// deterministic/keyword-only degraded mode.
	var embedder retrieval.Embedder
	var summarizer *artifacts.Summarizer
	var synthesizer *dream.Synthesizer
	intelligence := "deterministic"
	if cfg.LLMRouter.APIKey != "" {
		embedder, err = retrieval.NewLLMRouterEmbedder(cfg.LLMRouter.BaseURL, cfg.LLMRouter.APIKey, "bge-m3")
		if err != nil {
			return err
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
		summarizer = artifacts.NewSummarizer(llm)
		synthesizer = dream.NewSynthesizer(llm)
		intelligence = "llm"
	} else {
		logger.Warn("llmrouter api key not configured: worker runs deterministic summaries and keyword-only indexing")
	}

	bus, err := tasks.NewNATSBus(ctx, cfg.NATS.URL)
	if err != nil {
		return fmt.Errorf("nats (production-standard dependency): %w", err)
	}
	runner := tasks.NewRunner(bus)

	artifactSvc := artifacts.NewService(queries, objects, parsergateway.NewGateway(registry), runner, summarizer)
	dreamRunner := dream.NewRunner(queries, objects, dream.NewPolicyService(queries), runner, synthesizer)
	indexer := retrieval.NewIndexer(queries, search, embedder)

	w := worker.New(runner, worker.Deps{
		Artifacts: artifactSvc,
		Dreams:    dreamRunner,
		Indexer:   indexer,
	})
	if err := w.Start(ctx); err != nil {
		return err
	}
	logger.Info("atlas-worker consuming",
		zap.String("nats", cfg.NATS.URL),
		zap.String("intelligence", intelligence),
		zap.Strings("job_types", []string{artifacts.JobTypeArtifact, retrieval.JobTypeIndex, dream.JobTypeDream}),
	)

	<-ctx.Done()
	logger.Info("atlas-worker draining")
	w.Stop()
	return nil
}
