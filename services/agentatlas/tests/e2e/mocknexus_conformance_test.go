package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
)

// Contract conformance of the mock AgentNexus.
//
// Why this file exists at all: the mock's only consumer was
// TestAgentAtlasMVPRealStack, which skips unless ATLAS_E2E_REALSTACK=1 and a
// full docker-compose stack is up. Nothing in a default `go test ./...` ever
// started the mock, so every strictness rule it claimed to enforce was
// unverified and free to rot — which is exactly how the divergences this file
// now pins went unnoticed.
//
// The alternative was to make the real-stack test run by default. That is not
// possible and would not be desirable: it needs PostgreSQL, OpenSearch, MinIO,
// NATS, real docling and a real llmrouter API key, and it takes minutes. The
// property worth protecting is not "the deployed stack works", it is "the mock
// is not more permissive than a real AgentNexus" — and that is a property of
// the handlers alone. So these tests drive the SAME handlers over loopback with
// no external dependency and no build tag.
//
// Every assertion here goes over real HTTP against the real handler. Nothing
// inspects source text: a test that grepped this file would pass just as
// happily against a handler that had been deleted.

const (
	conformanceNeedID    = "brief_alpha"
	conformanceDataClass = "hr.work_brief"
	conformancePurpose   = "mock_contract_conformance"
	conformanceDetail    = "sanitized brief detail"

	// The handles the mock mints for the seeded fixture, in the frozen
	// grammars: wc_/evd_ followed by 16..128 chars of [A-Za-z0-9_-].
	conformanceWorkCaseRef = "wc_mocknexus0000000"
	conformanceEvidenceRef = "evd_brief_alpha00000"

	// A canonical sha256:<64 hex> digest and an apl_ approval plan handle.
	conformanceHash          = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	conformancePlanRef       = "apl_conformance00001"
	conformanceAuthorityName = "acme-oa"
)

// newConformanceServer starts the mock's real handlers on loopback with one
// seeded evidence fixture and one org event.
func newConformanceServer(t *testing.T) (*httptest.Server, *nexusclient.Mock, []nexus.OrgEvent) {
	t.Helper()
	backing := nexusclient.NewMock()
	backing.SetEvidence(conformanceNeedID, conformanceDetail)
	events := []nexus.OrgEvent{
		{EventID: "evt_1", EventType: "org.changed", OrgVersion: 7, OccurredAt: time.Unix(1700000000, 0).UTC()},
		{EventID: "evt_2", EventType: "org.changed", OrgVersion: 9, OccurredAt: time.Unix(1700000100, 0).UTC()},
	}
	server := httptest.NewServer(newMockNexusHandler(backing, events))
	t.Cleanup(server.Close)
	return server, backing, events
}

func conformanceExpiry() string {
	return time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
}

// postMock sends body to path and returns the status and decoded JSON object.
func postMock(t *testing.T, server *httptest.Server, path string, body any, decorate func(*http.Request)) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if decorate != nil {
		decorate(req)
	}
	return doMock(t, req)
}

func doMock(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// withServiceCredential is the trusted first-party service credential the
// deployed AgentAtlas presents over basic auth.
func withServiceCredential(req *http.Request) { req.SetBasicAuth("agentatlas", "mock-secret") }

func locateBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"request_id": "conf-locate",
		"purpose":    conformancePurpose,
		"expires_at": conformanceExpiry(),
		"data_needs": []any{map[string]any{
			"need_id": conformanceNeedID, "data_class": conformanceDataClass, "purpose": conformancePurpose,
		}},
	}
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

func readBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"request_id":           "conf-read",
		"business_context_ref": conformanceWorkCaseRef,
		"evidence_ref":         conformanceEvidenceRef,
		"purpose":              conformancePurpose,
		"expires_at":           conformanceExpiry(),
	}
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

