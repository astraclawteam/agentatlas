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
	return nexusclient.ReadEvidenceResult{
		Decision:        "allow",
		GrantRef:        "grn_bound",
		Data:            map[string]any{"detail": "sealed-detail"},
		ReceiptRef:      "rcp_0123456789abcdef0123",
		SourceVersion:   7,
		AsOf:            "2026-07-21T00:00:00Z",
		ServedFromCache: false,
	}, nil
}
