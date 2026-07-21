package nexusclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

// HTTPClient implements nexus.Client against api/openapi/agentnexus-client.yaml.
type HTTPClient struct {
	baseURL         string
	http            *http.Client
	serviceClientID string
	serviceSecret   string

	// actionSigningKeyID/actionTrustedKey are the trusted AgentNexus
	// ed25519 signing identity configured via ConfigureActionSigningKey.
	// grantTracker enforces one-use ActionGrant replay protection across
	// RequestAction calls. All three are nil-safe zero values until
	// configured: an unconfigured client's Action-surface methods reject
	// every receipt/grant (see verifySignature/VerifyActionGrant), never
	// silently accept one.
	actionSigningKeyID string
	actionTrustedKey   ed25519.PublicKey
	grantTracker       *nexus.GrantUsageTracker
}

// ConfigureApprovalFactsSecret loads the dedicated HMAC key shared only with
// the AgentNexus approval verifier. It must not reuse the service credential.
var _ nexus.Client = (*HTTPClient)(nil)
var _ nexus.ApprovalClient = (*HTTPClient)(nil)
var _ nexus.BrowserBFFClient = (*HTTPClient)(nil)
var _ nexus.GovernanceClient = (*HTTPClient)(nil)
var _ nexus.ActionClient = (*HTTPClient)(nil)

func (c *HTTPClient) String() string {
	if c == nil {
		return "HTTPClient<nil>"
	}
	return fmt.Sprintf("HTTPClient{baseURL:%q, serviceClientID:%q, serviceSecret:[REDACTED]}", c.baseURL, c.serviceClientID)
}

func (c *HTTPClient) GoString() string { return c.String() }

// New builds the AgentNexus HTTP client. tlsMgr configures the AgentNexus
// link's transport security (services/agentatlas/internal/transportsecurity);
// pass a Manager built with LinkConfig.Mode == ModeOff (or nil) to keep
// today's plaintext behavior — ConfigureTransport is then a no-op.
func New(baseURL string, timeout time.Duration, serviceClientID, serviceSecretFile string, tlsMgr *transportsecurity.Manager) (*HTTPClient, error) {
	if strings.TrimSpace(baseURL) == "" || timeout <= 0 || serviceClientID != "agentatlas" {
		return nil, fmt.Errorf("invalid AgentNexus client configuration")
	}
	serviceSecret, err := loadServiceSecretFile(serviceSecretFile)
	if err != nil {
		return nil, fmt.Errorf("load AgentNexus service credential: %w", err)
	}
	transport := &http.Transport{}
	if tlsMgr != nil {
		if err := tlsMgr.ConfigureTransport(transport); err != nil {
			return nil, fmt.Errorf("agentnexus tls: %w", err)
		}
	}
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		serviceClientID: serviceClientID,
		serviceSecret:   serviceSecret,
		// grantTracker is always initialized (not only once
		// ConfigureActionSigningKey is called): ActionGrant replay
		// protection is orthogonal to signature-key configuration — a
		// grant carries no signature of its own — and must never silently
		// no-op just because a caller forgot the separate action-signing
		// configuration step.
		grantTracker: nexus.NewGrantUsageTracker(),
	}, nil
}