// TestMockNexusEvidenceHandlersRejectWhatAgentNexusRejects is the load-bearing
// test of the pair: it asserts the mock REFUSES the bodies a real AgentNexus
// refuses. Asserting only that valid requests succeed would pass against a
// handler that accepted anything.
//
// Each case names what the retired hand-rolled six-name top-level blocklist did
// with it. The mock now decodes through the SDK's canonical strict decoders —
// the same nexusruntime.Decode* functions the real gateway's evidence handler
// calls — so the coverage is the contract's, not a list someone has to
// remember to extend.
func TestMockNexusEvidenceHandlersRejectWhatAgentNexusRejects(t *testing.T) {
	server, _, _ := newConformanceServer(t)

	cases := []struct {
		name      string
		overrides map[string]any
	}{
		// Trusted identity and connector topology never travel in request JSON.
		{"top-level enterprise id", map[string]any{"enterprise_id": "ent_forged"}},
		{"actor identity (blocklist missed)", map[string]any{"actor_user_id": "u_forged"}},
		{"connector instance (blocklist missed)", map[string]any{"connector_instance_id": "conn-7"}},
		{"connector-prefixed key (blocklist missed)", map[string]any{"connector_host": "erp.internal"}},
		{"key containing enterprise (blocklist missed)", map[string]any{"source_enterprise_ref": "ent_forged"}},
		{"identity nested one level (blocklist missed)", map[string]any{"constraints": map[string]any{"enterprise_id": "ent_forged"}}},

		// Retired members of the pre-migration surface. The blocklist named
		// these explicitly; strict decoding rejects them as unknown fields
		// without needing to.
		{"retired ticket_id", map[string]any{"ticket_id": "tick_1"}},
		{"retired resource_uri", map[string]any{"resource_uri": "postgres://db/table"}},
		{"retired evidence_pointer_id", map[string]any{"evidence_pointer_id": "ep_1"}},
		{"retired query_intent", map[string]any{"query_intent": "summarize"}},
		{"retired max_bytes", map[string]any{"max_bytes": 4096}},

		// Any unknown member at all, and the retired untyped action shape.
		{"unknown member (blocklist missed)", map[string]any{"org_version": 7}},
		{"legacy action+input shape (blocklist missed)", map[string]any{"action": "read", "input": map[string]any{}}},
	}

	for _, tc := range cases {
		t.Run("locate/"+tc.name, func(t *testing.T) {
			if status, _ := postMock(t, server, "/v1/runtime/locate", locateBody(tc.overrides), nil); status != http.StatusBadRequest {
				t.Fatalf("locate accepted %s: status %d, want 400", tc.name, status)
			}
		})
		t.Run("read/"+tc.name, func(t *testing.T) {
			if status, _ := postMock(t, server, "/v1/runtime/read", readBody(tc.overrides), nil); status != http.StatusBadRequest {
				t.Fatalf("read accepted %s: status %d, want 400", tc.name, status)
			}
		})
	}

	// Identity smuggled inside the data_needs array, not at the root: the
	// blocklist scanned only top-level keys, so this was invisible to it.
	nested := locateBody(map[string]any{"data_needs": []any{map[string]any{
		"need_id": conformanceNeedID, "data_class": conformanceDataClass, "purpose": conformancePurpose,
		"enterprise_id": "ent_forged",
	}}})
	if status, _ := postMock(t, server, "/v1/runtime/locate", nested, nil); status != http.StatusBadRequest {
		t.Fatalf("locate accepted identity nested in data_needs: status %d, want 400", status)
	}

	// A detached verification purpose. The frozen coupling is bidirectional, so
	// the purpose without its all-or-nothing verification_binding is rejected —
	// a rule no field-name blocklist could ever express.
	detached := readBody(map[string]any{"purpose": "postcondition_verification"})
	if status, _ := postMock(t, server, "/v1/runtime/read", detached, nil); status != http.StatusBadRequest {
		t.Fatalf("read accepted a detached verification purpose: status %d, want 400", status)
	}

	// Positive control: the rejections above mean nothing unless a
	// contract-clean request still succeeds.
	if status, _ := postMock(t, server, "/v1/runtime/locate", locateBody(nil), nil); status != http.StatusOK {
		t.Fatalf("locate rejected a contract-clean request: status %d, want 200", status)
	}
	if status, _ := postMock(t, server, "/v1/runtime/read", readBody(nil), nil); status != http.StatusOK {
		t.Fatalf("read rejected a contract-clean request: status %d, want 200", status)
	}
}

