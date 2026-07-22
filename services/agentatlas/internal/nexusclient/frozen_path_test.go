package nexusclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// TestNexusClientCallsOnlyFrozenContractPaths is the behavioural half of
// ELC-NEXUS-1. The parity test next to it checks what AgentAtlas DECLARES it
// will call; this one checks what the client actually puts on the wire, and
// holds it against the same pinned AgentNexus contract snapshot. A path that
// does not exist in the published contract is a call that can only ever 404
// against a real AgentNexus.
//
// The Action surface is driven here too. It used not to be: the three methods
// that POSTed to /v1/actions/request, /v1/actions/receipt and
// /v1/actions/observation were never called, so the assertion passed vacuously
// for exactly the three endpoints that were off-contract, while the allowlist
// below listed different surfaces than the parity test's and the two claimed to
// mirror each other.
func TestNexusClientCallsOnlyFrozenContractPaths(t *testing.T) {
	var called []string
	bodies := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := templatizePath(r.URL.Path)
		called = append(called, path)
		var decoded map[string]any
		if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &decoded)
		}
		bodies[path] = decoded
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := New(server.URL, 5*time.Second, "agentatlas", writeServiceSecret(t, "frozen-path-service-secret-0123456789abcdef"), nil)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	ctx := context.Background()
	// Response verification failures are irrelevant here: the assertion is about
	// which endpoint the client reaches for, not what came back.
	// The Access Ticket must be non-empty for the same reason RequestAction
	// below must be structurally valid: ticketPost refuses an empty ticket
	// locally, so the call would never reach the wire and this surface would
	// silently go unexercised.
	_, _ = client.Locate(ctx, "tick_frozen_path", nexusruntime.EvidenceRequest{
		RequestID: "req-locate-0123456789abcdef",
		Purpose:   "answer_employee_question",
		DataNeeds: []nexusruntime.DataNeed{{
			NeedID:    "need-1",
			DataClass: "finance.voucher",
			Purpose:   "answer_employee_question",
		}},
		ExpiresAt: time.Now().Add(5 * time.Minute).UTC(),
	})
	_, _ = client.Read(ctx, "tick_frozen_path", nexusruntime.EvidenceReadRequest{
		RequestID:          "req-read-0123456789abcdef",
		BusinessContextRef: "wc_0123456789abcdef0123",
		EvidenceRef:        "evd_0123456789abcdef0123",
		Purpose:            "answer_employee_question",
		ExpiresAt:          time.Now().Add(5 * time.Minute).UTC(),
	})
	_, _ = client.VerifyTicket(ctx, nexus.VerifyTicketRequest{})
	_, _ = client.AppendAuditEvidence(ctx, nexus.AppendAuditEvidenceRequest{
		// A bindable business context, or the client refuses the append
		// locally and this surface goes unexercised — see requireAuditContext.
		BusinessContextRef: "wc_0123456789abcdef0123",
	})
	// The Action surface. RequestAction must be structurally valid or the
	// client rejects it before the network and the call never reaches the
	// wire — which is precisely how these three used to escape this test.
	actionReq := frozenPathActionRequest()
	_, _ = client.RequestAction(ctx, actionReq)
	_, _ = client.FetchActionReceipt(ctx, actionReq, "rcp_frozenpath00000001")
	// Dormant: no frozen observation surface exists, so this must add nothing
	// to called. Asserted explicitly below rather than left to inference.
	_, _ = client.FetchObservationReceipt(ctx, actionReq, "need-0001")

	published := openAPIPathSet(t, readOpenAPIMap(t, filepath.Join("testdata", publishedGatewayRuntimeSnapshot)))
	var offContract []string
	for _, path := range called {
		if !published[path] {
			offContract = append(offContract, path)
		}
	}
	if len(offContract) > 0 {
		sort.Strings(offContract)
		t.Fatalf("client called %d path(s) that do not exist in the frozen AgentNexus contract: %v", len(offContract), offContract)
	}
	// Every surface this test drives must actually have been observed. A method
	// that fails closed before the network is invisible to the loop above, so
	// without this the assertion silently stops covering it.
	for _, want := range []string{
		"/v1/runtime/locate", "/v1/runtime/read", "/v1/tickets/verify",
		"/v1/audit/evidence", "/v1/runtime/act", "/v1/runtime/receipts/{receipt_ref}",
	} {
		if !contains(called, want) {
			t.Fatalf("%s was never reached; the assertion passes vacuously for it (observed: %v)", want, called)
		}
	}
	if contains(called, "/v1/runtime/receipts/{receipt_ref}") && len(called) != 6 {
		t.Fatalf("unexpected call set %v; the dormant observation fetch must reach for nothing", called)
	}

	// A path rename alone would satisfy the check above while still putting a
	// body on the wire that the frozen contract refuses. Trusted identity is
	// credential-derived and connector topology never appears in this contract,
	// so neither may appear in any request AgentAtlas sends.
	//
	// notYetMigrated is a SHRINKING allowlist, not an exemption. Each entry is a
	// surface still on the retired vocabulary; removing an entry is the
	// definition of "this surface is migrated". The list is asserted to be
	// exact, so a surface that quietly regresses onto banned fields fails here
	// even though it is listed, and a surface that is migrated but left listed
	// fails too.
	//
	// /v1/audit/evidence has left this list: the frozen AuditEvidenceRequest is
	// additionalProperties:false and AgentNexus rejects a body carrying
	// enterprise_id outright, so the append now sends business_context_ref and
	// nothing identity-bearing. /v1/runtime/act has joined it: its path is
	// migrated but AgentAtlas's ActionRequest still declares actor and
	// org_scope, which AgentNexus re-derives from the verified credential.
	notYetMigrated := map[string]bool{
		"/v1/tickets/verify": true, // -> StepGrantVerifyRequest
		"/v1/runtime/act":    true, // -> frozen ActionRequest (business_context_ref, typed parameters, signed risk_decision)
	}
	var offending []string
	for path, body := range bodies {
		for _, banned := range []string{"ticket_id", "enterprise_id", "resource_uri", "actor", "org_scope"} {
			if _, present := body[banned]; present {
				offending = append(offending, path)
				break
			}
		}
	}
	sort.Strings(offending)
	for _, path := range offending {
		if !notYetMigrated[path] {
			t.Fatalf("%s puts trusted identity or connector topology on the wire; the frozen contract refuses it", path)
		}
		delete(notYetMigrated, path)
	}
	for path := range notYetMigrated {
		t.Fatalf("%s is listed as not-yet-migrated but no longer sends banned fields; remove it from the allowlist", path)
	}
}

