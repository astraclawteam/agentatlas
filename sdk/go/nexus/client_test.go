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
func (fakeClient) LocateEvidence(context.Context, LocateEvidenceRequest) (LocateEvidenceResponse, error) {
	return LocateEvidenceResponse{}, nil
}
func (fakeClient) ReadEvidence(context.Context, ReadEvidenceRequest) (ReadEvidenceResponse, error) {
	return ReadEvidenceResponse{}, nil
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

func TestAppendAuditEvidenceMatchesPublishedAgentNexusContract(t *testing.T) {
	req := AppendAuditEvidenceRequest{
		TicketID: "ticket", EnterpriseID: "ent", Action: AuditDreamPolicyCreateRequested,
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
	wantKeys := []string{"action", "details", "enterprise_id", "resource_id", "resource_type", "ticket_id", "trace_id"}
	gotKeys := make([]string, 0, len(body))
	for key := range body {
		gotKeys = append(gotKeys, key)
	}
	slicesSort(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) || body["action"] != "dream_policy_create_requested" || body["resource_type"] != "dream_policy" || body["resource_id"] != "policy-1" {
		t.Fatalf("body=%s", raw)
	}
	if _, exists := body["workflow_run_id"]; exists {
		t.Fatalf("unpersisted workflow_run_id emitted: %s", raw)
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
	if _, exists := emptyBody["ticket_id"]; exists {
		t.Fatalf("optional browser bearer ticket_id must be omitted when empty: %s", emptyRaw)
	}
	for _, required := range []string{"enterprise_id", "action", "resource_type", "resource_id"} {
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
