package workflow

import (
	"context"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

type fakeWorkflowEvidence struct {
	locates []nexusruntime.EvidenceRequest
	reads   []nexusruntime.EvidenceReadRequest
	// deny makes Read answer the way AgentNexus answers a refusal: a
	// SUCCESSFUL 200 carrying decision "deny" and no data.
	deny bool
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
