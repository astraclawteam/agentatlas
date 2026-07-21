// atlas-worker consumes the async plane: artifact processing, index jobs, and
// dream runs over NATS JetStream (workflow runs join with Goal B5). atlas-api
// only enqueues — this binary owns consumption. Composition root: PostgreSQL,
// object storage, parser gateway, OpenSearch, llmrouter (model + embeddings),
// and the dream synthesizer.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmutil"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/spaces"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/trace"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/worker"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
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

	// Transport security: one Manager per named link, loaded fail-closed —
	// NewManager returns a named, sanitized error (never key material) if
	// a link's Mode != off but its cert/key/trust-bundle material is
	// missing or invalid. Every Manager is a safe no-op when its Mode is
	// off (today's backward-compatible default). atlas-worker has no
	// inbound API server of its own (only the plaintext metrics listener,
	// unaffected — not one of the named links), so this composes CLIENT
	// links only.
	tlsLinks, err := loadTLSManagers(cfg)
	if err != nil {
		return err
	}

	nexusClient, err := nexusclient.New(cfg.AgentNexus.BaseURL, 30*time.Second, cfg.AgentNexus.ClientID, cfg.AgentNexus.SecretFile, tlsLinks.agentNexus)
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
	pool, err := storage.NewPool(ctx, cfg.Postgres.DSN, tlsLinks.postgres)
	if err != nil {
		return err
	}
	defer pool.Close()
	queries := db.New(pool)

	objects, err := storage.NewObjectStore(cfg.ObjectStorage, tlsLinks.objectStorage)
	if err != nil {
		return err
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		return err
	}

	// Deployment topology: worker -> parser-gateway service over HTTP when
	// configured; in-process gateway otherwise. TLS ("parser" link) only
	// applies to the standalone-gateway path — the in-process gateway
	// dials the docling/mineru/asr/video sidecar providers directly, which
	// is out of this task's scope (see notes.md).
	var parserGW artifacts.ParserGateway
	if cfg.Parsers.GatewayURL != "" {
		parserGW, err = parsergateway.NewHTTPClient(cfg.Parsers.GatewayURL, tlsLinks.parser)
		if err != nil {
			return err
		}
	} else {
		registry, err := parsergateway.NewRegistry(
			parsergateway.NewDoclingProvider(cfg.Parsers.Docling.BaseURL),
			parsergateway.NewMinerUProvider(cfg.Parsers.MinerU.BaseURL),
			parsergateway.NewASRProvider(cfg.Parsers.ASR.BaseURL),
			parsergateway.NewVideoProvider(cfg.Parsers.Video.BaseURL),
		)
		if err != nil {
			return err
		}
		parserGW = parsergateway.NewGateway(registry)
	}

	search, err := retrieval.NewHTTPSearchClient(cfg.OpenSearch.Addresses, cfg.OpenSearch.Username, cfg.OpenSearch.Password, tlsLinks.openSearch)
	if err != nil {
		return err
	}

	// Model-backed intelligence: embeddings+rerank for retrieval, summarizer
	// for artifacts, synthesizer for dream runs, and the Knowledge Agent for
	// SOP extraction + answer generation in workflow nodes. No API key =
	// explicit deterministic/keyword-only degraded mode.
	var embedder retrieval.Embedder
	var reranker retrieval.Reranker
	var summarizer *artifacts.Summarizer
	var synthesizer *dream.Synthesizer
	var sopExtractor workflow.SOPExtractor
	var answerGen workflow.AnswerGenerator
	intelligence := "deterministic"
	if cfg.LLMRouter.APIKey != "" {
		embedder, err = retrieval.NewLLMRouterEmbedder(cfg.LLMRouter.BaseURL, cfg.LLMRouter.APIKey, "bge-m3")
		if err != nil {
			return err
		}
		reranker, err = retrieval.NewLLMRouterReranker(cfg.LLMRouter.BaseURL, cfg.LLMRouter.APIKey, cfg.LLMRouter.RerankModel)
		if err != nil {
			return err
		}
		defaultModel := cfg.LLMRouter.DefaultModel
		if defaultModel == "" {
			defaultModel = "deepseek-v4-flash"
		}
		llm, err := llmroutermodel.New(llmroutermodel.Config{
			BaseURL: cfg.LLMRouter.BaseURL, APIKey: cfg.LLMRouter.APIKey,
			DefaultModel: defaultModel, Timeout: 120 * time.Second, TLS: tlsLinks.llmRouter,
		})
		if err != nil {
			return err
		}
		summarizer = artifacts.NewSummarizer(llm)
		synthesizer = dream.NewSynthesizer(llm)
		agentRunner, err := agent.NewRunner(llm)
		if err != nil {
			return err
		}
		sopExtractor = func(ctx context.Context, doc agent.SOPDocument) (sdkworkflow.Workflow, error) {
			return agent.ExtractSOP(ctx, agentRunner, doc)
		}
		answerGen = func(ctx context.Context, question string, snippets []string) (string, string, error) {
			var prompt strings.Builder
			fmt.Fprintf(&prompt, "问题：%s\n", question)
			if len(snippets) > 0 {
				prompt.WriteString("依据材料：\n")
				for i, s := range snippets {
					fmt.Fprintf(&prompt, "%d. %s\n", i+1, s)
				}
			}
			text, err := llmutil.CompleteText(ctx, llm,
				"你是 AgentAtlas 的回答生成器。只根据提供的依据材料用中文回答；材料不足时明确说不确定，不得虚构。", prompt.String())
			if err != nil {
				return "", "", err
			}
			return strings.TrimSpace(text), llm.Name(), nil
		}
		intelligence = "llm"
	} else {
		logger.Warn("llmrouter api key not configured: worker runs deterministic summaries and keyword-only indexing")
	}

	bus, err := tasks.NewNATSBus(ctx, cfg.NATS.URL, tlsLinks.nats)
	if err != nil {
		return fmt.Errorf("nats (production-standard dependency): %w", err)
	}
	runner := tasks.NewRunner(bus)

	// Worker-side observability: dream/workflow/retrieval/parser/trace metrics
	// fire here; exposed on ATLAS_WORKER_METRICS_ADDR.
	metrics := observability.NewMetrics()
	shutdownTracing, err := observability.InitTracing(ctx, "atlas-worker")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	artifactSvc := artifacts.NewService(queries, objects, parserGW, runner, summarizer)
	indexer := retrieval.NewIndexer(queries, search, embedder)
	retrievalSvc := retrieval.NewService(queries, search, embedder, reranker)
	retrievalSvc.SetMetrics(metrics)
	traceSvc := trace.NewService(queries)
	traceSvc.SetMetrics(metrics)
	// Workflow runs execute HERE: real executors behind every node type.
	workflowSvc, err := workflow.NewService(queries)
	if err != nil {
		return err
	}
	registry := workflow.NewRegistryWithServices(workflow.Executors{
		Artifacts: artifactSvc,
		Documents: artifactSvc,
		SOP:       sopExtractor,
		Summarize: summarizer,
		Retrieval: retrievalSvc,
		Nexus:     nexusClient,
		Dream:     synthesizer.AggregateWorkflowInput,
		Answer:    answerGen,
		Traces:    traceSvc,
	})
	workflowRuntime := workflow.NewRuntime(queries, workflowSvc, registry)
	workflowRuntime.SetMetrics(metrics)
	dreamRunner := dream.NewRunner(queries, objects, dream.NewPolicyService(queries), runner, dream.NewOrchestrator(workflowRuntime))
	workflowRuntime.SetDreamLifecycleHook(dreamRunner.WorkflowLifecycle)
	dreamRunner.SetMetrics(metrics)
	runJobs := workflow.NewRunJobHandler(workflowRuntime, queries, runner)

	metricsAddr := os.Getenv("ATLAS_WORKER_METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9091"
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metrics.Handler())
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = metricsSrv.ListenAndServe() }()
	defer func() { _ = metricsSrv.Close() }()

	w := worker.New(runner, worker.Deps{
		Artifacts: artifactSvc,
		Dreams:    dreamRunner,
		Indexer:   indexer,
		Extra:     []worker.Registrar{runJobs},
	})
	if err := w.Start(ctx); err != nil {
		return err
	}
	if err := dreamRunner.ReconcileWorkflowLifecycle(ctx); err != nil {
		logger.Warn("initial Dream workflow lifecycle reconciliation", zap.Error(err))
	}
	if err := dreamRunner.RecoverExpiredExecutions(ctx); err != nil {
		logger.Warn("initial Dream execution recovery", zap.Error(err))
	}
	if err := runJobs.RecoverPendingRuns(ctx); err != nil {
		logger.Warn("initial workflow execution recovery", zap.Error(err))
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := dreamRunner.ReconcileWorkflowLifecycle(ctx); err != nil {
					logger.Warn("Dream workflow lifecycle reconciliation", zap.Error(err))
				}
				if err := dreamRunner.RecoverExpiredExecutions(ctx); err != nil {
					logger.Warn("Dream execution recovery", zap.Error(err))
				}
				if err := runJobs.RecoverPendingRuns(ctx); err != nil {
					logger.Warn("workflow execution recovery", zap.Error(err))
				}
			}
		}
	}()

	// Dream scheduling: the worker ticks every published policy's cron window
	// across all enterprises (ATLAS_DREAM_TICK_SECONDS, default 60; 0 = off).
	tickSeconds := 60
	if v := strings.TrimSpace(os.Getenv("ATLAS_DREAM_TICK_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tickSeconds = n
		}
	}
	if tickSeconds > 0 {
		scheduler := dream.NewScheduler(queries, dream.NewPolicyService(queries), runner)
		go func() {
			ticker := time.NewTicker(time.Duration(tickSeconds) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					enterprises, err := queries.ListEnterprises(ctx)
					if err != nil {
						logger.Warn("dream tick: list enterprises", zap.Error(err))
						continue
					}
					for _, ent := range enterprises {
						if n, err := scheduler.Tick(ctx, ent.ID, now.UTC()); err != nil {
							logger.Warn("dream tick", zap.String("enterprise_id", ent.ID), zap.Error(err))
						} else if n > 0 {
							logger.Info("dream runs dispatched", zap.String("enterprise_id", ent.ID), zap.Int("count", n))
						}
					}
				}
			}
		}()
		logger.Info("dream scheduler ticking", zap.Int("interval_seconds", tickSeconds))
	}

	// Org-version cursor: the worker owns the AgentNexus org-event subscription
	// (SSE) and records the tenant's sealed org version. It does NOT create
	// spaces from events -- the frozen OrgEvent carries no org unit identity,
	// so that is impossible by contract rather than merely unwired.
	//
	// ATLAS_ORG_SYNC_ENTERPRISES still names the tenant for local bookkeeping,
	// but it is never sent upstream: the feed is scoped by the verified service
	// credential. A deployment whose credential does not match a listed tenant
	// records versions under the wrong key, so the list must agree with the
	// credential.
	if ents := strings.TrimSpace(os.Getenv("ATLAS_ORG_SYNC_ENTERPRISES")); ents != "" {
		syncer := spaces.NewSyncer(queries, nexusClient, logger)
		for _, ent := range strings.Split(ents, ",") {
			ent = strings.TrimSpace(ent)
			if ent == "" {
				continue
			}
			go func(enterpriseID string) {
				for ctx.Err() == nil {
					err := syncer.Run(ctx, enterpriseID, 0)
					if errors.Is(err, nexusclient.ErrOrgCursorAhead) {
						// A cursor ahead of the sealed version can never
						// succeed on retry; looping would hammer the feed
						// forever. Stop this tenant and say so.
						logger.Error("org cursor is ahead of the sealed org version; not retrying",
							zap.String("enterprise_id", enterpriseID), zap.Error(err))
						return
					}
					if err != nil && ctx.Err() == nil {
						logger.Warn("org version stream ended; retrying",
							zap.String("enterprise_id", enterpriseID), zap.Error(err))
					}
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Second):
					}
				}
			}(ent)
		}
		logger.Info("org version cursor subscribed", zap.String("enterprises", ents))
	}
	logger.Info("atlas-worker consuming",
		zap.String("nats", cfg.NATS.URL),
		zap.String("intelligence", intelligence),
		zap.Strings("job_types", []string{artifacts.JobTypeArtifact, retrieval.JobTypeIndex, dream.JobTypeDream, workflow.JobTypeRun}),
	)

	<-ctx.Done()
	logger.Info("atlas-worker draining")
	w.Stop()
	return nil
}

// tlsManagers is atlas-worker's transport-security composition: one
// Manager per named CLIENT link this binary dials out to (atlas-worker has
// no inbound API server of its own).
type tlsManagers struct {
	agentNexus    *transportsecurity.Manager
	llmRouter     *transportsecurity.Manager
	postgres      *transportsecurity.Manager
	openSearch    *transportsecurity.Manager
	nats          *transportsecurity.Manager
	objectStorage *transportsecurity.Manager
	parser        *transportsecurity.Manager // atlas-worker -> standalone parser-gateway
}

func loadTLSManagers(cfg *config.Config) (tlsManagers, error) {
	var out tlsManagers
	var err error
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
	if out.parser, err = transportsecurity.NewManager("parser", transportsecurity.FromLinkTLS(cfg.TLS.Parser)); err != nil {
		return out, fmt.Errorf("parser tls: %w", err)
	}
	return out, nil
}
