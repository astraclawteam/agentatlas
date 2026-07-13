// parser-gateway is the standalone parse service: POST /v1/parse fans out to
// the Docling/MinerU/ASR/video sidecar providers and returns the normalized
// ParseOutput; /healthz reports per-sidecar reachability.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/app"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "parser-gateway:", err)
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
	logger, err := observability.NewLogger("parser-gateway")
	if err != nil {
		return err
	}
	defer logger.Sync()

	// Transport security: parser-gateway's own server identity (the
	// "gateway" link) — loaded fail-closed, a safe no-op when Mode is off
	// (today's backward-compatible default). The docling/mineru/asr/video
	// sidecar connections this binary makes as a CLIENT are out of this
	// task's scope (see notes.md — "parser" names the atlas-worker ->
	// parser-gateway link, not the gateway -> sidecar links).
	gatewayTLS, err := transportsecurity.NewManager("gateway", transportsecurity.FromLinkTLS(cfg.TLS.Gateway))
	if err != nil {
		return fmt.Errorf("gateway server tls: %w", err)
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

	metrics := observability.NewMetrics()
	shutdownTracing, err := observability.InitTracing(ctx, "parser-gateway")
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	gateway := parsergateway.NewGateway(registry)
	gateway.SetMetrics(metrics)
	router := app.NewParserRouter(app.ParserRouterDeps{
		Gateway: gateway,
		Sidecars: map[string]string{
			"docling": cfg.Parsers.Docling.BaseURL,
			"mineru":  cfg.Parsers.MinerU.BaseURL,
			"asr":     cfg.Parsers.ASR.BaseURL,
			"video":   cfg.Parsers.Video.BaseURL,
		},
		Metrics: metrics,
	})

	addr := os.Getenv("ATLAS_PARSER_GATEWAY_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	logger.Info("parser-gateway serving",
		zap.String("addr", addr),
		zap.String("version", app.Version),
		zap.String("docling", cfg.Parsers.Docling.BaseURL),
		zap.String("mineru", cfg.Parsers.MinerU.BaseURL),
		zap.String("asr", cfg.Parsers.ASR.BaseURL),
		zap.String("video", cfg.Parsers.Video.BaseURL),
		zap.String("tls_mode", string(cfg.TLS.Gateway.Mode)),
	)
	server := &http.Server{Addr: addr, Handler: router, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	ln, err = gatewayTLS.WrapListener(ln)
	if err != nil {
		return fmt.Errorf("gateway server tls: %w", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("parser-gateway shutting down")
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
