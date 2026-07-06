// atlas-agent is the agent-first control plane entrypoint: Knowledge Agent
// runs, workflow drafts, and confirmations. Goal 1 scope: announce identity.
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
		fmt.Fprintln(os.Stderr, "atlas-agent: ", err)
		os.Exit(1)
	}
	logger, err := observability.NewLogger("atlas-agent")
	if err != nil {
		fmt.Fprintln(os.Stderr, "atlas-agent: logger: ", err)
		os.Exit(1)
	}
	defer logger.Sync()

	health := app.NewHealthStatus("atlas-agent",
		"postgres", "nats", "agentnexus", "llmrouter")
	logger.Info("service identity",
		zap.String("version", health.Version),
		zap.Strings("dependencies", health.Dependencies),
		zap.String("llmrouter", cfg.LLMRouter.BaseURL),
	)
}
