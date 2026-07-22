package workcaseexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// frozenContractSnapshot is AgentNexus's published gateway-runtime contract, the
// SAME byte-pinned snapshot internal/nexusclient's parity tests hold themselves
// against (its digest is asserted there against AgentNexus's own contract.lock).
// Reusing it rather than re-copying is what makes the assertions below track the
// real contract instead of a second, drifting transcription.
const frozenContractSnapshot = "../nexusclient/testdata/agentnexus-gateway-runtime-published.yaml"

// TestFrozenActSurfaceRefusesTheOnlyCredentialAHeadlessAtlasHolds pins
// ErrNoServiceCallableActionSurface.
//
// A headless AgentAtlas process holds exactly one AgentNexus credential: the
// trusted service secret nexusclient.New loads from disk. Access Tickets arrive
// only on inbound requests. If POST /v1/runtime/act does not accept
// trustedServiceSecret, an orchestrator running in atlas-worker has nothing it
// is allowed to present, and no ActionGateway can dispatch.
//
// When AgentNexus adds trustedServiceSecret to that operation this test goes
// red, which is the signal that the gateway can and must be written.
func TestFrozenActSurfaceRefusesTheOnlyCredentialAHeadlessAtlasHolds(t *testing.T) {
	schemes := securitySchemesFor(t, "/v1/runtime/act", "post")
	if len(schemes) == 0 {
		t.Fatal("POST /v1/runtime/act declares no security schemes; the assertion would pass vacuously")
	}
	for _, scheme := range schemes {
		if scheme == "trustedServiceSecret" {
			t.Fatalf("POST /v1/runtime/act now accepts trustedServiceSecret (%v): a headless AgentAtlas CAN reach the action surface, so ErrNoServiceCallableActionSurface is stale and an ActionGateway must be implemented", schemes)
		}
	}
	// The receipt read is the other half of a Dispatch round trip; if it were
	// service-callable while /act was not, the reason above would be incomplete.
	receiptSchemes := securitySchemesFor(t, "/v1/runtime/receipts/{receipt_ref}", "get")
	if len(receiptSchemes) == 0 {
		t.Fatal("GET /v1/runtime/receipts/{receipt_ref} declares no security schemes; the assertion would pass vacuously")
	}
}

// TestAtlasCannotAuthorAValidFrozenActionRequest pins ErrNoRiskDecisionSigner.
//
// It builds the MOST complete frozen runtime.ActionRequest AgentAtlas can
// actually produce today -- every field it has a real source for -- and shows
// Validate rejects it. The rejection is on risk_decision, which requires a
// signature by the calling authority, and AgentAtlas holds no signing key
// anywhere in this repository or its configuration.
//
// This is deliberately built from real values rather than a zero struct: a zero
// struct would fail on the first missing field and prove nothing about which
// input is genuinely unobtainable.
func TestAtlasCannotAuthorAValidFrozenActionRequest(t *testing.T) {
	parameters, parameterHash, err := nexusruntime.BuildParameters(map[string]any{"purchase_order_id": "PO-100"})
	if err != nil {
		t.Fatalf("build parameters: %v", err)
	}
	req := nexusruntime.ActionRequest{
		RequestID:          "req-workcase-step-0001",
		BusinessContextRef: "wc_0123456789abcdef0123",
		Capability:         "erp.purchase_order.approve",
		Parameters:         parameters,
		ParameterHash:      parameterHash,
		Purpose:            "execute_workcase_step",
		// RiskDecision is left unset because AgentAtlas cannot produce one: it
		// carries a Signature by the calling authority, and this product holds
		// no private key. Everything else above is populated.
		IdempotencyKey:        "wc-step-idem-0123456789",
		ExpiresAt:             time.Now().Add(time.Hour).UTC(),
		ExpectedReceiptSchema: "erp.purchase_order.approve.v1",
	}
	err = req.Validate()
	if err == nil {
		t.Fatal("the frozen ActionRequest now validates without a signed RiskDecision: ErrNoRiskDecisionSigner is stale and an ActionGateway must be implemented")
	}
	if !strings.Contains(err.Error(), "risk_decision") {
		t.Fatalf("expected the unobtainable input to be risk_decision, got %v", err)
	}
	// Prove the rejection is specifically the SIGNATURE, not some other missing
	// RiskDecision field we could in fact fill in.
	req.RiskDecision = nexusruntime.RiskDecision{
		DecisionID:         "dec-0001",
		Authority:          "agentatlas",
		RiskLevel:          nexusruntime.RiskLow,
		Capability:         req.Capability,
		ParameterHash:      req.ParameterHash,
		BusinessContextRef: req.BusinessContextRef,
		IssuedAt:           time.Now().UTC(),
		ExpiresAt:          time.Now().Add(time.Hour).UTC(),
		// Signature deliberately absent.
	}
	err = req.Validate()
	if err == nil {
		t.Fatal("an UNSIGNED RiskDecision is now accepted; the frozen contract's signing requirement has changed")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected the remaining gap to be the signature, got %v", err)
	}
}

// TestAtlasWorkCaseIDsAreNotFrozenWorkCaseHandles records a third, smaller gap
// that would otherwise be discovered at the first real dispatch: the frozen
// contract validates business_context_ref as an opaque wc_* handle, and
// internal/workcase mints case_* ids. It is pinned here, next to the other two,
// so C2 does not rediscover it.
func TestAtlasWorkCaseIDsAreNotFrozenWorkCaseHandles(t *testing.T) {
	// The exact shape internal/workcase.deriveCaseID produces.
	const atlasCaseID = "case_0123456789abcdef0123456789abcdef"
	if err := nexusruntime.ValidateHandle(atlasCaseID, nexusruntime.HandleWorkCase); err == nil {
		t.Fatal("an AgentAtlas case_* id now validates as a frozen wc_* WorkCase handle; the note in this test is stale")
	}
}

// --- snapshot helpers ----------------------------------------------------

func securitySchemesFor(t *testing.T, path, method string) []string {
	t.Helper()
	document := map[string]any{}
	raw, err := os.ReadFile(filepath.FromSlash(frozenContractSnapshot))
	if err != nil {
		t.Fatalf("read frozen contract snapshot: %v", err)
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse frozen contract snapshot: %v", err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("frozen contract snapshot has no paths object")
	}
	item, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("frozen contract declares no %s; this assertion is about a path that no longer exists", path)
	}
	operation, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("frozen contract declares no %s %s", method, path)
	}
	entries, ok := operation["security"].([]any)
	if !ok {
		return nil
	}
	var schemes []string
	for _, entry := range entries {
		requirement, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		for name := range requirement {
			schemes = append(schemes, name)
		}
	}
	return schemes
}

// jsonRoundTrip keeps the compiler honest about the imports this file needs for
// the parameter bytes above; BuildParameters returns marshal-stable JSON and
// this asserts that assumption rather than trusting it.
func TestFrozenParameterBytesAreMarshalStable(t *testing.T) {
	parameters, _, err := nexusruntime.BuildParameters(map[string]any{"purchase_order_id": "PO-100"})
	if err != nil {
		t.Fatalf("build parameters: %v", err)
	}
	wrapped, err := json.Marshal(struct {
		Parameters json.RawMessage `json:"parameters"`
	}{Parameters: parameters})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back struct {
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(wrapped, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if nexusruntime.HashParameters(back.Parameters) != nexusruntime.HashParameters(parameters) {
		t.Fatal("parameter bytes did not survive a marshal round trip; the hash binding would break in transit")
	}
}
