package app

import (
	"context"

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
	locErr  error
	readErr error
	// empty makes Locate return no handles.
	empty bool
	// deny makes Read answer the way AgentNexus answers a refusal: a
	// SUCCESSFUL 200 carrying decision "deny" and no data. Modelling it as an
	// error would have hidden the bug this knob exists to catch.
	deny bool
}

func (f *fakeFrozenEvidence) Locate(_ context.Context, req nexusruntime.EvidenceRequest) (nexusclient.LocateEvidenceResult, error) {
	f.locates = append(f.locates, req)
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

func (f *fakeFrozenEvidence) Read(_ context.Context, req nexusruntime.EvidenceReadRequest) (nexusclient.ReadEvidenceResult, error) {
	f.reads = append(f.reads, req)
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
