// atlas-api is the stable runtime entrypoint: answer API, work-brief
// ingestion, space/timeline/trace queries. Composition root: PostgreSQL,
// OpenSearch, NATS JetStream, object storage, AgentNexus client, and the
// OpenAI-compatible model adapter.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/adk/model"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/auditrefs"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
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

	// Transport security: one Manager per named link, loaded fail-closed —
	// NewManager returns a named, sanitized error (never key material) if
	// a link's Mode != off but its cert/key/trust-bundle material is
	// missing or invalid. Every Manager is a safe no-op when its Mode is
	// off (today's backward-compatible default).
	tlsLinks, err := loadTLSManagers(cfg)
	if err != nil {
		return err
	}

	rawNexusClient, err := nexusclient.New(cfg.AgentNexus.BaseURL, 30*time.Second, cfg.AgentNexus.ClientID, cfg.AgentNexus.SecretFile, tlsLinks.agentNexus)
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
	pool, err := storage.NewPool(ctx, cfg.Postgres.DSN, tlsLinks.postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	queries := db.New(pool)

	search, err := retrieval.NewHTTPSearchClient(cfg.OpenSearch.Addresses, cfg.OpenSearch.Username, cfg.OpenSearch.Password, tlsLinks.openSearch)
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

	// Model access degrades rather than exits, for the same reason the
	// keyword-only branch above does: an unconfigured router removes answer
	// generation, not space/timeline/trace queries or work-brief ingestion.
	// See composeModel.
	llm, modelGap, err := composeModel(cfg.LLMRouter, tlsLinks.llmRouter)
	if err != nil {
		return err
	}
	if modelGap != "" {
		logger.Warn("answer generation is not composed", zap.String("reason", modelGap))
	}

	bus, err := tasks.NewNATSBus(ctx, cfg.NATS.URL, tlsLinks.nats)
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
	nexusClient := auditrefs.New(rawNexusClient, metrics)
	retrievalSvc := retrieval.NewService(queries, search, embedder, reranker)
	retrievalSvc.SetMetrics(metrics)
	traceSvc := trace.NewService(queries)
	traceSvc.SetMetrics(metrics)

	// Artifact ingestion (upload + job rows + enqueue); processing runs on
	// atlas-worker, so no parser gateway or summarizer here.
	objects, err := storage.NewObjectStore(cfg.ObjectStorage, tlsLinks.objectStorage)
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
		TLS: tlsLinks.agentAtlas, NotReadyReason: modelGap,
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
		zap.Bool("answer_generation", llm != nil),
		zap.String("tls_mode", string(cfg.TLS.AgentAtlas.Mode)),
	)
	server := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	ln, err = tlsLinks.agentAtlas.WrapListener(ln)
	if err != nil {
		return fmt.Errorf("agentatlas server tls: %w", err)
	}
	return server.Serve(ln)
}

// composeModel builds the OpenAI-compatible model adapter, or explains why it
// could not.
//
// An unconfigured router is a deployment gap, and this binary deliberately does
// NOT exit on it — for the same reason AgentNexus's gateway-agent does not
// (agentnexus/services/agentnexus/cmd/gateway-agent/main.go): a container that
// dies on boot leaves the operator with no health surface to ask, and the rest
// of this API does not need a model. /v1/answer answers 503 when the model is
// nil; everything else is unaffected.
//
// The gap branch returns a nil INTERFACE, not a typed nil pointer, so the
// handler's nil check actually fires.
//
// The reason is served from /healthz and logged, so it names the environment
// variable rather than any value: one of those values is the router API key.
func composeModel(router config.LLMRouter, tls *transportsecurity.Manager) (model.LLM, string, error) {
	if strings.TrimSpace(router.BaseURL) == "" {
		return nil, "answer generation not composed: llmrouter has no endpoint, set ATLAS_LLMROUTER_BASE_URL", nil
	}
	defaultModel := router.DefaultModel
	if strings.TrimSpace(defaultModel) == "" {
		defaultModel = "deepseek-v4-flash"
	}
	llm, err := llmroutermodel.New(llmroutermodel.Config{
		BaseURL: router.BaseURL, APIKey: router.APIKey,
		DefaultModel: defaultModel, Timeout: 120 * time.Second, TLS: tls,
	})
	if err != nil {
		return nil, "", err
	}
	return llm, "", nil
}

// tlsManagers is atlas-api's transport-security composition: one Manager
// per named link this binary participates in (its own server identity plus
// every dependency it dials out to).
type tlsManagers struct {
	agentAtlas    *transportsecurity.Manager // server identity (this binary accepts inbound connections)
	agentNexus    *transportsecurity.Manager
	llmRouter     *transportsecurity.Manager
	postgres      *transportsecurity.Manager
	openSearch    *transportsecurity.Manager
	nats          *transportsecurity.Manager
	objectStorage *transportsecurity.Manager
}

func loadTLSManagers(cfg *config.Config) (tlsManagers, error) {
	var out tlsManagers
	var err error
	if out.agentAtlas, err = transportsecurity.NewManager("AgentAtlas", transportsecurity.FromLinkTLS(cfg.TLS.AgentAtlas)); err != nil {
		return out, fmt.Errorf("agentatlas server tls: %w", err)
	}
	if out.agentNexus, err = transportsecurity.NewManager("AgentNexus", transportsecurity.FromLinkTLS(cfg.TLS.AgentNexus)); err != nil {
		return out, fmt.Errorf("agentnexus tls: %w", err)
	}
	if out.llmRouter, err = transportsecurity.NewManager("llmrouter", transportsecurity.FromLinkTLS(cfg.TLS.LLMRouter)); err != nil {
		return out, fmt.Errorf("llmrouter tls: %w", err)
	}
	if out.postgres, err = transportsecurity.NewManager("PostgreSQL", transportsecurity.FromLinkTLS(cfg.TLS.Postgres)); err != nil {
		return out, fmt.Errorf("postgres tls: %w", err)
	}
	if out.openSearch, err = transportsecurity.NewManager("OpenSearch", transportsecurity.FromLinkTLS(cfg.TLS.OpenSearch)); err != nil {
		return out, fmt.Errorf("opensearch tls: %w", err)
	}
	if out.nats, err = transportsecurity.NewManager("NATS", transportsecurity.FromLinkTLS(cfg.TLS.NATS)); err != nil {
		return out, fmt.Errorf("nats tls: %w", err)
	}
	if out.objectStorage, err = transportsecurity.NewManager("object storage", transportsecurity.FromLinkTLS(cfg.TLS.ObjectStorage)); err != nil {
		return out, fmt.Errorf("object storage tls: %w", err)
	}
	return out, nil
}
