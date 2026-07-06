package trace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// Store is the persistence surface (db/generated satisfies it).
type Store interface {
	CreateAnswerTrace(ctx context.Context, arg db.CreateAnswerTraceParams) (db.AnswerTrace, error)
	GetAnswerTrace(ctx context.Context, id string) (db.AnswerTrace, error)
	InsertAnswerTraceStep(ctx context.Context, arg db.InsertAnswerTraceStepParams) error
	InsertAnswerTraceEvidence(ctx context.Context, arg db.InsertAnswerTraceEvidenceParams) error
	InsertAnswerTraceModelEvent(ctx context.Context, arg db.InsertAnswerTraceModelEventParams) error
	InsertAnswerTraceAuditRef(ctx context.Context, arg db.InsertAnswerTraceAuditRefParams) error
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// Create persists the trace with steps, evidence links, and the model event.
func (s *Service) Create(ctx context.Context, rec Record) (db.AnswerTrace, error) {
	if rec.EnterpriseID == "" || rec.CaseTicketID == "" {
		return db.AnswerTrace{}, fmt.Errorf("trace: enterprise_id and case_ticket_id are required")
	}
	row, err := s.store.CreateAnswerTrace(ctx, db.CreateAnswerTraceParams{
		ID:                       newID("tr"),
		EnterpriseID:             rec.EnterpriseID,
		CaseTicketID:             rec.CaseTicketID,
		ActorUserID:              rec.ActorUserID,
		QuestionHash:             HashText(rec.Question),
		SanitizedQuestionSummary: truncateRunes(rec.SanitizedQuestionSummary, 1000),
		WorkflowRunID:            pgTextOrNull(rec.WorkflowRunID),
		SpaceIds:                 orEmpty(rec.SpaceIDs),
		RetrievalPlanID:          pgTextOrNull(rec.RetrievalPlanID),
		EvidencePointerIds:       orEmpty(rec.EvidencePointerIDs),
		AgentnexusReadGrantIds:   orEmpty(rec.ReadGrantIDs),
		ModelRoute:               rec.ModelRoute,
		AnswerHash:               HashText(rec.Answer),
	})
	if err != nil {
		return db.AnswerTrace{}, fmt.Errorf("trace: %w", err)
	}
	for i, step := range rec.Steps {
		detail, err := json.Marshal(step.Detail)
		if err != nil {
			return db.AnswerTrace{}, err
		}
		if detail == nil || string(detail) == "null" {
			detail = []byte(`{}`)
		}
		if err := s.store.InsertAnswerTraceStep(ctx, db.InsertAnswerTraceStepParams{
			TraceID: row.ID, StepNo: int32(i + 1), Kind: step.Kind, Detail: detail,
		}); err != nil {
			return db.AnswerTrace{}, fmt.Errorf("trace step %d: %w", i+1, err)
		}
	}
	for i, ev := range rec.EvidencePointerIDs {
		grant := ""
		if i < len(rec.ReadGrantIDs) {
			grant = rec.ReadGrantIDs[i]
		}
		if err := s.store.InsertAnswerTraceEvidence(ctx, db.InsertAnswerTraceEvidenceParams{
			TraceID: row.ID, EvidencePointerID: ev, GrantID: grant,
		}); err != nil {
			return db.AnswerTrace{}, fmt.Errorf("trace evidence: %w", err)
		}
	}
	if rec.ModelRoute != "" {
		if err := s.store.InsertAnswerTraceModelEvent(ctx, db.InsertAnswerTraceModelEventParams{
			TraceID: row.ID, ModelRoute: rec.ModelRoute,
			PromptHash: HashText(rec.Question), Usage: []byte(`{}`),
			LatencyMs: int32(rec.ModelLatencyMS),
		}); err != nil {
			return db.AnswerTrace{}, fmt.Errorf("trace model event: %w", err)
		}
	}
	return row, nil
}

func (s *Service) Get(ctx context.Context, id string) (db.AnswerTrace, error) {
	return s.store.GetAnswerTrace(ctx, id)
}

func pgTextOrNull(v string) pgtype.Text {
	return pgtype.Text{String: v, Valid: v != ""}
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