// TestMockNexusReadEnvelopeMatchesTheRealHandler pins the read response shape.
//
// The mock used to answer every allowed read with a grant_ref under a grn_
// prefix. Neither half was real: the frozen handle grammar for a grant is
// ^grant_[A-Za-z0-9_-]{16,128}$, and the real AgentNexus read handler never
// populates the member at all — a Step Grant is a separate audited object from
// POST /v1/step-grants, while reads are served from evidence authorized at
// locate time.
//
// That divergence was live, not cosmetic: AgentAtlas's two dream-evidence
// handlers used to refuse any read whose GrantRef was empty, so against a real
// AgentNexus they returned 403 for every request, and this mock was the only
// reason it looked fine. Both now gate on the decision instead, and this test
// keeps the mock from quietly reintroducing the member they used to trust.
func TestMockNexusReadEnvelopeMatchesTheRealHandler(t *testing.T) {
	server, _, _ := newConformanceServer(t)

	status, located := postMock(t, server, "/v1/runtime/locate", locateBody(nil), nil)
	if status != http.StatusOK {
		t.Fatalf("locate: status %d", status)
	}
	handles, _ := located["evidence"].([]any)
	if len(handles) != 1 {
		t.Fatalf("locate returned %d handles, want 1: %v", len(handles), located)
	}
	handle, _ := handles[0].(map[string]any)
	evidenceRef, _ := handle["evidence_ref"].(string)
	if evidenceRef != conformanceEvidenceRef {
		t.Fatalf("evidence_ref = %q, want %q", evidenceRef, conformanceEvidenceRef)
	}

	status, read := postMock(t, server, "/v1/runtime/read", readBody(nil), nil)
	if status != http.StatusOK {
		t.Fatalf("read: status %d", status)
	}
	if _, present := read["grant_ref"]; present {
		t.Fatalf("read emitted grant_ref; the real AgentNexus read handler never does: %v", read)
	}
	if read["decision"] != "allow" {
		t.Fatalf("decision = %v, want allow", read["decision"])
	}
	// The freshness trio is stated on every allowed read so a cached answer can
	// never masquerade as a live one.
	for _, member := range []string{"source_version", "as_of", "served_from_cache"} {
		if _, present := read[member]; !present {
			t.Fatalf("allowed read omitted %s: %v", member, read)
		}
	}

	// Decoding into the production type is what proves the divergence reaches
	// AgentAtlas rather than stopping at the JSON: this is the exact value the
	// two dream-evidence handlers test for emptiness.
	raw, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	var decoded nexusclient.ReadEvidenceResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GrantRef != "" {
		t.Fatalf("ReadEvidenceResult.GrantRef = %q, want empty", decoded.GrantRef)
	}
	if decoded.Decision != "allow" || decoded.SourceVersion == 0 || decoded.AsOf == "" {
		t.Fatalf("freshness trio did not survive decoding: %+v", decoded)
	}
}

// TestMockNexusReadFailsClosedOnAnUnknownHandle: an invalidated, revoked,
// expired or source-deleted handle is a denial, never an empty success.
func TestMockNexusReadFailsClosedOnAnUnknownHandle(t *testing.T) {
	server, _, _ := newConformanceServer(t)
	body := readBody(map[string]any{"evidence_ref": "evd_neverlocated0001"})
	if status, _ := postMock(t, server, "/v1/runtime/read", body, nil); status != http.StatusForbidden {
		t.Fatalf("read of an unknown handle: status %d, want 403", status)
	}
}

