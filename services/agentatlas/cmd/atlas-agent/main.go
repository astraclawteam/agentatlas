// atlas-agent is the agent-first control plane: Knowledge Agent runs, workflow
// draft/publish, dream policies, and confirmations. Composition root:
// PostgreSQL, AgentNexus client, llmrouter model adapter, the ADK Knowledge
// Agent runner, and the workflow/dream services.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
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
	// Configuration is validated BEFORE any irreversible work. storage.Migrate
	// below applies 22 migrations; a service that refuses to start on its own
	// configuration must not have mutated the schema on the way out.
	if err := validateConfig(cfg); err != nil {
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
	// The approval-facts attestation belonged to the retired resolution
	// surface: AgentAtlas used to sign the facts it asked AgentNexus to decide
	// on. AgentNexus no longer decides, so there are no facts to attest here -
	// the authority attests its own decision instead, and that signature comes
	// back with the evidence.
	logger, err := observability.NewLogger("atlas-agent")
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
	changes := governance.NewService(changeStore, governance.NexusRouteResolver{Client: rawNexusClient, Now: time.Now, Transmit: rawNexusClient, Authority: cfg.AgentNexus.ApprovalAuthority}, governance.NexusAuditAppender{Client: rawNexusClient}, nil, time.Now)
	changes.SetAuthorizer(governance.NexusAuthorizer{Client: rawNexusClient})

	defaultModel := resolvedDefaultModel(cfg.LLMRouter)
	agentRunner, llm, modelGap, err := composeAgentRunner(cfg.LLMRouter, defaultModel, tlsLinks.llmRouter)
	if err != nil {
		return err
	}
	if modelGap != "" {
		logger.Warn("knowledge agent is not composed", zap.String("reason", modelGap))
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
	bus, err := tasks.NewNATSBus(ctx, cfg.NATS.URL, tlsLinks.nats)
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

	deps := app.AgentRouterDeps{
		Nexus: nexusClient, Agent: agentRunner, Workflows: workflowSvc,
		Runtime: workflowRuntime, Dreams: dreamPolicies, Store: queries,
		DreamRerun: dreamScheduler, Runner: taskRunner, Metrics: metrics,
		BrowserSessions: browserSessions, BrowserHandleProtector: protector, Changes: changes, BrowserAuthorizer: rawNexusClient,
		ApprovalTransmitter: rawNexusClient, ApprovalAuthority: cfg.AgentNexus.ApprovalAuthority,
		// OrgAuthorization and Evidence are the frozen-contract surfaces the
		// dream paths reach. Both fail closed when nil, so leaving them unset
		// did not crash - it made every org check answer 502 and every
		// evidence drill-down unreachable, while the tests stayed green
		// because each test sets them itself.
		OrgAuthorization: rawNexusClient, Evidence: rawNexusClient,
		TLS: tlsLinks.agentAtlas, NotReadyReason: modelGap,
		Dependencies: readinessProbes(pool, bus, rawNexusClient, llm),
	}
	// Refuse to start with a required dependency unwired. These deps fail
	// closed, so an unset one does not crash - it silently removes a feature,
	// which is how this binary once served every dream-policy org check as a
	// 502 while its tests were green. Naming them at boot turns that into a
	// startup failure an operator can act on.
	if missing := deps.MissingRequired(); len(missing) > 0 {
		return fmt.Errorf("atlas-agent composition is incomplete; unwired dependencies: %s", strings.Join(missing, ", "))
	}
	router := app.NewAgentRouter(deps)

	addr := os.Getenv("ATLAS_AGENT_ADDR")
	if addr == "" {
		addr = ":8081"
	}
	logger.Info("atlas-agent serving",
		zap.String("addr", addr),
		zap.String("version", app.Version),
		zap.String("agentnexus", cfg.AgentNexus.BaseURL),
		zap.String("model", defaultModel),
		zap.Bool("knowledge_agent", agentRunner != nil),
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

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()
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

// validateConfig rejects the deployment gaps this binary cannot serve around,
// before it touches PostgreSQL. Everything checked here is a value an operator
// sets, so the errors name the setting rather than the code that reads it.
func validateConfig(cfg *config.Config) error {
	// An unset approval authority is a deployment gap, not a default: the
	// resolver and the dream handlers all fail closed on it rather than
	// approving locally, so refuse to start instead of serving a governance
	// surface that can only ever answer 503.
	if strings.TrimSpace(cfg.AgentNexus.ApprovalAuthority) == "" {
		return errors.New("agentnexus.approval_authority is required: name the customer OA/BPM system that decides governed changes")
	}
	return nil
}

// resolvedDefaultModel applies the model name this deployment requests when the
// operator has not named one. Unlike the router endpoint it is safe to default:
// a wrong model name is answered by the router, not silently swallowed.
func resolvedDefaultModel(router config.LLMRouter) string {
	if strings.TrimSpace(router.DefaultModel) == "" {
		return "deepseek-v4-flash"
	}
	return router.DefaultModel
}

// composeAgentRunner builds the Knowledge Agent runner, or explains why it
// could not.
//
// An unconfigured router is a deployment gap, and this binary deliberately does
// NOT exit on it. Workflow drafting, dream policies, governed changes and the
// whole Console BFF need no model; only /v1/agent/runs does, and it already
// answers 503 when the runner is nil. AgentNexus's gateway-agent answers the
// identical gap the same way (agentnexus/services/agentnexus/cmd/gateway-agent/
// main.go): start, serve health, report not-ready with a stated reason. Two
// products in one stack must not answer "no model configured" with `exit 1` and
// `degrade with a reason` respectively.
//
// The reason is served from /healthz and logged, so it names the environment
// variable rather than any value: one of those values is the router API key,
// and a health surface is not a place a secret may ever appear.
// It returns the model alongside the runner because the readiness surface has
// to probe the same endpoint this binary would actually call. Building a second
// client for the probe would let the two drift, and a health check that passes
// against a different endpoint than the work uses is worse than none.
func composeAgentRunner(router config.LLMRouter, defaultModel string, tls *transportsecurity.Manager) (*agent.Runner, *llmroutermodel.Model, string, error) {
	if strings.TrimSpace(router.BaseURL) == "" {
		return nil, nil, "knowledge agent not composed: llmrouter has no endpoint, set ATLAS_LLMROUTER_BASE_URL", nil
	}
	llm, err := llmroutermodel.New(llmroutermodel.Config{
		BaseURL: router.BaseURL, APIKey: router.APIKey,
		DefaultModel: defaultModel, Timeout: 120 * time.Second, TLS: tls,
	})
	if err != nil {
		return nil, nil, "", err
	}
	runner, err := agent.NewRunner(llm)
	if err != nil {
		return nil, nil, "", err
	}
	return runner, llm, "", nil
}

// readinessProbes pairs every dependency atlas-agent's health surface NAMES
// with the check that answers for it.
//
// The pairing is the point. /healthz used to publish
// ["postgres","nats","agentnexus","llmrouter"] next to a constant ready:true,
// which reads as four checked results and was four string literals; the names
// now come from this list, so one cannot be published without a check behind
// it. llmrouter is absent when the deployment did not configure it, because a
// name with no check is exactly what this replaces -- the composition gap is
// reported through the not-ready reason instead.
//
// agentnexus matters most here: atlas-agent makes NO call to AgentNexus during
// startup, so a wrong ATLAS_NEXUS_BASE_URL used to stay invisible until the
// first real cross-product request, long after every surface had reported
// itself healthy.
func readinessProbes(pool *pgxpool.Pool, bus *tasks.NATSBus, nexus *nexusclient.HTTPClient, llm *llmroutermodel.Model) []app.DependencyProbe {
	probes := []app.DependencyProbe{
		{Name: "postgres", Check: pool.Ping},
		{Name: "nats", Check: bus.Probe},
		{Name: "agentnexus", Check: nexus.Probe},
	}
	if llm != nil {
		probes = append(probes, app.DependencyProbe{Name: "llmrouter", Check: llm.Probe})
	}
	return probes
}

// tlsManagers is atlas-agent's transport-security composition: one Manager
// per named link this binary participates in (its own server identity plus
// every dependency it dials out to).
type tlsManagers struct {
	agentAtlas *transportsecurity.Manager // server identity (this binary accepts inbound connections)
	agentNexus *transportsecurity.Manager
	llmRouter  *transportsecurity.Manager
	postgres   *transportsecurity.Manager
	nats       *transportsecurity.Manager
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
	if out.nats, err = transportsecurity.NewManager("NATS", transportsecurity.FromLinkTLS(cfg.TLS.NATS)); err != nil {
		return out, fmt.Errorf("nats tls: %w", err)
	}
	return out, nil
}
