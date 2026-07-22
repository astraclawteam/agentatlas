package nexus

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// fakeClient proves the interface is implementable with value semantics.
type fakeClient struct{}

func (fakeClient) VerifyTicket(_ context.Context, req VerifyTicketRequest) (VerifyTicketResponse, error) {
	return VerifyTicketResponse{Valid: req.TicketID != ""}, nil
}
func (fakeClient) AppendAuditEvidence(context.Context, AppendAuditEvidenceRequest) (AppendAuditEvidenceResponse, error) {
	return AppendAuditEvidenceResponse{AuditRefID: "ref"}, nil
}
func (fakeClient) SubscribeOrgEvents(context.Context, string, int64, OrgEventHandler) error {
	return nil
}

var _ Client = fakeClient{}

func TestVerifyTicketContract(t *testing.T) {
	var c Client = fakeClient{}
	resp, err := c.VerifyTicket(context.Background(), VerifyTicketRequest{TicketID: "t1"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !resp.Valid {
		t.Fatal("expected valid ticket")
	}
	empty, err := c.VerifyTicket(context.Background(), VerifyTicketRequest{})
	if err != nil {
		t.Fatalf("verify empty: %v", err)
	}
	if empty.Valid {
		t.Fatal("empty ticket must be invalid")
	}
}

// TestAppendAuditEvidenceMatchesPublishedAgentNexusContract pins the exact key
// set that reaches the wire. The frozen AuditEvidenceRequest is
// additionalProperties:false AND AgentNexus decodes it under an allowed-key
// check that rejects trusted-identity members outright, so an extra key is not
// an ignored field — it fails the whole append. An absent required key fails it
// too. Both directions are asserted.
func TestAppendAuditEvidenceMatchesPublishedAgentNexusContract(t *testing.T) {
	req := AppendAuditEvidenceRequest{
		RequestID: "req-1", BusinessContextRef: "wc_0123456789abcdef0123", Action: AuditDreamPolicyCreateRequested,
		ResourceType: "dream_policy", ResourceID: "policy-1", TraceID: "trace-1",
		Details: map[string]any{"phase": "create_requested"},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"action", "business_context_ref", "details", "request_id", "resource_id", "resource_type", "trace_id"}
	gotKeys := make([]string, 0, len(body))
	for key := range body {
		gotKeys = append(gotKeys, key)
	}
	slicesSort(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) || body["action"] != "dream_policy_create_requested" || body["resource_type"] != "dream_policy" || body["resource_id"] != "policy-1" {
		t.Fatalf("body=%s", raw)
	}
	// The tenant and the actor are derived from the verified credential. Either
	// one in the body is a 400 from AgentNexus, not a field it ignores.
	for _, banned := range []string{"enterprise_id", "ticket_id", "actor_user_id", "org_version", "org_unit_id", "authorized_action", "review_mode", "queue", "workflow_run_id"} {
		if _, exists := body[banned]; exists {
			t.Fatalf("%q is not a member of the frozen AuditEvidenceRequest: %s", banned, raw)
		}
	}
	if AuditDreamPolicyCreated != "dream_policy_created" {
		t.Fatal("legacy dream_policy_created action changed")
	}
	emptyRaw, err := json.Marshal(AppendAuditEvidenceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var emptyBody map[string]any
	if err := json.Unmarshal(emptyRaw, &emptyBody); err != nil {
		t.Fatal(err)
	}
	if _, exists := emptyBody["request_id"]; exists {
		t.Fatalf("optional request_id must be omitted when empty: %s", emptyRaw)
	}
	for _, required := range []string{"business_context_ref", "action", "resource_type", "resource_id"} {
		if _, exists := emptyBody[required]; !exists {
			t.Fatalf("required field %q has omitempty tag: %s", required, emptyRaw)
		}
	}
}

func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
