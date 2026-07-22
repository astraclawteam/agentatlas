package nexusclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

// --- fixtures ---------------------------------------------------------

func actionFixtureRequest() nexus.ActionRequest {
	return nexus.ActionRequest{
		ActionID:         "act-0001",
		GoalRef:          "goal-0001",
		OutcomeRef:       "outcome-0001",
		WorkCaseID:       "case-0001",
		WorkPlanRevision: 1,
		Actor:            "actor-alice",
		OrgScope:         "org-1",
		Capability:       "erp.purchase_order.approve",
		ParameterHash:    "sha256:" + fixtureHex('a'),
		Postconditions: []nexus.PostconditionSpec{
			{PostconditionID: "post-0001", Description: "PO 100 approved", VerificationNeedID: "need-0001"},
		},
		VerificationNeeds: []nexus.VerificationNeed{
			{NeedID: "need-0001", Source: "erp.purchase_order", Authority: "erp-system-of-record", MaxAge: time.Hour},
		},
		Risk:                   nexus.RiskMedium,
		IdempotencyKey:         "idem-key-0001-abcdefgh",
		ExpiresAt:              time.Now().Add(time.Hour),
		ExecutionReceiptSchema: "erp.purchase_order.approve.v1",
	}
}

func fixtureHex(b byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return string(out)
}

func fixtureKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

// signedReceiptJSON signs the given map (minus "signature") with priv under
// keyID and returns the JSON bytes to write as an HTTP response body. Tests
// build the receipt as a map (not nexus.ActionReceipt) so they can construct
// deliberately unsigned/mis-keyed/tampered wire payloads that would not
// otherwise round-trip through the Go struct's own signingPayload().
func signedJSON(t *testing.T, priv ed25519.PrivateKey, keyID string, fields map[string]any, payload []byte) []byte {
	t.Helper()
	sig := ed25519.Sign(priv, payload)
	out := map[string]any{}
	for k, v := range fields {
		out[k] = v
	}
	out["signature"] = map[string]any{
		"algorithm": nexus.SignatureAlgorithmEd25519,
		"key_id":    keyID,
		"value":     base64.StdEncoding.EncodeToString(sig),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal signed fixture: %v", err)
	}
	return raw
}

// actionReceiptSigningPayload reproduces nexus.ActionReceipt.signingPayload
// (unexported) so this package can build correctly and incorrectly signed
// wire fixtures without depending on nexus test-only helpers.
func actionReceiptSigningPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	withoutSig := map[string]any{}
	for k, v := range fields {
		withoutSig[k] = v
	}
	var r nexus.ActionReceipt
	raw, err := json.Marshal(withoutSig)
	if err != nil {
		t.Fatalf("marshal action receipt fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("round-trip action receipt fixture: %v", err)
	}
	r.Signature = nil
	canon, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("canonicalize action receipt fixture: %v", err)
	}
	return canon
}

func observationReceiptSigningPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	var o nexus.ObservationReceipt
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal observation receipt fixture: %v", err)
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("round-trip observation receipt fixture: %v", err)
	}
	o.Signature = nil
	canon, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("canonicalize observation receipt fixture: %v", err)
	}
	return canon
}

func actionGrantFields(req nexus.ActionRequest) map[string]any {
	return map[string]any{
		"grant_id":       "grant-0001",
		"action_id":      req.ActionID,
		"work_case_id":   req.WorkCaseID,
		"actor":          req.Actor,
		"org_scope":      req.OrgScope,
		"capability":     req.Capability,
		"parameter_hash": req.ParameterHash,
		"one_use":        true,
		"issued_at":      time.Now().Format(time.RFC3339),
		"expires_at":     time.Now().Add(time.Hour).Format(time.RFC3339),
	}
}

func actionReceiptFields(req nexus.ActionRequest) map[string]any {
	return map[string]any{
		"receipt_id":        "rcp-0001",
		"action_id":         req.ActionID,
		"parameter_hash":    req.ParameterHash,
		"connector_ref":     "connector-ref-opaque",
		"target_ref":        "target-ref-opaque",
		"executor_identity": "svc-connector-worker-7",
		"result_code":       "succeeded",
		"audit_ref":         "audit-ref-0001",
		"issued_at":         time.Now().Format(time.RFC3339),
	}
}

