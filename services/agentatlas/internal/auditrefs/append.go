package auditrefs

import (
	"context"
	"fmt"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
)

// Appender decorates a nexus.Client: every audit append failure increments
// the fail-closed counter before the error propagates. All other client
// methods pass through unchanged.
type Appender struct {
	nexus.Client
	metrics *observability.Metrics
}

var _ nexus.Client = (*Appender)(nil)

func New(client nexus.Client, metrics *observability.Metrics) *Appender {
	return &Appender{Client: client, metrics: metrics}
}

func (a *Appender) AppendAuditEvidence(ctx context.Context, req nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	resp, err := a.Client.AppendAuditEvidence(ctx, req)
	if err != nil || resp.AuditRefID == "" {
		if a.metrics != nil {
			a.metrics.AuditAppendFail.Inc()
		}
		if err == nil {
			err = fmt.Errorf("audit append returned empty ref")
		}
		return nexus.AppendAuditEvidenceResponse{}, err
	}
	return resp, nil
}
