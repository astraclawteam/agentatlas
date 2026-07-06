// parser-gateway schedules parser sidecars (Docling, MinerU, ASR, video) and
// normalizes their output into AtlasDocument. Goal 1 scope: announce identity.
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
		fmt.Fprintln(os.Stderr, "parser-gateway: ", err)
		os.Exit(1)
	}
	logger, err := observability.NewLogger("parser-gateway")
	if err != nil {
		fmt.Fprintln(os.Stderr, "parser-gateway: logger: ", err)
		os.Exit(1)
	}
	defer logger.Sync()

	health := app.NewHealthStatus("parser-gateway",
		"docling", "mineru", "asr", "video", "object-storage")
	logger.Info("service identity",
		zap.String("version", health.Version),
		zap.Strings("dependencies", health.Dependencies),
		zap.String("docling", cfg.Parsers.Docling.BaseURL),
		zap.String("mineru", cfg.Parsers.MinerU.BaseURL),
		zap.String("asr", cfg.Parsers.ASR.BaseURL),
		zap.String("video", cfg.Parsers.Video.BaseURL),
	)
}