// TestMockNexusOrgEventsRefusesForbiddenInputs re-asserts over HTTP what the
// org-events handler already enforces: the feed is credentialed, and tenant
// scope comes from the credential rather than from a caller-supplied parameter.
func TestMockNexusOrgEventsRefusesForbiddenInputs(t *testing.T) {
	server, _, events := newConformanceServer(t)

	anonymous, err := http.NewRequest(http.MethodGet, server.URL+"/v1/org-events?since_version=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := doMock(t, anonymous); status != http.StatusUnauthorized {
		t.Fatalf("anonymous org-events: status %d, want 401", status)
	}

	scoped, err := http.NewRequest(http.MethodGet, server.URL+"/v1/org-events?since_version=0&enterprise_id=ent_forged", nil)
	if err != nil {
		t.Fatal(err)
	}
	withServiceCredential(scoped)
	if status, _ := doMock(t, scoped); status != http.StatusBadRequest {
		t.Fatalf("caller-supplied enterprise_id: status %d, want 400", status)
	}

	// Positive control: a credentialed cursor request replays strictly after
	// since_version. The stream is deliberately held open, so read the events
	// and cancel rather than waiting for an end that never comes.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	streamReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/org-events?since_version=7", nil)
	if err != nil {
		t.Fatal(err)
	}
	withServiceCredential(streamReq)
	resp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("credentialed org-events: status %d, want 200", resp.StatusCode)
	}
	var replayed []nexus.OrgEvent
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		payload, isData := strings.CutPrefix(scanner.Text(), "data:")
		if !isData {
			continue
		}
		var event nexus.OrgEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &event); err != nil {
			t.Fatal(err)
		}
		replayed = append(replayed, event)
		break // only v9 is strictly after the cursor
	}
	cancel()
	if len(replayed) != 1 || replayed[0].OrgVersion != events[1].OrgVersion {
		t.Fatalf("since_version=7 replayed %v, want only v%d", replayed, events[1].OrgVersion)
	}
}

func transmissionBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"request_id":           "conf-approval",
		"business_context_ref": conformanceWorkCaseRef,
		"capability":           "knowledge.publish",
		"parameter_hash":       conformanceHash,
		"purpose":              "governed_change_approval",
		"plan": map[string]any{
			"plan_ref": conformancePlanRef, "plan_hash": conformanceHash, "authority": conformanceAuthorityName,
		},
		"expires_at": conformanceExpiry(),
	}
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

