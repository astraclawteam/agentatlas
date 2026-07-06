package parsergateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Gateway resolves a provider and executes the parse with timing so callers
// can record parser_provider_runs.
type Gateway struct {
	registry *Registry
}

func NewGateway(registry *Registry) *Gateway {
	return &Gateway{registry: registry}
}

type GatewayResult struct {
	Output    ParseOutput
	LatencyMS int64
}

func (g *Gateway) Parse(ctx context.Context, hint string, in ParseInput) (GatewayResult, error) {
	provider, err := g.registry.Resolve(hint, in.ContentType)
	if err != nil {
		return GatewayResult{}, err
	}
	max := int64(provider.Descriptor().MaxFileSizeMB) * 1024 * 1024
	if max > 0 && int64(len(in.Data)) > max {
		return GatewayResult{}, fmt.Errorf("artifact %s exceeds provider %s limit (%d MB)",
			in.ArtifactID, provider.Descriptor().ProviderID, provider.Descriptor().MaxFileSizeMB)
	}
	start := time.Now()
	out, err := provider.Parse(ctx, in)
	if err != nil {
		return GatewayResult{}, fmt.Errorf("provider %s: %w", provider.Descriptor().ProviderID, err)
	}
	out.ProviderID = provider.Descriptor().ProviderID
	return GatewayResult{Output: out, LatencyMS: time.Since(start).Milliseconds()}, nil
}

func (g *Gateway) Capabilities() []map[string]any {
	caps := g.registry.Capabilities()
	out := make([]map[string]any, 0, len(caps))
	for _, c := range caps {
		raw, _ := json.Marshal(c)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

// --- shared HTTP helpers for sidecar clients --------------------------------

func postMultipart(ctx context.Context, client *http.Client, url string, fields map[string]string, fileField, filename string, data []byte, out any) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return err
		}
	}
	part, err := w.CreateFormFile(fileField, filename)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return doJSON(client, req, out)
}

func postJSON(ctx context.Context, client *http.Client, url string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(client, req, out)
}

func doJSON(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: status %d: %s", req.URL.Path, resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
