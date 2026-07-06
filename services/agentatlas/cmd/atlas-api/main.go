// atlas-api is the stable runtime entrypoint: answer API, ingestion API,
// space queries, and trace queries. Goal 1 scope: announce identity cleanly.
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
		fmt.Fprintln(os.Stderr, "atlas-api: ", err)
		os.Exit(1)
	}
	logger, err := observability.NewLogger("atlas-api")
	if err != nil {
		fmt.Fprintln(os.Stderr, "atlas-api: logger: ", err)
		os.Exit(1)
	}
	defer logger.Sync()

	health := app.NewHealthStatus("atlas-api",
		"postgres", "opensearch", "nats", "object-storage", "agentnexus", "llmrouter")
	logger.Info("service identity",
		zap.String("version", health.Version),
		zap.Strings("dependencies", health.Dependencies),
		zap.String("agentnexus", cfg.AgentNexus.BaseURL),
	)
}
