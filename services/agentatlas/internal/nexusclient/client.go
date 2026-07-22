package nexusclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
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
// a post-construction configuration pattern rather than an added New parameter, so
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
		path == "/v1/approvals/transmissions" || path == runtimeActPath {
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

// get is doPost's read-only twin for the frozen surfaces that are GETs rather
// than POSTs. It carries the same service credential and the same no-follow
// redirect guard the client was built with.
func (c *HTTPClient) get(ctx context.Context, path string, out any) error {
	ctx, span := observability.Tracer("nexusclient").Start(ctx, "nexus"+strings.ReplaceAll(path, "/", "."))
	defer span.End()
	err := c.doGet(ctx, path, out)
	if err != nil {
		span.RecordError(err)
	}
	return err
}

func (c *HTTPClient) doGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("nexus %s: %w", path, err)
	}
	req.SetBasicAuth(c.serviceClientID, c.serviceSecret)
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

// The frozen Action surfaces. /v1/actions/request, /v1/actions/receipt and
// /v1/actions/observation — the paths this client used to POST to — appear
// nowhere in AgentNexus's contract and could only ever 404.
//
// runtimeReceiptPath is a TEMPLATE: the frozen receipt surface is addressed by
// an opaque ReceiptRef, not by an action id. That handle is minted by
// AgentNexus and travels back on the Action returned from /v1/runtime/act, so
// the caller has to carry it here — see FetchActionReceipt.
const (
	runtimeActPath          = "/v1/runtime/act"
	runtimeReceiptPathTmpl  = "/v1/runtime/receipts/{receipt_ref}"
	runtimeReceiptPathParam = "{receipt_ref}"
)

func runtimeReceiptPath(receiptRef string) string {
	return strings.Replace(runtimeReceiptPathTmpl, runtimeReceiptPathParam, url.PathEscape(receiptRef), 1)
}

// ErrNoFrozenSurface reports a governed-Action operation AgentNexus has not
// frozen an endpoint for. Failing closed here is the point: the alternative is
// a request to an invented path, which is indistinguishable from an outage at
// the call site and silently rots as the contract moves.
var ErrNoFrozenSurface = errors.New("no frozen AgentNexus surface for this operation")

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
	if err := c.post(ctx, runtimeActPath, req, &grant); err != nil {
		return grant, err
	}
	if err := nexus.VerifyActionGrant(req, grant, c.grantTracker, time.Now()); err != nil {
		return nexus.ActionGrant{}, err
	}
	return grant, nil
}

// FetchActionReceipt implements nexus.ActionClient. It GETs the frozen receipt
// surface for receiptRef and rechecks the result with
// nexus.VerifyActionReceipt (exact Action/parameter-hash binding plus a
// signature that verifies against the trusted key configured by
// ConfigureActionSigningKey) before returning it. An unsigned, wrong-key,
// detached or parameter-hash-mismatched receipt from the wire is never
// returned as success.
//
// receiptRef is the opaque handle AgentNexus mints and returns on the Action
// from /v1/runtime/act; the frozen surface has no by-action-id lookup, so a
// caller without a handle has nothing to fetch and is refused here rather than
// having req.ActionID passed off as a receipt handle.
func (c *HTTPClient) FetchActionReceipt(ctx context.Context, req nexus.ActionRequest, receiptRef string) (nexus.ActionReceipt, error) {
	var receipt nexus.ActionReceipt
	// Validate the request up front (mirrors RequestAction) so a garbage
	// req.ActionID/parameter_hash is rejected before any network call and
	// before it is used as the binding baseline for the receipt recheck.
	if err := req.Validate(); err != nil {
		return receipt, fmt.Errorf("nexus action request: %w", err)
	}
	if strings.TrimSpace(receiptRef) == "" {
		return receipt, fmt.Errorf("nexus %s: missing receipt handle", runtimeReceiptPathTmpl)
	}
	if err := c.get(ctx, runtimeReceiptPath(receiptRef), &receipt); err != nil {
		return receipt, err
	}
	if err := nexus.VerifyActionReceipt(req, receipt, c.actionSigningKeyID, c.actionTrustedKey); err != nil {
		return nexus.ActionReceipt{}, err
	}
	return receipt, nil
}

// FetchObservationReceipt implements nexus.ActionClient, and is DORMANT.
//
// AgentNexus's frozen contract has no observation-read surface at all: the only
// receipt surface is GET /v1/runtime/receipts/{receipt_ref}, which returns an
// ActionReceipt. The retired POST /v1/actions/observation was never an
// AgentNexus endpoint — it was AgentAtlas's own agreed-spec placeholder pending
// AgentNexus's freeze (see the ObservationReceipt note in
// api/openapi/agentnexus-client.yaml), and it could only 404.
//
// Failing closed here keeps that visible. The recheck this method used to wire
// up, nexus.VerifyObservationReceipt, is unchanged and independently covered in
// sdk/go/nexus/action_test.go, so nothing about ObservationReceipt validation is
// lost — only the transport, which does not exist to test.
func (c *HTTPClient) FetchObservationReceipt(context.Context, nexus.ActionRequest, string) (nexus.ObservationReceipt, error) {
	return nexus.ObservationReceipt{}, fmt.Errorf("nexus observation receipt: %w", ErrNoFrozenSurface)
}

