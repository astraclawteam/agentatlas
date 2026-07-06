package nexusclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// HTTPClient implements nexus.Client against api/openapi/agentnexus-client.yaml.
type HTTPClient struct {
	baseURL string
	http    *http.Client
}

var _ nexus.Client = (*HTTPClient)(nil)

func New(baseURL string, timeout time.Duration) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *HTTPClient) post(ctx context.Context, path string, in, out any) error {
	ctx, span := observability.Tracer("nexusclient").Start(ctx, "nexus"+strings.ReplaceAll(path, "/", "."))
	defer span.End()
	err := c.doPost(ctx, path, in, out)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

func (c *HTTPClient) doPost(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("nexus %s: encode: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("nexus %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("nexus %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("nexus %s: %w", path, ErrDenied)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("nexus %s: status %d: %s", path, resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("nexus %s: decode: %w", path, err)
	}
	return nil
}

func (c *HTTPClient) VerifyTicket(ctx context.Context, req nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	var out nexus.VerifyTicketResponse
	err := c.post(ctx, "/v1/tickets/verify", req, &out)
	return out, err
}

func (c *HTTPClient) LocateEvidence(ctx context.Context, req nexus.LocateEvidenceRequest) (nexus.LocateEvidenceResponse, error) {
	var out nexus.LocateEvidenceResponse
	err := c.post(ctx, "/v1/evidence/locate", req, &out)
	return out, err
}

func (c *HTTPClient) ReadEvidence(ctx context.Context, req nexus.ReadEvidenceRequest) (nexus.ReadEvidenceResponse, error) {
	var out nexus.ReadEvidenceResponse
	err := c.post(ctx, "/v1/evidence/read", req, &out)
	return out, err
}

func (c *HTTPClient) AppendAuditEvidence(ctx context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	var out nexus.AppendAuditEvidenceResponse
	err := c.post(ctx, "/v1/audit/evidence", req, &out)
	return out, err
}

// SubscribeOrgEvents consumes the SSE stream. It returns nil when the server
// closes the stream cleanly, ctx.Err() on cancellation, and the handler's
// error if event processing fails (so the caller can resume from the last
// processed org_version).
func (c *HTTPClient) SubscribeOrgEvents(ctx context.Context, enterpriseID string, sinceVersion int64, handler nexus.OrgEventHandler) error {
	q := url.Values{}
	q.Set("enterprise_id", enterpriseID)
	q.Set("since_version", strconv.FormatInt(sinceVersion, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/org-events?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("nexus org-events: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	// The stream is long-lived; bypass the client-level timeout.
	streaming := &http.Client{Transport: c.http.Transport}
	resp, err := streaming.Do(req)
	if err != nil {
		return fmt.Errorf("nexus org-events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("nexus org-events: status %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev nexus.OrgEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return fmt.Errorf("nexus org-events: decode: %w", err)
		}
		if err := handler(ctx, ev); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("nexus org-events: stream: %w", err)
	}
	return nil
}
