package workflow

import (
	"context"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

type fakeWorkflowEvidence struct {
	locates []nexusruntime.EvidenceRequest
	reads   []nexusruntime.EvidenceReadRequest
}

func (f *fakeWorkflowEvidence) Locate(_ context.Context, req nexusruntime.EvidenceRequest) (nexusclient.LocateEvidenceResult, error) {
	f.locates = append(f.locates, req)
	return nexusclient.LocateEvidenceResult{
		BusinessContextRef: "wc_0123456789abcdef0123",
		Evidence: []nexusruntime.EvidenceHandle{{
			EvidenceRef: "evd_0123456789abcdef0123",
			DataClass:   workflowEvidenceDataClass,
		}},
	}, nil
}

func (f *fakeWorkflowEvidence) Read(_ context.Context, req nexusruntime.EvidenceReadRequest) (nexusclient.ReadEvidenceResult, error) {
	f.reads = append(f.reads, req)
	return nexusclient.ReadEvidenceResult{
		Decision: "allow", GrantRef: "grn_bound",
		Data:          map[string]any{"detail": "sealed-detail"},
		SourceVersion: 7, AsOf: "2026-07-21T00:00:00Z", ServedFromCache: false,
	}, nil
}