func observationReceiptFields(req nexus.ActionRequest) map[string]any {
	return map[string]any{
		"observation_id":              "obs-0001",
		"action_id":                   req.ActionID,
		"parameter_hash":              req.ParameterHash,
		"postcondition_id":            req.Postconditions[0].PostconditionID,
		"verification_need_id":        req.VerificationNeeds[0].NeedID,
		"source":                      req.VerificationNeeds[0].Source,
		"authority":                   req.VerificationNeeds[0].Authority,
		"observed_at":                 time.Now().Format(time.RFC3339),
		"normalized_observation_hash": "sha256:" + fixtureHex('e'),
		"evidence_ref":                "evd_fixture0000000001",
		"audit_ref":                   "audit-ref-0002",
	}
}

// actionTestReceiptRef is the opaque handle the frozen receipt surface is
// addressed by. AgentNexus mints it and returns it on the Action; tests stand in
// for that by handing it to FetchActionReceipt directly.
const actionTestReceiptRef = "rcp_fixture0000000001"

func actionTestReceiptRoute() string { return "GET /v1/runtime/receipts/" + actionTestReceiptRef }

// actionServer wires the frozen Action endpoints the test drives, each returning
// the response body handed to it verbatim — tests control the exact wire payload
// (signed correctly, signed wrongly, or not at all). Keys are full
// "METHOD /path" routes: the receipt surface is a GET addressed by handle, not
// another POST, so the method is part of what these tests pin.
func actionServer(t *testing.T, serviceSecret string, responses map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for route, body := range responses {
		body, route := body, route
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			clientID, secret, ok := r.BasicAuth()
			if !ok || clientID != "agentatlas" || secret != serviceSecret {
				t.Errorf("%s: missing/invalid service Basic credentials", route)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func jsonBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

func newActionTestClient(t *testing.T, serviceSecret string, srv *httptest.Server, trustedKey ed25519.PublicKey) *HTTPClient {
	t.Helper()
	c, err := New(srv.URL, 5*time.Second, "agentatlas", writeServiceSecret(t, serviceSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigureActionSigningKey("nexus-signing-key-1", trustedKey); err != nil {
		t.Fatalf("configure action signing key: %v", err)
	}
	return c
}

const actionTestServiceSecret = "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3"

// --- ActionClient interface satisfaction -----------------------------------

var _ nexus.ActionClient = (*HTTPClient)(nil)

// --- RequestAction -----------------------------------------------------

func TestActionClientRequestActionAcceptsValidGrant(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	grantJSON, err := json.Marshal(actionGrantFields(req))
	if err != nil {
		t.Fatal(err)
	}
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		"POST /v1/runtime/act": grantJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	grant, err := c.RequestAction(context.Background(), req)
	if err != nil {
		t.Fatalf("RequestAction: %v", err)
	}
	if grant.GrantID != "grant-0001" || grant.ActionID != req.ActionID {
		t.Fatalf("unexpected grant: %+v", grant)
	}
}

func TestActionClientRequestActionRejectsMissingIdempotencyKey(t *testing.T) {
	req := actionFixtureRequest()
	req.IdempotencyKey = ""
	pub, _ := fixtureKeyPair(t)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		"POST /v1/runtime/act": []byte(`{}`),
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	if _, err := c.RequestAction(context.Background(), req); err == nil {
		t.Fatal("ActionRequest with no idempotency key was accepted by the client")
	}
}

func TestActionClientRequestActionRejectsExpiredGrant(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	fields := actionGrantFields(req)
	fields["issued_at"] = time.Now().Add(-2 * time.Hour).Format(time.RFC3339)
	fields["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	grantJSON, _ := json.Marshal(fields)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		"POST /v1/runtime/act": grantJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	_, err := c.RequestAction(context.Background(), req)
	if !errors.Is(err, nexus.ErrGrantExpired) {
		t.Fatalf("expired grant from the wire rejected for the wrong reason: %v", err)
	}
}

func TestActionClientRequestActionRejectsMismatchedOrgActor(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	fields := actionGrantFields(req)
	fields["actor"] = "actor-mallory"
	grantJSON, _ := json.Marshal(fields)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		"POST /v1/runtime/act": grantJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	_, err := c.RequestAction(context.Background(), req)
	if !errors.Is(err, nexus.ErrActorOrgMismatch) {
		t.Fatalf("actor-mismatched grant from the wire rejected for the wrong reason: %v", err)
	}
}

func TestActionClientRequestActionRejectsReplayedGrant(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	grantJSON, _ := json.Marshal(actionGrantFields(req))
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		"POST /v1/runtime/act": grantJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	if _, err := c.RequestAction(context.Background(), req); err != nil {
		t.Fatalf("first RequestAction: %v", err)
	}
	// The server (deliberately, for this test) returns the exact same
	// grant_id a second time, simulating a replayed/duplicated grant.
	_, err := c.RequestAction(context.Background(), req)
	if !errors.Is(err, nexus.ErrGrantReplayed) {
		t.Fatalf("replayed grant_id from the wire rejected for the wrong reason: %v", err)
	}
}

func TestActionClientRequestActionSendsNoConnectorSpecificBody(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	grantJSON, _ := json.Marshal(actionGrantFields(req))

	var capturedBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runtime/act", func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = jsonBody(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(grantJSON)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	if _, err := c.RequestAction(context.Background(), req); err != nil {
		t.Fatalf("RequestAction: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("decode captured request body: %v", err)
	}
	forbidden := []string{"connector_instance_id", "connector_endpoint", "credential", "endpoint", "connector_id", "table", "api_path"}
	for _, key := range forbidden {
		if _, ok := body[key]; ok {
			t.Errorf("ActionRequest wire body contains a connector-specific key %q: %s", key, capturedBody)
		}
	}
	for key := range body {
		if strings.HasPrefix(key, "connector_") {
			t.Errorf("ActionRequest wire body contains a connector_-prefixed key %q: %s", key, capturedBody)
		}
	}
}

// --- FetchActionReceipt --------------------------------------------------

func TestActionClientFetchActionReceiptAcceptsValidSigned(t *testing.T) {
	req := actionFixtureRequest()
	pub, priv := fixtureKeyPair(t)
	fields := actionReceiptFields(req)
	payload := actionReceiptSigningPayload(t, fields)
	receiptJSON := signedJSON(t, priv, "nexus-signing-key-1", fields, payload)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		actionTestReceiptRoute(): receiptJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	receipt, err := c.FetchActionReceipt(context.Background(), req, actionTestReceiptRef)
	if err != nil {
		t.Fatalf("FetchActionReceipt: %v", err)
	}
	if receipt.ReceiptID != "rcp-0001" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
}

func TestActionClientFetchActionReceiptRejectsUnsigned(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	fields := actionReceiptFields(req)
	unsigned, _ := json.Marshal(fields) // no "signature" key at all
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		actionTestReceiptRoute(): unsigned,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	_, err := c.FetchActionReceipt(context.Background(), req, actionTestReceiptRef)
	if !errors.Is(err, nexus.ErrUnsignedReceipt) {
		t.Fatalf("unsigned wire ActionReceipt rejected for the wrong reason: %v", err)
	}
}

func TestActionClientFetchActionReceiptRejectsWrongKey(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	_, wrongPriv := fixtureKeyPair(t)
	fields := actionReceiptFields(req)
	payload := actionReceiptSigningPayload(t, fields)
	receiptJSON := signedJSON(t, wrongPriv, "nexus-signing-key-1", fields, payload)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		actionTestReceiptRoute(): receiptJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	_, err := c.FetchActionReceipt(context.Background(), req, actionTestReceiptRef)
	if !errors.Is(err, nexus.ErrUntrustedSignature) {
		t.Fatalf("wrong-key wire ActionReceipt rejected for the wrong reason: %v", err)
	}
}

func TestActionClientFetchActionReceiptRejectsChangedParameterHash(t *testing.T) {
	req := actionFixtureRequest()
	pub, priv := fixtureKeyPair(t)
	// Tamper parameter_hash FIRST, then sign the tampered payload with the
	// valid key, so the wire receipt is validly signed over its own bytes
	// and the ONLY thing wrong is that its parameter_hash no longer matches
	// the ActionRequest. This isolates VerifyActionReceipt's binding check
	// (receipt.ParameterHash != req.ParameterHash) from its signature check:
	// the signature verifies, so only the binding guard can reject —
	// deleting that guard would make this test fail. (Signing the ORIGINAL
	// payload then tampering after would instead let the signature check
	// shadow the binding check.)
	fields := actionReceiptFields(req)
	fields["parameter_hash"] = "sha256:" + fixtureHex('9')
	payload := actionReceiptSigningPayload(t, fields)
	receiptJSON := signedJSON(t, priv, "nexus-signing-key-1", fields, payload)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		actionTestReceiptRoute(): receiptJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	if _, err := c.FetchActionReceipt(context.Background(), req, actionTestReceiptRef); err == nil {
		t.Fatal("ActionReceipt with a changed parameter_hash was accepted by the client")
	}
}

func TestActionClientActionReceiptNeverConfirmsOutcome(t *testing.T) {
	req := actionFixtureRequest()
	pub, priv := fixtureKeyPair(t)
	fields := actionReceiptFields(req)
	payload := actionReceiptSigningPayload(t, fields)
	receiptJSON := signedJSON(t, priv, "nexus-signing-key-1", fields, payload)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		actionTestReceiptRoute(): receiptJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	receipt, err := c.FetchActionReceipt(context.Background(), req, actionTestReceiptRef)
	if err != nil {
		t.Fatalf("FetchActionReceipt: %v", err)
	}
	if err := receipt.ConfirmsOutcome(); !errors.Is(err, nexus.ErrReceiptNotOutcomeEvidence) {
		t.Fatalf("a receipt fetched over the wire must still never confirm a business Outcome, got: %v", err)
	}
}

// --- FetchObservationReceipt (dormant) -------------------------------------

// TestActionClientFetchObservationReceiptIsDormantAndCallsNothing pins the
// dormancy rather than leaving it to a comment. AgentNexus's frozen contract has
// no observation-read surface — its only receipt surface is the GET the tests
// above drive, which returns an ActionReceipt — so this method must reach for no
// endpoint at all. It previously POSTed to /v1/actions/observation, a path that
// appears nowhere in the contract and could only ever 404.
//
// The three rejection cases this replaces (detached from Action, detached from
// PostconditionSpec, never-confirms-Outcome) were assertions about
// nexus.VerifyObservationReceipt reached through a transport that does not
// exist. They live on unchanged in sdk/go/nexus/action_test.go, against the
// verifier directly.
func TestActionClientFetchObservationReceiptIsDormantAndCallsNothing(t *testing.T) {
	req := actionFixtureRequest()
	pub, _ := fixtureKeyPair(t)
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	_, err := c.FetchObservationReceipt(context.Background(), req, "need-0001")
	if !errors.Is(err, ErrNoFrozenSurface) {
		t.Fatalf("observation fetch must fail closed on the missing frozen surface, got: %v", err)
	}
	if len(called) > 0 {
		t.Fatalf("dormant observation fetch put %v on the wire", called)
	}
}

// --- fail-closed unconfigured signing key ----------------------------------

func TestActionClientUnconfiguredSigningKeyRejectsReceipt(t *testing.T) {
	req := actionFixtureRequest()
	_, priv := fixtureKeyPair(t)
	fields := actionReceiptFields(req)
	payload := actionReceiptSigningPayload(t, fields)
	receiptJSON := signedJSON(t, priv, "nexus-signing-key-1", fields, payload)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		actionTestReceiptRoute(): receiptJSON,
	})
	// Client built WITHOUT ConfigureActionSigningKey: no trusted key is
	// registered, so verifySignature cannot verify any signature and the
	// client must reject even a validly-signed receipt (fail-closed) rather
	// than silently skip the recheck.
	c, err := New(srv.URL, 5*time.Second, "agentatlas", writeServiceSecret(t, actionTestServiceSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.FetchActionReceipt(context.Background(), req, actionTestReceiptRef); !errors.Is(err, nexus.ErrUntrustedSignature) {
		t.Fatalf("unconfigured-key client must reject any receipt (fail-closed), got: %v", err)
	}
}

// --- external_receipt round-trip over the real HTTP wire path --------------

func TestActionClientFetchActionReceiptRoundTripsNonEmptyExternalReceipt(t *testing.T) {
	req := actionFixtureRequest()
	pub, priv := fixtureKeyPair(t)
	fields := actionReceiptFields(req)
	// Populate external_receipt (json.RawMessage on the struct). Its bytes
	// must survive the FULL wire path — server marshal -> HTTP ->
	// client json.Decoder decode -> VerifyActionReceipt signingPayload
	// re-marshal — byte-for-byte, or signature verification fails. 0E-0J
	// will populate this field and its canonicalization is the least obvious
	// in the type, so lock the round-trip now.
	fields["external_receipt"] = json.RawMessage(`{"erp_document_number":"PO-100-A","lines":[{"n":1},{"n":2}]}`)
	payload := actionReceiptSigningPayload(t, fields)
	receiptJSON := signedJSON(t, priv, "nexus-signing-key-1", fields, payload)
	srv := actionServer(t, actionTestServiceSecret, map[string][]byte{
		actionTestReceiptRoute(): receiptJSON,
	})
	c := newActionTestClient(t, actionTestServiceSecret, srv, pub)

	receipt, err := c.FetchActionReceipt(context.Background(), req, actionTestReceiptRef)
	if err != nil {
		t.Fatalf("receipt with a non-empty external_receipt failed to round-trip over the wire: %v", err)
	}
	if len(receipt.ExternalReceipt) == 0 {
		t.Fatal("external_receipt was dropped on the wire")
	}
}
