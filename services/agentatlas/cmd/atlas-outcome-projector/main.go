// atlas-outcome-projector is the Task 0I Outcome Graph projector service. It
// consumes the ONE shared projection outbox (outcome_graph_outbox) from the
// authoritative PostgreSQL, applies each tenant's committed domain facts to the
// Apache AGE read model via idempotent parameterized Cypher MERGE, and advances a
// per-tenant watermark only after the AGE write commits.
//
// It is a PURE consumer of authoritative state: it never writes authoritative
// PostgreSQL and never receives a direct graph write that did not originate from
// the outbox. If AGE is unavailable it degrades EXPLICITLY (no watermark
// advance, clear error) while authoritative mutation and already-approved
// execution continue unaffected; on recovery it catches up from the last
// committed watermark with no duplicate edges.
//
// The AGE connection is a SEPARATELY CONFIGURED endpoint (its own DSN,
// ATLAS_OUTCOME_GRAPH_DSN), independent from the authoritative WorkCase Postgres.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcomegraph"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "atlas-outcome-projector:", err)
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
	logger, err := observability.NewLogger("atlas-outcome-projector")
	if err != nil {
		return err
	}
	defer logger.Sync()

	// The graph is a separately-configured endpoint (its own DSN). Fail closed if
	// it is not configured: without the graph read model the projector has no job.
	graphDSN := strings.TrimSpace(os.Getenv("ATLAS_OUTCOME_GRAPH_DSN"))
	if graphDSN == "" {
		return fmt.Errorf("ATLAS_OUTCOME_GRAPH_DSN is required (the Apache AGE graph endpoint)")
	}

	pgTLS, err := transportsecurity.NewManager("PostgreSQL", transportsecurity.FromLinkTLS(cfg.TLS.Postgres))
	if err != nil {
		return fmt.Errorf("postgres tls: %w", err)
	}
	natsTLS, err := transportsecurity.NewManager("NATS", transportsecurity.FromLinkTLS(cfg.TLS.NATS))
	if err != nil {
		return fmt.Errorf("nats tls: %w", err)
	}

	// Authoritative PostgreSQL: the projector reads the shared outbox from here.
	if err := storage.Migrate(ctx, cfg.Postgres.DSN); err != nil {
		return err
	}
	pool, err := storage.NewPool(ctx, cfg.Postgres.DSN, pgTLS)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Apache AGE graph (separately configured DSN).
	graphName := strings.TrimSpace(os.Getenv("ATLAS_OUTCOME_GRAPH_NAME"))
	if graphName == "" {
		graphName = outcomegraph.DefaultGraphName
	}
	ageStore, err := outcomegraph.NewAGEStore(ctx, graphDSN, graphName)
	if err != nil {
		return fmt.Errorf("apache age: %w", err)
	}
	defer ageStore.Close()

	projector := outcomegraph.NewProjector(ageStore, outcomegraph.NewPostgresOutbox(pool))

	// NATS wakeups reduce projection latency; the periodic ProjectAll loop is the
	// reliable path (the outbox is the source of truth, NATS is only a nudge).
	bus, err := tasks.NewNATSBus(ctx, cfg.NATS.URL, natsTLS)
	if err != nil {
		return fmt.Errorf("nats (production-standard dependency): %w", err)
	}
	unsub, err := projector.Subscribe(ctx, bus)
	if err != nil {
		return fmt.Errorf("subscribe projection wakeups: %w", err)
	}
	defer unsub()

	tickSeconds := 15
	if v := strings.TrimSpace(os.Getenv("ATLAS_OUTCOME_PROJECTOR_TICK_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tickSeconds = n
		}
	}
	// Initial catch-up so a fresh/recovered projector converges immediately.
	if err := projector.ProjectAll(ctx); err != nil {
		logger.Warn("initial projection catch-up (degraded until AGE reachable)", zap.Error(err))
	}
	go func() {
		ticker := time.NewTicker(time.Duration(tickSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := projector.ProjectAll(ctx); err != nil {
					logger.Warn("projection catch-up (graph-dependent planning degrades explicitly)", zap.Error(err))
				}
			}
		}
	}()

	logger.Info("atlas-outcome-projector running",
		zap.String("nats", cfg.NATS.URL),
		zap.String("graph", graphName),
		zap.Int("tick_seconds", tickSeconds),
		zap.String("provider", outcomegraph.ProviderName),
		zap.String("age_release", outcomegraph.AGERelease))

	<-ctx.Done()
	logger.Info("atlas-outcome-projector draining")
	return nil
}