// TestMockNexusApprovalTransmissionSurface covers the surface the mock did not
// serve at all, which is why AgentAtlas's two approval-transmission callers
// (internal/governance/nexus.go via internal/app/dream_handler.go and
// internal/app/browser_dream_handler.go) had zero end-to-end coverage.
//
// Transmission is DORMANT by design today — governance.WorkCaseContextFor
// returns false until WorkCase-backed changes exist under task C1, so the route
// resolver never reaches the wire. Serving the surface anyway is the point: the
// path has to be exercisable before C1 lands, not after.
func TestMockNexusApprovalTransmissionSurface(t *testing.T) {
	server, _, _ := newConformanceServer(t)

	t.Run("refuses an anonymous transmit", func(t *testing.T) {
		if status, _ := postMock(t, server, "/v1/approvals/transmissions", transmissionBody(nil), nil); status != http.StatusUnauthorized {
			t.Fatalf("anonymous transmit: status %d, want 401", status)
		}
	})

	rejected := []struct {
		name      string
		overrides map[string]any
	}{
		{"forged identity", map[string]any{"enterprise_id": "ent_forged"}},
		{"identity nested in the plan", map[string]any{"plan": map[string]any{
			"plan_ref": conformancePlanRef, "plan_hash": conformanceHash,
			"authority": conformanceAuthorityName, "actor_user_id": "u_forged",
		}}},
		{"unknown member", map[string]any{"risk_level": "high"}},
		{"approver selector", map[string]any{"approver_user_id": "u_boss"}},
		{"non-opaque plan handle", map[string]any{"plan": map[string]any{
			"plan_ref": "plan-1", "plan_hash": conformanceHash, "authority": conformanceAuthorityName,
		}}},
		{"agentnexus as the plan authority", map[string]any{"plan": map[string]any{
			"plan_ref": conformancePlanRef, "plan_hash": conformanceHash, "authority": "AgentNexus",
		}}},
		{"connector capability", map[string]any{"capability": "not a capability"}},
		{"already expired plan", map[string]any{"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}},
	}
	for _, tc := range rejected {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			status, _ := postMock(t, server, "/v1/approvals/transmissions", transmissionBody(tc.overrides), withServiceCredential)
			if status != http.StatusBadRequest {
				t.Fatalf("transmit accepted %s: status %d, want 400", tc.name, status)
			}
		})
	}

	t.Run("transmits, is idempotent, and conflicts on a rebound plan", func(t *testing.T) {
		status, first := postMock(t, server, "/v1/approvals/transmissions", transmissionBody(nil), withServiceCredential)
		if status != http.StatusOK {
			t.Fatalf("transmit: status %d, want 200: %v", status, first)
		}
		if first["plan_ref"] != conformancePlanRef || first["status"] != nexusclient.TransmissionDelivered {
			t.Fatalf("transmit status = %v", first)
		}
		// Delivery is transport, never a decision: only evidence_recorded
		// carries one, and nothing but the external authority can produce it.
		if _, present := first["decision"]; present {
			t.Fatalf("a delivered transmission must carry no decision: %v", first)
		}
		// The frozen schema states the boundary in words; assert it on the wire.
		for _, forbidden := range []string{"approver", "approver_user_id", "queue", "risk_level", "reviewer_user_id"} {
			if _, present := first[forbidden]; present {
				t.Fatalf("transmission status leaked %q: %v", forbidden, first)
			}
		}

		status, again := postMock(t, server, "/v1/approvals/transmissions", transmissionBody(nil), withServiceCredential)
		if status != http.StatusOK {
			t.Fatalf("re-transmit: status %d, want 200 (idempotent)", status)
		}
		if again["delivery_attempts"] != first["delivery_attempts"] {
			t.Fatalf("a delivered plan was re-sent: attempts %v -> %v", first["delivery_attempts"], again["delivery_attempts"])
		}

		rebound := transmissionBody(map[string]any{"parameter_hash": "sha256:" + strings.Repeat("f", 64)})
		if status, _ := postMock(t, server, "/v1/approvals/transmissions", rebound, withServiceCredential); status != http.StatusConflict {
			t.Fatalf("plan_ref rebound to a different operation: status %d, want 409", status)
		}

		statusReq, err := http.NewRequest(http.MethodGet, server.URL+"/v1/approvals/transmissions/"+conformancePlanRef, nil)
		if err != nil {
			t.Fatal(err)
		}
		withServiceCredential(statusReq)
		code, fetched := doMock(t, statusReq)
		if code != http.StatusOK || fetched["plan_hash"] != conformanceHash {
			t.Fatalf("get transmission: status %d, body %v", code, fetched)
		}
	})

	t.Run("refuses an anonymous status read and 404s an unknown plan", func(t *testing.T) {
		anonymous, err := http.NewRequest(http.MethodGet, server.URL+"/v1/approvals/transmissions/"+conformancePlanRef, nil)
		if err != nil {
			t.Fatal(err)
		}
		if status, _ := doMock(t, anonymous); status != http.StatusUnauthorized {
			t.Fatalf("anonymous status read: status %d, want 401", status)
		}
		unknown, err := http.NewRequest(http.MethodGet, server.URL+"/v1/approvals/transmissions/apl_neverTransmitted", nil)
		if err != nil {
			t.Fatal(err)
		}
		withServiceCredential(unknown)
		if status, _ := doMock(t, unknown); status != http.StatusNotFound {
			t.Fatalf("unknown plan_ref: status %d, want 404", status)
		}
	})
}
