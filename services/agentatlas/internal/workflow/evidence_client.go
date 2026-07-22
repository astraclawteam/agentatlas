package workflow

import (
	"context"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	nexusruntime "github.com/astraclawteam/agentnexus/sdk/go/runtime"
)

// EvidenceClient is the frozen-contract evidence surface the nexus.locate and
// nexus.read nodes use.
//
// It is declared here rather than shared with internal/app because app already
// imports this package; a shared declaration would invert that dependency. A
// two-method consumer-side interface is the idiomatic Go answer to that, and it
// keeps each package honest about exactly what it needs.
// Both methods take the caller's verified Access Ticket; see the identically
// shaped app.FrozenEvidenceClient for why it is required rather than optional.
type EvidenceClient interface {
	Locate(ctx context.Context, ticketID string, req nexusruntime.EvidenceRequest) (nexusclient.LocateEvidenceResult, error)
	Read(ctx context.Context, ticketID string, req nexusruntime.EvidenceReadRequest) (nexusclient.ReadEvidenceResult, error)
}

const (
	workflowEvidenceDataClass = "workflow.evidence"
	workflowEvidencePurpose   = "workflow_node_evidence_read"
	// workflowEvidenceTTL bounds a located request. The frozen contract requires
	// an explicit expiry; a workflow node must not hold an open-ended one.
	workflowEvidenceTTL = 5 * time.Minute
)
