// atlas-worker executes asynchronous work: dream jobs, index jobs, artifact
// processing, and workflow runs. Goal 1 scope: announce identity cleanly.
package main

import (
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

func main() {
	cfg, err := config.Load(os.Getenv("ATLAS_CONFIG"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "atlas-worker: ", err)
		os.Exit(1)
	}
	logger, err := observability.NewLogger("atlas-worker")
	if err != nil {
		fmt.Fprintln(os.Stderr, "atlas-worker: logger: ", err)
		os.Exit(1)
	}
	defer logger.Sync()

	health := app.NewHealthStatus("atlas-worker",
		"postgres", "opensearch", "nats", "object-storage", "parser-gateway", "llmrouter")
	logger.Info("service identity",
		zap.String("version", health.Version),
		zap.Strings("dependencies", health.Dependencies),
		zap.String("nats", cfg.NATS.URL),
	)
}
