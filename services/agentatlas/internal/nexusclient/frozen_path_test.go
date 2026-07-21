package nexusclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
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
func TestNexusClientCallsOnlyFrozenContractPaths(t *testing.T) {
	var called []string
	bodies := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.URL.Path)
		var decoded map[string]any
		if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &decoded)
		}
		bodies[r.URL.Path] = decoded
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
	_, _ = client.Locate(ctx, nexusruntime.EvidenceRequest{
		RequestID: "req-locate-0123456789abcdef",
		Purpose:   "answer_employee_question",
		DataNeeds: []nexusruntime.DataNeed{{
			NeedID:    "need-1",
			DataClass: "finance.voucher",
			Purpose:   "answer_employee_question",
		}},
		ExpiresAt: time.Now().Add(5 * time.Minute).UTC(),
	})
	_, _ = client.Read(ctx, nexusruntime.EvidenceReadRequest{
		RequestID:          "req-read-0123456789abcdef",
		BusinessContextRef: "wc_0123456789abcdef0123",
		EvidenceRef:        "evd_0123456789abcdef0123",
		Purpose:            "answer_employee_question",
		ExpiresAt:          time.Now().Add(5 * time.Minute).UTC(),
	})
	_, _ = client.VerifyTicket(ctx, nexus.VerifyTicketRequest{})
	_, _ = client.AppendAuditEvidence(ctx, nexus.AppendAuditEvidenceRequest{})

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
	if len(called) == 0 {
		t.Fatal("no client call was observed; the test would pass vacuously")
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
	notYetMigrated := map[string]bool{
		"/v1/tickets/verify": true, // -> StepGrantVerifyRequest
		"/v1/audit/evidence": true, // -> frozen AuditEvidence vocabulary
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
