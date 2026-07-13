package parsergateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

// ParseResponse is the parser-gateway HTTP contract for POST /v1/parse —
// written by the server (internal/app/parser_server.go) and read by
// HTTPClient, so the two cannot drift.
type ParseResponse struct {
	ProviderID string      `json:"provider_id"`
	LatencyMS  int64       `json:"latency_ms"`
	Output     ParseOutput `json:"output"`
}

// HTTPClient calls a remote parser-gateway service over its /v1/parse
// contract. It satisfies the same Parse surface as the in-process *Gateway,
// so the artifact pipeline swaps between them at the composition root.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

// NewHTTPClient dials the standalone parser-gateway service. tlsMgr
// configures the "parser" link's transport security
// (services/agentatlas/internal/transportsecurity) — this is AgentAtlas
// (atlas-worker) as CLIENT dialing OUT to the parser-gateway server; nil,
// or a Manager built with LinkConfig.Mode == ModeOff, keeps today's
// plaintext behavior.
func NewHTTPClient(baseURL string, tlsMgr *transportsecurity.Manager) (*HTTPClient, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("parser gateway client: base URL required")
	}
	transport := &http.Transport{}
	if tlsMgr != nil {
		if err := tlsMgr.ConfigureTransport(transport); err != nil {
			return nil, fmt.Errorf("parser gateway tls: %w", err)
		}
	}
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		// Parsing large documents/media is slow; generous ceiling, context
		// cancellation still applies.
		http: &http.Client{Timeout: 10 * time.Minute, Transport: transport},
	}, nil
}

// Parse mirrors Gateway.Parse over HTTP.
func (c *HTTPClient) Parse(ctx context.Context, hint string, in ParseInput) (GatewayResult, error) {
	fields := map[string]string{
		"enterprise_id": in.EnterpriseID,
		"artifact_id":   in.ArtifactID,
		"content_type":  in.ContentType,
		"parser_hint":   hint,
	}
	var resp ParseResponse
	if err := postMultipart(ctx, c.http, c.baseURL+"/v1/parse", fields, "file", in.Filename, in.Data, &resp); err != nil {
		return GatewayResult{}, fmt.Errorf("parser gateway: %w", err)
	}
	return GatewayResult{Output: resp.Output, LatencyMS: resp.LatencyMS}, nil
}
