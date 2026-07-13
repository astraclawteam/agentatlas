// atlas-agent is the agent-first control plane: Knowledge Agent runs, workflow
// draft/publish, dream policies, and confirmations. Composition root:
// PostgreSQL, AgentNexus client, llmrouter model adapter, the ADK Knowledge
// Agent runner, and the workflow/dream services.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/auditrefs"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/llmroutermodel"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "atlas-agent:", err)
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
	rawNexusClient, err := nexusclient.New(cfg.AgentNexus.BaseURL, 30*time.Second, cfg.AgentNexus.ClientID, cfg.AgentNexus.SecretFile)
	if err != nil {
		return err
	}
	if cfg.AgentNexus.ApprovalFactsSecretFile != "" {
		if err := rawNexusClient.ConfigureApprovalFactsSecret(cfg.AgentNexus.ApprovalFactsSecretFile); err != nil {
			return err
		}
	}
	logger, err := observability.NewLogger("atlas-agent")
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
	consoleSecret, err := browsersession.LoadConsoleClientSecret(cfg.AgentNexus.BrowserClientSecretFile)
	if err != nil {
		return fmt.Errorf("browser client secret: %w", err)
	}
	encryptionKey, err := browsersession.LoadEncryptionKey(cfg.AgentNexus.BrowserSessionEncryptionKeyFile)
	if err != nil {
		return fmt.Errorf("browser session key: %w", err)
	}
	keys := []browsersession.EncryptionKey{{ID: cfg.AgentNexus.BrowserSessionEncryptionKeyID, Key: encryptionKey}}
	if cfg.AgentNexus.BrowserSessionPreviousEncryptionKeyFile != "" {
		previousKey, err := browsersession.LoadEncryptionKey(cfg.AgentNexus.BrowserSessionPreviousEncryptionKeyFile)
		if err != nil {
			return fmt.Errorf("previous browser session key: %w", err)
		}
		keys = append(keys, browsersession.EncryptionKey{ID: cfg.AgentNexus.BrowserSessionPreviousEncryptionKeyID, Key: previousKey})
	}
	protector, err := browsersession.NewProtectorKeyring(keys[0], keys[1:]...)
	if err != nil {
		return err
	}
	browserStore, err := browsersession.NewPostgresStore(pool, protector, time.Now)
	if err != nil {
		return err
	}
	oidcClient := browsersession.NewOIDCClient(&http.Client{Timeout: 30 * time.Second}, cfg.AgentNexus.BaseURL, time.Now)
	browserSessions, err := browsersession.New(browsersession.Config{Issuer: cfg.AgentNexus.BaseURL, ClientID: cfg.AgentNexus.BrowserClientID, ClientSecret: consoleSecret, RedirectURI: strings.TrimRight(cfg.AgentNexus.AtlasPublicURL, "/") + "/auth/callback"}, browserStore, oidcClient, time.Now)
	if err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := browserSessions.ReconcileLogouts(ctx, 100); err != nil && ctx.Err() == nil {
					logger.Error("browser logout reconciliation failed", zap.Error(err))
				}
			}
		}
	}()
	changeStore, err := governance.NewPostgresStore(pool, time.Now)
	if err != nil {
		return err
	}
	changes := governance.NewService(changeStore, governance.NexusRouteResolver{Client: rawNexusClient, Now: time.Now}, governance.NexusAuditAppender{Client: rawNexusClient}, nil, time.Now)
	changes.SetAuthorizer(governance.NexusAuthorizer{Client: rawNexusClient})

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
	agentRunner, err := agent.NewRunner(llm)
	if err != nil {
		return err
	}
	workflowSvc, err := workflow.NewService(queries)
	if err != nil {
		return err
	}
	// Runtime here only registers pending runs (CreatePending) and records
	// confirm decisions (Resume → back to pending); it never executes nodes.
	// The confirm handler re-enqueues approved runs so atlas-worker — the only
	// process with the fully wired executor registry — runs the remaining
	// nodes. The built-in registry below is therefore intentionally inert.
	workflowRuntime := workflow.NewRuntime(queries, workflowSvc, workflow.NewRegistry())
	dreamPolicies := dream.NewPolicyService(queries)

	// Producer-only task runner: the control plane enqueues workflow runs;
	// atlas-worker consumes them.
	bus, err := tasks.NewNATSBus(ctx, cfg.NATS.URL)
	if err != nil {
		return fmt.Errorf("nats (production-standard dependency): %w", err)
	}
	taskRunner := tasks.NewRunner(bus)
	taskRunner.AllowEnqueue(workflow.JobTypeRun, retrieval.JobTypeIndex, dream.JobTypeDream)
	dreamScheduler := dream.NewScheduler(queries, dreamPolicies, taskRunner)

	metrics := observability.NewMetrics()
	workflowRuntime.SetMetrics(metrics) // confirm-resume terminal statuses count here
	shutdownTracing, err := observability.InitTracing(ctx, "atlas-agent")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// audit appends are counted; write paths fail closed on append failure
	nexusClient := auditrefs.New(rawNexusClient, metrics)

	router := app.NewAgentRouter(app.AgentRouterDeps{
		Nexus: nexusClient, Agent: agentRunner, Workflows: workflowSvc,
		Runtime: workflowRuntime, Dreams: dreamPolicies, Store: queries,
		DreamRerun: dreamScheduler, Runner: taskRunner, Metrics: metrics,
		BrowserSessions: browserSessions, BrowserHandleProtector: protector, Changes: changes, BrowserAuthorizer: rawNexusClient,
	})

	addr := os.Getenv("ATLAS_AGENT_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	logger.Info("atlas-agent serving",
		zap.String("addr", addr),
		zap.String("version", app.Version),
		zap.String("agentnexus", cfg.AgentNexus.BaseURL),
		zap.String("model", defaultModel),
	)
	server := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("atlas-agent shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
