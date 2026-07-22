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

// conformanceCaseTicket is the opaque Access Ticket the per-actor evidence
// surface accepts.
const conformanceCaseTicket = "tick_conformance0001"

// withCaseTicket presents an Access Ticket in the exact format the frozen
// caseTicket scheme declares: an apiKey in the Authorization header written
// "CaseTicket <opaque>". It is NOT an X-Case-Ticket header — that name appears
// nowhere in the pinned contract.
func withCaseTicket(req *http.Request) {
	req.Header.Set("Authorization", "CaseTicket "+conformanceCaseTicket)
}

// withBrowserSession presents the third accepted credential, the session
// cookie, so the mock is pinned to accept every scheme the contract declares
// rather than only the one AgentAtlas happens to send.
func withBrowserSession(req *http.Request) {
	req.AddCookie(&http.Cookie{Name: "nexus_browser_session", Value: "sess_conformance0001"})
}

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
			if status, _ := postMock(t, server, "/v1/runtime/locate", locateBody(tc.overrides), withCaseTicket); status != http.StatusBadRequest {
				t.Fatalf("locate accepted %s: status %d, want 400", tc.name, status)
			}
		})
		t.Run("read/"+tc.name, func(t *testing.T) {
			if status, _ := postMock(t, server, "/v1/runtime/read", readBody(tc.overrides), withCaseTicket); status != http.StatusBadRequest {
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
	if status, _ := postMock(t, server, "/v1/runtime/locate", nested, withCaseTicket); status != http.StatusBadRequest {
		t.Fatalf("locate accepted identity nested in data_needs: status %d, want 400", status)
	}

	// A detached verification purpose. The frozen coupling is bidirectional, so
	// the purpose without its all-or-nothing verification_binding is rejected —
	// a rule no field-name blocklist could ever express.
	detached := readBody(map[string]any{"purpose": "postcondition_verification"})
	if status, _ := postMock(t, server, "/v1/runtime/read", detached, withCaseTicket); status != http.StatusBadRequest {
		t.Fatalf("read accepted a detached verification purpose: status %d, want 400", status)
	}

	// Positive control: the rejections above mean nothing unless a
	// contract-clean request still succeeds.
	if status, _ := postMock(t, server, "/v1/runtime/locate", locateBody(nil), withCaseTicket); status != http.StatusOK {
		t.Fatalf("locate rejected a contract-clean request: status %d, want 200", status)
	}
	if status, _ := postMock(t, server, "/v1/runtime/read", readBody(nil), withCaseTicket); status != http.StatusOK {
		t.Fatalf("read rejected a contract-clean request: status %d, want 200", status)
	}
}

// TestMockNexusEvidenceSurfaceIsPerActorAuthorized pins WHO may call the
// frozen evidence surface, which the mock did not check at all.
//
// locateRuntimeEvidence and readRuntimeEvidence declare
// security [{browserSession}, {browserAccessToken}, {caseTicket}] and
// deliberately omit trustedServiceSecret: these are per-actor authorization
// surfaces, not service-to-service ones. Two distinct rules follow, and a mock
// that enforced only one would still be more permissive than production:
//
//  1. A request carrying NONE of the three is unauthenticated and gets 401.
//     This is the one that mattered — AgentAtlas was sending exactly that, and
//     because the mock answered 200 the whole e2e path looked healthy.
//  2. A request carrying the service credential is not thereby authorized,
//     because Basic is not an accepted scheme here. Accepting it would invite
//     the "fix" of putting a service credential on an actor-scoped surface.
func TestMockNexusEvidenceSurfaceIsPerActorAuthorized(t *testing.T) {
	server, _, _ := newConformanceServer(t)

	surfaces := []struct {
		path string
		body map[string]any
	}{
		{"/v1/runtime/locate", locateBody(nil)},
		{"/v1/runtime/read", readBody(nil)},
	}

	for _, surface := range surfaces {
		t.Run("anonymous "+surface.path, func(t *testing.T) {
			if status, _ := postMock(t, server, surface.path, surface.body, nil); status != http.StatusUnauthorized {
				t.Fatalf("anonymous %s: status %d, want 401", surface.path, status)
			}
		})

		// Not 200: a service credential does not authorize this surface. The
		// status is what a real gateway answers a caller it cannot resolve an
		// actor for.
		t.Run("service credential "+surface.path, func(t *testing.T) {
			if status, _ := postMock(t, server, surface.path, surface.body, withServiceCredential); status != http.StatusUnauthorized {
				t.Fatalf("%s accepted a service credential: status %d, want 401", surface.path, status)
			}
		})

		// An empty ticket is not a ticket. Without this, a client bug that
		// formatted the header but left the credential blank would pass.
		t.Run("empty ticket "+surface.path, func(t *testing.T) {
			blank := func(req *http.Request) { req.Header.Set("Authorization", "CaseTicket ") }
			if status, _ := postMock(t, server, surface.path, surface.body, blank); status != http.StatusUnauthorized {
				t.Fatalf("%s accepted an empty ticket: status %d, want 401", surface.path, status)
			}
		})

		// Positive controls: all three declared schemes are accepted. Asserting
		// only the rejections would pass against a handler that refused
		// everything, including the credentials AgentAtlas actually sends.
		for name, decorate := range map[string]func(*http.Request){
			"case ticket":     withCaseTicket,
			"browser token":   func(req *http.Request) { req.Header.Set("Authorization", "Bearer tok_conformance00001") },
			"browser session": withBrowserSession,
		} {
			t.Run(name+" "+surface.path, func(t *testing.T) {
				if status, _ := postMock(t, server, surface.path, surface.body, decorate); status != http.StatusOK {
					t.Fatalf("%s rejected an accepted credential (%s): status %d, want 200", surface.path, name, status)
				}
			})
		}
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

	status, located := postMock(t, server, "/v1/runtime/locate", locateBody(nil), withCaseTicket)
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

	status, read := postMock(t, server, "/v1/runtime/read", readBody(nil), withCaseTicket)
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
	if status, _ := postMock(t, server, "/v1/runtime/read", body, withCaseTicket); status != http.StatusForbidden {
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

// TestMockNexusTicketVerifyServesVerifyStepGrant pins POST /v1/tickets/verify
// to the operation the frozen snapshot actually declares there.
//
// This is the surface an earlier audit half-saw. It recorded that the mock
// served "the retired Case-Ticket shape" while the snapshot has verifyStepGrant,
// filed it as mock infidelity, and stopped — so the mock was reshaped toward
// what AgentAtlas's client sends and the client itself was never opened. The
// mismatch that mattered survived behind a mock that now agreed with it.
//
// So this test deliberately does NOT describe the mock in terms of what
// AgentAtlas sends. It describes verifyStepGrant, and AgentAtlas's VerifyTicket
// fails against it — see
// nexusclient.TestAccessTicketVerificationHasNoFrozenCounterpart, which is red
// on purpose. If this file is ever edited to make the deployed path green
// again, that is the blind spot reopening.
func TestMockNexusTicketVerifyServesVerifyStepGrant(t *testing.T) {
	server, _, _ := newConformanceServer(t)
	const path = "/v1/tickets/verify"

	// The frozen body: additionalProperties:false, required grant_ref +
	// capability + parameter_hash, each in its declared grammar.
	frozenBody := map[string]any{
		"grant_ref":      "grant_conformance00001",
		"capability":     "workflow.publish",
		"parameter_hash": conformanceHash,
	}
	// What AgentAtlas's HTTPClient.VerifyTicket puts on the wire today.
	retiredBody := map[string]any{"ticket_id": conformanceCaseTicket}

	// --- credential half -------------------------------------------------
	// verifyStepGrant declares browserSession, browserAccessToken and
	// caseTicket, and NOT trustedServiceSecret. Both rejections are asserted:
	// checking only that the service credential is refused is equally
	// satisfied by a handler that authenticates nobody, and checking only the
	// anonymous case is equally satisfied by one that accepts anything named.
	t.Run("anonymous", func(t *testing.T) {
		if status, _ := postMock(t, server, path, frozenBody, nil); status != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: status %d, want 401", path, status)
		}
	})
	t.Run("service credential", func(t *testing.T) {
		// This is the credential AgentAtlas presents today: doPost's allowlist
		// attaches Basic auth to this path.
		if status, _ := postMock(t, server, path, frozenBody, withServiceCredential); status != http.StatusUnauthorized {
			t.Fatalf("%s accepted a service credential: status %d, want 401", path, status)
		}
	})

	// --- body half -------------------------------------------------------
	// Isolated from the credential: every request below carries an accepted
	// Access Ticket, so a rejection can only be about the body.
	t.Run("retired Case-Ticket body", func(t *testing.T) {
		if status, _ := postMock(t, server, path, retiredBody, withCaseTicket); status != http.StatusBadRequest {
			t.Fatalf("%s accepted a {\"ticket_id\":...} body: status %d, want 400", path, status)
		}
	})
	t.Run("frozen body with an extra member", func(t *testing.T) {
		extended := map[string]any{"ticket_id": conformanceCaseTicket}
		for key, value := range frozenBody {
			extended[key] = value
		}
		if status, _ := postMock(t, server, path, extended, withCaseTicket); status != http.StatusBadRequest {
			t.Fatalf("%s accepted an unknown member alongside the frozen ones: status %d, want 400 "+
				"(StepGrantVerifyRequest is additionalProperties:false)", path, status)
		}
	})
	t.Run("malformed handle grammar", func(t *testing.T) {
		malformed := map[string]any{
			"grant_ref":      "tick_not_a_grant_handle",
			"capability":     "workflow.publish",
			"parameter_hash": conformanceHash,
		}
		if status, _ := postMock(t, server, path, malformed, withCaseTicket); status != http.StatusBadRequest {
			t.Fatalf("%s accepted a grant_ref outside ^grant_[A-Za-z0-9_-]{16,128}$: status %d, want 400", path, status)
		}
	})

	// --- positive control ------------------------------------------------
	// Without this, every assertion above would pass against a handler that
	// refused everything, and the mock would be useless rather than faithful.
	t.Run("accepted credential and frozen body", func(t *testing.T) {
		status, body := postMock(t, server, path, frozenBody, withCaseTicket)
		if status != http.StatusOK {
			t.Fatalf("%s rejected an accepted credential with the frozen body: status %d, want 200", path, status)
		}
		if valid, _ := body["valid"].(bool); !valid {
			t.Fatalf("%s: valid=%v, want true: %v", path, body["valid"], body)
		}
		// And the point of the whole exercise: success still carries no actor.
		// ticketGuard needs enterprise_id/actor_user_id/scopes; a Step Grant
		// verification answers with its own binding and nothing else, so no
		// amount of fixing the client turns this into the identity ticketGuard
		// is asking for.
		for _, member := range []string{"enterprise_id", "actor_user_id", "scopes"} {
			if _, present := body[member]; present {
				t.Fatalf("%s answered with %q; the mock is inventing an actor identity that "+
					"StepGrantVerifyResponse does not declare, which is exactly the infidelity that hid this defect: %v",
					path, member, body)
			}
		}
	})
}
