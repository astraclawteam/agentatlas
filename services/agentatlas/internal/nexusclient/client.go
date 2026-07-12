package nexusclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// HTTPClient implements nexus.Client against api/openapi/agentnexus-client.yaml.
type HTTPClient struct {
	baseURL             string
	http                *http.Client
	serviceClientID     string
	serviceSecret       string
	approvalFactsSecret string
}

// ConfigureApprovalFactsSecret loads the dedicated HMAC key shared only with
// the AgentNexus approval verifier. It must not reuse the service credential.
func (c *HTTPClient) ConfigureApprovalFactsSecret(path string) error {
	secret, err := loadServiceSecretFile(path)
	if err != nil {
		return fmt.Errorf("load AgentNexus approval facts credential: %w", err)
	}
	c.approvalFactsSecret = secret
	return nil
}

var _ nexus.Client = (*HTTPClient)(nil)
var _ nexus.ApprovalClient = (*HTTPClient)(nil)

func (c *HTTPClient) String() string {
	if c == nil {
		return "HTTPClient<nil>"
	}
	return fmt.Sprintf("HTTPClient{baseURL:%q, serviceClientID:%q, serviceSecret:[REDACTED]}", c.baseURL, c.serviceClientID)
}

func (c *HTTPClient) GoString() string { return c.String() }

func New(baseURL string, timeout time.Duration, serviceClientID, serviceSecretFile string) (*HTTPClient, error) {
	if strings.TrimSpace(baseURL) == "" || timeout <= 0 || serviceClientID != "agentatlas" {
		return nil, fmt.Errorf("invalid AgentNexus client configuration")
	}
	serviceSecret, err := loadServiceSecretFile(serviceSecretFile)
	if err != nil {
		return nil, fmt.Errorf("load AgentNexus service credential: %w", err)
	}
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		serviceClientID: serviceClientID,
		serviceSecret:   serviceSecret,
	}, nil
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
	if path == "/v1/audit/evidence" {
		req.SetBasicAuth(c.serviceClientID, c.serviceSecret)
		if auditReq, ok := in.(nexus.AppendAuditEvidenceRequest); ok && auditReq.IdempotencyKey != "" {
			req.Header.Set("Idempotency-Key", auditReq.IdempotencyKey)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("nexus %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("nexus %s: %w", path, ErrDenied)
	}
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("nexus %s: %w", path, ErrConflict)
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

func loadServiceSecretFile(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("secret path must be canonical and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret must be a regular non-symlink file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file permissions are too broad")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := string(data)
	if len(secret) < 32 || len(secret) > 256 {
		return "", fmt.Errorf("secret must contain 32 to 256 bytes")
	}
	seen := map[byte]struct{}{}
	for i := 0; i < len(secret); i++ {
		if secret[i] < 0x21 || secret[i] > 0x7e {
			return "", fmt.Errorf("secret must be printable ASCII without whitespace")
		}
		seen[secret[i]] = struct{}{}
	}
	if len(seen) < 12 {
		return "", fmt.Errorf("secret does not meet minimum entropy diversity")
	}
	return secret, nil
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

func (c *HTTPClient) ResolveApprovalRoute(ctx context.Context, input nexus.ApprovalResolveRequest) (nexus.ApprovalRoute, error) {
	var out nexus.ApprovalRoute
	if len(input.IdempotencyKey) < 16 || input.TicketID == "" || input.EnterpriseID == "" || input.ActorUserID == "" {
		return out, fmt.Errorf("nexus approval: incomplete bound request")
	}
	attestation, err := approvalFactsAttestation(c.approvalFactsSecret, input)
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/approvals/resolve", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Case-Ticket", input.TicketID)
	req.Header.Set("Idempotency-Key", input.IdempotencyKey)
	req.Header.Set("X-Approval-Facts-Attestation", attestation)
	resp, err := c.http.Do(req)
	if err != nil {
		return out, fmt.Errorf("nexus approval: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return out, fmt.Errorf("nexus approval: status %d: %s", resp.StatusCode, snippet)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("nexus approval: decode: %w", err)
	}
	if out.AutoPublish {
		return out, fmt.Errorf("nexus approval violated no-auto-publish contract")
	}
	return out, nil
}

func approvalFactsAttestation(secret string, input nexus.ApprovalResolveRequest) (string, error) {
	if len(secret) < 32 {
		return "", fmt.Errorf("nexus approval facts secret unavailable")
	}
	keyHash := sha256.Sum256([]byte(input.IdempotencyKey))
	changed := append([]string{}, input.ChangedFields...)
	impacted := append([]string{}, input.ImpactedOrgUnitIDs...)
	sort.Strings(changed)
	sort.Strings(impacted)
	payload := struct {
		EnterpriseID            string    `json:"enterprise_id"`
		ActorUserID             string    `json:"actor_user_id"`
		OrgVersion              int64     `json:"org_version"`
		OrgUnitID               string    `json:"org_unit_id"`
		ResourceType            string    `json:"resource_type"`
		ResourceID              string    `json:"resource_id"`
		Action                  string    `json:"action"`
		ChangedFields           []string  `json:"changed_fields"`
		ImpactedOrgUnitIDs      []string  `json:"impacted_org_unit_ids"`
		ImpactedUserCount       int       `json:"impacted_user_count"`
		PublishedBehaviorChange bool      `json:"published_behavior_change"`
		ExternalSideEffect      bool      `json:"external_side_effect"`
		FactsIssuedAt           time.Time `json:"facts_issued_at"`
		FactsExpiresAt          time.Time `json:"facts_expires_at"`
		FactsNonce              string    `json:"facts_nonce"`
		IdempotencyKeyHash      string    `json:"idempotency_key_hash"`
	}{input.EnterpriseID, input.ActorUserID, input.OrgVersion, input.OrgUnitID, input.ResourceType, input.ResourceID, input.Action, changed, impacted, input.ImpactedUserCount, input.PublishedBehaviorChange, input.ExternalSideEffect, input.FactsIssuedAt.UTC(), input.FactsExpiresAt.UTC(), input.FactsNonce, hex.EncodeToString(keyHash[:])}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
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