// templatizePath maps a concrete request path back onto the templated path the
// frozen contract declares, so a GET of /v1/runtime/receipts/rcp_abc is checked
// against /v1/runtime/receipts/{receipt_ref} rather than reported as missing.
func templatizePath(path string) string {
	if rest, ok := strings.CutPrefix(path, "/v1/runtime/receipts/"); ok && rest != "" {
		return "/v1/runtime/receipts/{receipt_ref}"
	}
	return path
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// frozenPathActionRequest is the minimum structurally valid ActionRequest:
// RequestAction and FetchActionReceipt both call Validate before the network,
// so an invalid fixture would make this test pass without either method ever
// reaching for an endpoint.
func frozenPathActionRequest() nexus.ActionRequest {
	return nexus.ActionRequest{
		ActionID:               "act-frozen-path-0001",
		GoalRef:                "goal-0001",
		OutcomeRef:             "outcome-0001",
		WorkCaseID:             "case-0001",
		WorkPlanRevision:       1,
		Actor:                  "actor-alice",
		OrgScope:               "org-1",
		Capability:             "erp.purchase_order.approve",
		ParameterHash:          "sha256:" + fixtureHex('a'),
		Risk:                   nexus.RiskMedium,
		IdempotencyKey:         "idem-key-frozen-path-01",
		ExpiresAt:              time.Now().Add(time.Hour),
		ExecutionReceiptSchema: "erp.purchase_order.approve.v1",
	}
}
