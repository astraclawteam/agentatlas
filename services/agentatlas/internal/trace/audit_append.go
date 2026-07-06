package trace

import (
	"context"
	"fmt"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// AppendAudit pushes the trace onto the AgentNexus audit chain and records
// the returned reference. Failure is a hard error: callers must fail the
// answer path rather than serve an unaudited answer.
func (s *Service) AppendAudit(ctx context.Context, client nexus.Client, ticketID string, traceRow db.AnswerTrace) (string, error) {
	resp, err := client.AppendAuditEvidence(ctx, nexus.AppendAuditEvidenceRequest{
		TicketID:     ticketID,
		EnterpriseID: traceRow.EnterpriseID,
		Action:       nexus.AuditAnswerTraceCreated,
		TraceID:      traceRow.ID,
		Details: map[string]any{
			"question_hash": traceRow.QuestionHash,
			"answer_hash":   traceRow.AnswerHash,
			"space_ids":     traceRow.SpaceIds,
			"evidence_ids":  traceRow.EvidencePointerIds,
			"model_route":   traceRow.ModelRoute,
		},
	})
	if err != nil {
		return "", fmt.Errorf("audit append failed (answer must fail closed): %w", err)
	}
	if resp.AuditRefID == "" {
		return "", fmt.Errorf("audit append returned empty ref (fail closed)")
	}
	if err := s.store.InsertAnswerTraceAuditRef(ctx, db.InsertAnswerTraceAuditRefParams{
		TraceID: traceRow.ID, AuditRefID: resp.AuditRefID,
	}); err != nil {
		return "", fmt.Errorf("record audit ref: %w", err)
	}
	return resp.AuditRefID, nil
}