func (c *HTTPClient) VerifyTicket(ctx context.Context, req nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	var out nexus.VerifyTicketResponse
	err := c.post(ctx, "/v1/tickets/verify", req, &out)
	return out, err
}

// ErrAuditContextUnbound reports an audit append that has no
// business_context_ref to bind. The frozen AuditEvidenceRequest requires one
// and AgentNexus resolves it with VerifyAccessTicket for every credential
// source, so an unbound append can only ever be answered 401 invalid_ticket.
// Failing here keeps that a named local error instead of a remote rejection
// the caller has to decode.
var ErrAuditContextUnbound = errors.New("audit append has no business_context_ref to bind")

func requireAuditContext(req nexus.AppendAuditEvidenceRequest) error {
	if strings.TrimSpace(req.BusinessContextRef) == "" {
		return fmt.Errorf("nexus /v1/audit/evidence: %w (action %q, resource %s/%s)",
			ErrAuditContextUnbound, req.Action, req.ResourceType, req.ResourceID)
	}
	return nil
}

func (c *HTTPClient) AppendAuditEvidence(ctx context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	var out nexus.AppendAuditEvidenceResponse
	if err := requireAuditContext(req); err != nil {
		return out, err
	}
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
	// A browser token does NOT exempt the append from carrying a business
	// context: AgentNexus verifies the ref as an access ticket and then
	// requires the token's principal to match it. See browserAuditContextRef
	// in internal/app for why no browser session can satisfy this yet.
	if err := requireAuditContext(input); err != nil {
		return out, err
	}
	err := c.bearerPost(ctx, "/v1/audit/evidence", accessToken, input.IdempotencyKey, input, &out, nil)
	return out, err
}

func (c *HTTPClient) bearerPost(ctx context.Context, path, accessToken, idempotency string, in, out any, headers map[string]string) error {
	if len(accessToken) < 16 {
		return fmt.Errorf("nexus %s: missing BFF credential", path)
	}
	return c.actorPost(ctx, path, "Bearer "+accessToken, idempotency, in, out, headers)
}

// ErrMissingCaseTicket reports a per-actor call with no Access Ticket to
// present. The frozen evidence surface accepts only browserSession,
// browserAccessToken or caseTicket — deliberately NOT trustedServiceSecret —
// so a caller with no ticket has no credential it is allowed to send. Failing
// here keeps that a named local error rather than an unauthenticated request
// that a real AgentNexus can only answer 401.
var ErrMissingCaseTicket = errors.New("no Access Ticket to present on a per-actor surface")

// ticketPost is bearerPost's Access Ticket twin. The pinned contract defines
// the caseTicket scheme as an apiKey in the Authorization header with the
// exact format "CaseTicket <opaque>", so the ticket rides the same header the
// bearer token does under a different scheme — it is NOT an X-Case-Ticket
// header, and it never travels in the body, where it would be a caller-
// supplied identity claim of exactly the kind the frozen contract removed.
func (c *HTTPClient) ticketPost(ctx context.Context, path, ticketID, idempotency string, in, out any, headers map[string]string) error {
	if strings.TrimSpace(ticketID) == "" {
		return fmt.Errorf("nexus %s: %w", path, ErrMissingCaseTicket)
	}
	return c.actorPost(ctx, path, "CaseTicket "+ticketID, idempotency, in, out, headers)
}

// actorPost carries one verified per-actor credential, already formatted as an
// Authorization value. It deliberately does NOT fall back to the service
// credential: on these operations that credential is not an accepted scheme.
func (c *HTTPClient) actorPost(ctx context.Context, path, authorization, idempotency string, in, out any, headers map[string]string) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
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
	// Deliberately NO enterprise_id. The frozen operation declares exactly one
	// parameter, since_version, and states that tenant scope "is derived from
	// the verified service credential and never travels as a request
	// parameter". Sending it was a caller-supplied org fact crossing the
	// boundary; the server ignored it, so it was a silent lie rather than a
	// rejection.
	q.Set("since_version", strconv.FormatInt(sinceVersion, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/org-events?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("nexus org-events: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	// The frozen operation is security:[{trustedServiceSecret}]. This request
	// previously carried no credential at all: basic auth is applied only
	// inside doPost, behind a path allowlist this GET never passes through, so
	// against a real AgentNexus it could only ever 401. It went unnoticed
	// because the composition root is env-gated and has never run.
	req.SetBasicAuth(c.serviceClientID, c.serviceSecret)

	// The stream is long-lived, so it bypasses the client-level timeout - but
	// it must NOT bypass the no-follow redirect guard installed in New. Building
	// a client from the transport alone dropped that guard, which was harmless
	// only while the request was unauthenticated; now that it carries a
	// credential, following a redirect would forward the Authorization header
	// to whatever host the response named.
	streaming := &http.Client{
		Transport: c.http.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := streaming.Do(req)
	if err != nil {
		return fmt.Errorf("nexus org-events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		// The contract declares 409 for a cursor ahead of the sealed version.
		// That is a corrupt-consumer signal, not a transient fault, and callers
		// must not retry it in a loop: the cursor has to be reset deliberately.
		return fmt.Errorf("%w: since_version %d is ahead of the sealed org version", ErrOrgCursorAhead, sinceVersion)
	}
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
