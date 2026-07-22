package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// fakeFrozenEvidence is the frozen-contract evidence double.
//
// It deliberately returns the freshness trio on every allowed read: the
// contract states those on every allowed read so a cached answer can never
// masquerade as a live one, and a double that omitted them would let a
// regression that drops the disclosure pass unnoticed.
type fakeFrozenEvidence struct {
	locates []nexusruntime.EvidenceRequest
	reads   []nexusruntime.EvidenceReadRequest
	// tickets records the Access Ticket presented with each call, in call
	// order across both methods. The frozen locate/read operations accept
	// only per-actor credentials, so a handler that forwards no ticket makes
	// a request a real AgentNexus answers 401 — recording it here is what
	// lets a test assert the forwarding actually happens.
	tickets []string
	locErr  error
	readErr error
	// empty makes Locate return no handles.
	empty bool
	// deny makes Read answer the way AgentNexus answers a refusal: a
	// SUCCESSFUL 200 carrying decision "deny" and no data. Modelling it as an
	// error would have hidden the bug this knob exists to catch.
	deny bool
}

// refuseWithoutTicket mirrors the real client's ticketPost: locate and read
// accept only per-actor credentials, so an empty Access Ticket is refused
// locally and never reaches the wire.
//
// A double that accepted one would be more permissive than the client it
// stands in for, which is the same trap the e2e mock had — and it is not
// hypothetical here: with this double silently accepting "", every handler
// test passed against a handler that forwarded no ticket at all.
func refuseWithoutTicket(ticketID string) error {
	if strings.TrimSpace(ticketID) == "" {
		return fmt.Errorf("nexus evidence: %w", nexusclient.ErrMissingCaseTicket)
	}
	return nil
}

func (f *fakeFrozenEvidence) Locate(_ context.Context, ticketID string, req nexusruntime.EvidenceRequest) (nexusclient.LocateEvidenceResult, error) {
	f.locates = append(f.locates, req)
	f.tickets = append(f.tickets, ticketID)
	if err := refuseWithoutTicket(ticketID); err != nil {
		return nexusclient.LocateEvidenceResult{}, err
	}
	if f.locErr != nil {
		return nexusclient.LocateEvidenceResult{}, f.locErr
	}
	if f.empty {
		return nexusclient.LocateEvidenceResult{BusinessContextRef: "wc_0123456789abcdef0123"}, nil
	}
	return nexusclient.LocateEvidenceResult{
		BusinessContextRef: "wc_0123456789abcdef0123",
		Evidence: []nexusruntime.EvidenceHandle{{
			EvidenceRef: "evd_0123456789abcdef0123",
			DataClass:   dreamEvidenceDataClass,
		}},
	}, nil
}

func (f *fakeFrozenEvidence) Read(_ context.Context, ticketID string, req nexusruntime.EvidenceReadRequest) (nexusclient.ReadEvidenceResult, error) {
	f.reads = append(f.reads, req)
	f.tickets = append(f.tickets, ticketID)
	if err := refuseWithoutTicket(ticketID); err != nil {
		return nexusclient.ReadEvidenceResult{}, err
	}
	if f.readErr != nil {
		return nexusclient.ReadEvidenceResult{}, f.readErr
	}
	if f.deny {
		return nexusclient.ReadEvidenceResult{Decision: nexusclient.ReadDeny}, nil
	}
	// No GrantRef: the real AgentNexus read handler never emits one, and a
	// double that invents one is precisely what kept two handlers dead in
	// production while their tests passed.
	return nexusclient.ReadEvidenceResult{
		Decision:        nexusclient.ReadAllow,
		Data:            map[string]any{"detail": "sealed-detail"},
		ReceiptRef:      "rcp_0123456789abcdef0123",
		SourceVersion:   7,
		AsOf:            "2026-07-21T00:00:00Z",
		ServedFromCache: false,
	}, nil
}
