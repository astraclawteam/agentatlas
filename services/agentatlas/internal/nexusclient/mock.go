package nexusclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

// Mock is an in-memory nexus.Client for unit tests. Configure state, run the
// code under test, then inspect AuditLog / calls.
type Mock struct {
	mu sync.Mutex

	// evidence is the frozen-contract fixture: need id -> detail.
	evidence map[string]string

	Tickets   map[string]nexus.VerifyTicketResponse // ticket_id -> response
	OrgEvents []nexus.OrgEvent

	AuditLog      []nexus.AppendAuditEvidenceRequest
	auditReceipts map[string]mockAuditReceipt
}

type mockAuditReceipt struct {
	hash string
	ref  string
}

var _ nexus.Client = (*Mock)(nil)

func NewMock() *Mock {
	return &Mock{
		Tickets: map[string]nexus.VerifyTicketResponse{},
	}
}

func (m *Mock) VerifyTicket(_ context.Context, req nexus.VerifyTicketRequest) (nexus.VerifyTicketResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if resp, ok := m.Tickets[req.TicketID]; ok {
		return resp, nil
	}
	return nexus.VerifyTicketResponse{Valid: false}, nil
}

func (m *Mock) AppendAuditEvidence(_ context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if req.IdempotencyKey != "" {
		if m.auditReceipts == nil {
			m.auditReceipts = map[string]mockAuditReceipt{}
		}
		raw, _ := json.Marshal(req)
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		if receipt, ok := m.auditReceipts[req.BusinessContextRef+"\x00"+req.IdempotencyKey]; ok {
			if receipt.hash != hash {
				return nexus.AppendAuditEvidenceResponse{}, fmt.Errorf("audit idempotency payload mismatch")
			}
			return nexus.AppendAuditEvidenceResponse{AuditRefID: receipt.ref}, nil
		}
		ref := fmt.Sprintf("audit_%d", len(m.AuditLog)+1)
		m.AuditLog = append(m.AuditLog, req)
		m.auditReceipts[req.BusinessContextRef+"\x00"+req.IdempotencyKey] = mockAuditReceipt{hash: hash, ref: ref}
		return nexus.AppendAuditEvidenceResponse{AuditRefID: ref}, nil
	}
	m.AuditLog = append(m.AuditLog, req)
	return nexus.AppendAuditEvidenceResponse{AuditRefID: fmt.Sprintf("audit_%d", len(m.AuditLog))}, nil
}

// SubscribeOrgEvents replays queued events newer than sinceVersion, then
// returns nil (stream end). Callers resume with the last processed version.
func (m *Mock) SubscribeOrgEvents(ctx context.Context, enterpriseID string, sinceVersion int64, handler nexus.OrgEventHandler) error {
	m.mu.Lock()
	events := make([]nexus.OrgEvent, 0, len(m.OrgEvents))
	// Filter on the cursor only. The frozen event carries no enterprise id, and
	// the real feed is already scoped to the tenant of the verified service
	// credential, so filtering by enterpriseID here would be a fidelity bug:
	// the double would drop events the real server would deliver.
	for _, ev := range m.OrgEvents {
		if ev.OrgVersion > sinceVersion {
			events = append(events, ev)
		}
	}
	m.mu.Unlock()

	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := handler(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// Evidence is the frozen-contract evidence fixture: need id -> detail. A need
// absent from the map is simply not located, which the frozen contract models
// as an empty handle list rather than as a denial.
func (m *Mock) SetEvidence(needID, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.evidence == nil {
		m.evidence = map[string]string{}
	}
	m.evidence[needID] = detail
}

// EvidenceAllowed reports whether a declared need resolves to readable
// evidence for this fixture.
func (m *Mock) EvidenceAllowed(needID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.evidence[needID]
	return ok
}

// EvidenceDetail resolves an opaque handle back to its fixture detail. The
// handle grammar is evd_<padded need id>, so the mock can round-trip without
// keeping a second index.
func (m *Mock) EvidenceDetail(evidenceRef string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	trimmed := strings.TrimPrefix(evidenceRef, "evd_")
	for needID, detail := range m.evidence {
		padded := needID
		for len(padded) < 16 {
			padded += "0"
		}
		if padded == trimmed {
			return detail, true
		}
	}
	return "", false
}
