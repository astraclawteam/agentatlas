package nexus

import (
	"context"
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