// ConfigureActionSigningKey registers the trusted AgentNexus ed25519 public
// key used to verify ActionReceipt/ObservationReceipt signatures on the
// governed Action Request/Grant/Receipt protocol (GA Task 0D). It follows
// the same post-construction configuration pattern as
// ConfigureApprovalFactsSecret rather than an added New parameter, so
// New's signature — including the *transportsecurity.Manager parameter
// Task 13A added — is unchanged. Until this is called, RequestAction still
// enforces expiry/replay/actor-org/capability/parameter-hash binding on
// ActionGrant (which carries no signature), but FetchActionReceipt and
// FetchObservationReceipt reject every receipt: an unconfigured (nil/empty)
// trusted key never has the length verifySignature requires, so the client
// fails closed rather than silently skipping the recheck.
func (c *HTTPClient) ConfigureActionSigningKey(keyID string, publicKey ed25519.PublicKey) error {
	if strings.TrimSpace(keyID) == "" || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid AgentNexus action signing key configuration")
	}
	c.actionSigningKeyID = keyID
	c.actionTrustedKey = publicKey
	return nil
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
	if path == "/v1/audit/evidence" || path == "/v1/authorization/decisions" || path == "/v1/tickets/verify" ||
		path == "/v1/actions/request" || path == "/v1/actions/receipt" || path == "/v1/actions/observation" {
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

// actionReceiptFetchRequest and observationReceiptFetchRequest are the
// minimal wire-identifying bodies for the two fetch endpoints: AgentNexus
// already owns the full receipt once issued, so the client need only name
// which one it wants. These are transport plumbing, not part of the nexus
// SDK's Action protocol surface (which stops at ActionRequest/ActionGrant/
// ActionReceipt/ObservationReceipt) — kept private to this file.
type actionReceiptFetchRequest struct {
	ActionID string `json:"action_id"`
}

type observationReceiptFetchRequest struct {
	ActionID           string `json:"action_id"`
	VerificationNeedID string `json:"verification_need_id"`
}

// RequestAction implements nexus.ActionClient. It rejects a structurally
// invalid ActionRequest before any network call (see
// nexus.ActionRequest.Validate — missing idempotency key, malformed
// parameter hash, detached postcondition/verification-need binding, and so
// on), sends ONLY the governed ActionRequest (never a connector-specific
// body), and rechecks the returned ActionGrant with nexus.VerifyActionGrant
// — exact WorkCase/actor/org/capability/parameter-hash binding, one-use
// semantics, non-expiry and (via the client's internal GrantUsageTracker)
// replay protection — before returning it to the caller. An expired,
// replayed or actor/org-mismatched grant from the wire is never returned as
// success.
func (c *HTTPClient) RequestAction(ctx context.Context, req nexus.ActionRequest) (nexus.ActionGrant, error) {
	var grant nexus.ActionGrant
	if err := req.Validate(); err != nil {
		return grant, fmt.Errorf("nexus action request: %w", err)
	}
	if err := c.post(ctx, "/v1/actions/request", req, &grant); err != nil {
		return grant, err
	}
	if err := nexus.VerifyActionGrant(req, grant, c.grantTracker, time.Now()); err != nil {
		return nexus.ActionGrant{}, err
	}
	return grant, nil
}

// FetchActionReceipt implements nexus.ActionClient. It retrieves the signed
// ActionReceipt AgentNexus issued for req.ActionID and rechecks it with
// nexus.VerifyActionReceipt (exact Action/parameter-hash binding plus a
// signature that verifies against the trusted key configured by
// ConfigureActionSigningKey) before returning it. An unsigned, wrong-key,
// detached or parameter-hash-mismatched receipt from the wire is never
// returned as success.
func (c *HTTPClient) FetchActionReceipt(ctx context.Context, req nexus.ActionRequest) (nexus.ActionReceipt, error) {
	var receipt nexus.ActionReceipt
	// Validate the request up front (mirrors RequestAction) so a garbage
	// req.ActionID/parameter_hash is rejected before any network call and
	// before it is used as the binding baseline for the receipt recheck.
	if err := req.Validate(); err != nil {
		return receipt, fmt.Errorf("nexus action request: %w", err)
	}
	if err := c.post(ctx, "/v1/actions/receipt", actionReceiptFetchRequest{ActionID: req.ActionID}, &receipt); err != nil {
		return receipt, err
	}
	if err := nexus.VerifyActionReceipt(req, receipt, c.actionSigningKeyID, c.actionTrustedKey); err != nil {
		return nexus.ActionReceipt{}, err
	}
	return receipt, nil
}

// FetchObservationReceipt implements nexus.ActionClient. It retrieves the
// signed ObservationReceipt confirming verificationNeedID for req.ActionID
// and rechecks it with nexus.VerifyObservationReceipt (exact Action/
// parameter-hash binding, that the referenced PostconditionSpec and
// VerificationNeed were actually declared on req, plus a signature that
// verifies against the trusted key) before returning it. An
// ObservationReceipt detached from its Action or PostconditionSpec, or one
// that is unsigned or wrong-key-signed, is never returned as success.
func (c *HTTPClient) FetchObservationReceipt(ctx context.Context, req nexus.ActionRequest, verificationNeedID string) (nexus.ObservationReceipt, error) {
	var obs nexus.ObservationReceipt
	// Validate the request up front (mirrors RequestAction) so a garbage
	// req is rejected before any network call and before it is used as the
	// binding baseline for the observation recheck.
	if err := req.Validate(); err != nil {
		return obs, fmt.Errorf("nexus action request: %w", err)
	}
	fetchReq := observationReceiptFetchRequest{ActionID: req.ActionID, VerificationNeedID: verificationNeedID}
	if err := c.post(ctx, "/v1/actions/observation", fetchReq, &obs); err != nil {
		return obs, err
	}
	if err := nexus.VerifyObservationReceipt(req, obs, c.actionSigningKeyID, c.actionTrustedKey); err != nil {
		return nexus.ObservationReceipt{}, err
	}
	return obs, nil
}

func (c *HTTPClient) VerifyTicket(ctx context.Context, req nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	var out nexus.VerifyTicketResponse
	err := c.post(ctx, "/v1/tickets/verify", req, &out)
	return out, err
}

func (c *HTTPClient) AppendAuditEvidence(ctx context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	var out nexus.AppendAuditEvidenceResponse
	err := c.post(ctx, "/v1/audit/evidence", req, &out)
	return out, err
}

func (c *HTTPClient) AuthorizeBrowserOperation(ctx context.Context, accessToken string, input nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	var out nexus.BrowserAuthorizationDecision
	err := c.bearerPost(ctx, "/v1/authorization/decisions", accessToken, "", input, &out, nil)
	return out, err
}
func (c *HTTPClient) AuthorizeTicketOperation(ctx context.Context, ticketID string, input nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	var out nexus.BrowserAuthorizationDecision
	if ticketID == "" {
		return out, fmt.Errorf("nexus ticket authorization: missing CaseTicket")
	}
	input.TicketID = ticketID
	if err := c.post(ctx, "/v1/authorization/decisions", input, &out); err != nil {
		return out, err
	}
	return out, nil
}
func (c *HTTPClient) AppendAuditEvidenceWithBearer(ctx context.Context, accessToken string, input nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	var out nexus.AppendAuditEvidenceResponse
	err := c.bearerPost(ctx, "/v1/audit/evidence", accessToken, input.IdempotencyKey, input, &out, nil)
	return out, err
}

func (c *HTTPClient) bearerPost(ctx context.Context, path, accessToken, idempotency string, in, out any, headers map[string]string) error {
	if len(accessToken) < 16 {
		return fmt.Errorf("nexus %s: missing BFF credential", path)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("nexus %s: %w", path, ErrDenied)
	}
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("nexus %s: %w", path, ErrConflict)
	}
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("nexus %s: status %d: %s", path, resp.StatusCode, snippet)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
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
