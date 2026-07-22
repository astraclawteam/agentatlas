package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

type fakeWorkflowEvidence struct {
	locates []nexusruntime.EvidenceRequest
	reads   []nexusruntime.EvidenceReadRequest
	// tickets records the Access Ticket presented with each call, in call
	// order across both methods.
	tickets []string
	// deny makes Read answer the way AgentNexus answers a refusal: a
	// SUCCESSFUL 200 carrying decision "deny" and no data.
	deny bool
}

// refuseWithoutTicket mirrors the real client's ticketPost: the per-actor
// evidence surface accepts no service credential, so an empty Access Ticket is
// refused locally and never reaches the wire. A double that accepted one would
// be more permissive than the client it stands in for.
func refuseWithoutTicket(ticketID string) error {
	if strings.TrimSpace(ticketID) == "" {
		return fmt.Errorf("nexus evidence: %w", nexusclient.ErrMissingCaseTicket)
	}
	return nil
}

func (f *fakeWorkflowEvidence) Locate(_ context.Context, ticketID string, req nexusruntime.EvidenceRequest) (nexusclient.LocateEvidenceResult, error) {
	f.locates = append(f.locates, req)
	f.tickets = append(f.tickets, ticketID)
	if err := refuseWithoutTicket(ticketID); err != nil {
		return nexusclient.LocateEvidenceResult{}, err
	}
	return nexusclient.LocateEvidenceResult{
		BusinessContextRef: "wc_0123456789abcdef0123",
		Evidence: []nexusruntime.EvidenceHandle{{
			EvidenceRef: "evd_0123456789abcdef0123",
			DataClass:   workflowEvidenceDataClass,
		}},
	}, nil
}

func (f *fakeWorkflowEvidence) Read(_ context.Context, ticketID string, req nexusruntime.EvidenceReadRequest) (nexusclient.ReadEvidenceResult, error) {
	f.reads = append(f.reads, req)
	f.tickets = append(f.tickets, ticketID)
	if err := refuseWithoutTicket(ticketID); err != nil {
		return nexusclient.ReadEvidenceResult{}, err
	}
	if f.deny {
		return nexusclient.ReadEvidenceResult{Decision: nexusclient.ReadDeny}, nil
	}
	// No GrantRef: the real read handler never emits one.
	return nexusclient.ReadEvidenceResult{
		Decision:      nexusclient.ReadAllow,
		Data:          map[string]any{"detail": "sealed-detail"},
		SourceVersion: 7, AsOf: "2026-07-21T00:00:00Z", ServedFromCache: false,
	}, nil
}
