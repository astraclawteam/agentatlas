package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
)

// Store is the persistence surface the workflow runtime needs; db/generated
// Queries satisfies it.
type Store interface {
	CreateWorkflowDraft(ctx context.Context, arg db.CreateWorkflowDraftParams) (db.Workflow, error)
	UpdateWorkflowDraft(ctx context.Context, arg db.UpdateWorkflowDraftParams) (int64, error)
	GetWorkflow(ctx context.Context, id string) (db.Workflow, error)
	PublishWorkflowVersion(ctx context.Context, arg db.PublishWorkflowVersionParams) (db.WorkflowVersion, error)
	GetWorkflowVersion(ctx context.Context, arg db.GetWorkflowVersionParams) (db.WorkflowVersion, error)
	GetLatestWorkflowVersion(ctx context.Context, workflowID string) (db.WorkflowVersion, error)
	InsertWorkflowNode(ctx context.Context, arg db.InsertWorkflowNodeParams) error
	InsertWorkflowEdge(ctx context.Context, arg db.InsertWorkflowEdgeParams) error
	CreateWorkflowRun(ctx context.Context, arg db.CreateWorkflowRunParams) (db.WorkflowRun, error)
	UpdateWorkflowRunStatus(ctx context.Context, arg db.UpdateWorkflowRunStatusParams) (int64, error)
	GetWorkflowRun(ctx context.Context, id string) (db.WorkflowRun, error)
	InsertWorkflowRunEvent(ctx context.Context, arg db.InsertWorkflowRunEventParams) (db.WorkflowRunEvent, error)
	ListWorkflowRunEvents(ctx context.Context, runID string) ([]db.WorkflowRunEvent, error)
}

type DraftView struct {
	ID, EnterpriseID, Name, Kind string
	Definition                   Definition
	UpdatedAt                    time.Time
	LatestVersion                int32
}

func (s *Service) GetDraft(ctx context.Context, enterpriseID, workflowID string) (DraftView, error) {
	row, err := s.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return DraftView{}, err
	}
	if row.EnterpriseID != enterpriseID {
		return DraftView{}, ErrWorkflowForbidden
	}
	var def Definition
	if json.Unmarshal(row.Draft, &def) != nil {
		return DraftView{}, fmt.Errorf("decode draft")
	}
	latestVersion := int32(0)
	if latest, latestErr := s.store.GetLatestWorkflowVersion(ctx, workflowID); latestErr == nil {
		latestVersion = latest.Version
	} else if !errors.Is(latestErr, pgx.ErrNoRows) {
		return DraftView{}, latestErr
	}
	return DraftView{ID: row.ID, EnterpriseID: row.EnterpriseID, Name: row.Name, Kind: row.Kind, Definition: def, UpdatedAt: row.DraftUpdatedAt.Time, LatestVersion: latestVersion}, nil
}
func (s *Service) ListDrafts(ctx context.Context, enterpriseID string, limit int32) ([]DraftView, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	lister, ok := s.store.(interface {
		ListWorkflowsByEnterprise(context.Context, db.ListWorkflowsByEnterpriseParams) ([]db.Workflow, error)
	})
	if !ok {
		return nil, fmt.Errorf("workflow listing unavailable")
	}
	rows, err := lister.ListWorkflowsByEnterprise(ctx, db.ListWorkflowsByEnterpriseParams{EnterpriseID: enterpriseID, Limit: limit + 1})
	if err != nil {
		return nil, err
	}
	if len(rows) > int(limit) {
		return nil, fmt.Errorf("workflow bounded read exceeded %d", limit)
	}
	out := make([]DraftView, 0, len(rows))
	for _, row := range rows {
		view, viewErr := s.GetDraft(ctx, enterpriseID, row.ID)
		if viewErr != nil {
			return nil, viewErr
		}
		out = append(out, view)
	}
	return out, nil
}

type Service struct {
	store     Store
	validator *Validator
}

func NewService(store Store) (*Service, error) {
	v, err := NewValidator()
	if err != nil {
		return nil, err
	}
	return &Service{store: store, validator: v}, nil
}

func (s *Service) Validator() *Validator { return s.validator }

// CreateDraft validates and stores a new workflow draft.
func (s *Service) CreateDraft(ctx context.Context, enterpriseID, name, createdBy string, def Definition) (string, error) {
	if def.WorkflowID == "" {
		def.WorkflowID = newID("wf")
	}
	def.Version = 0
	if err := s.validator.Validate(def); err != nil {
		return "", err
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("encode draft: %w", err)
	}
	wf, err := s.store.CreateWorkflowDraft(ctx, db.CreateWorkflowDraftParams{
		ID: def.WorkflowID, EnterpriseID: enterpriseID, Name: name,
		Kind: string(def.Kind), CreatedBy: createdBy, Draft: raw,
	})
	if err != nil {
		return "", fmt.Errorf("store draft: %w", err)
	}
	return wf.ID, nil
}

// UpdateDraft replaces the draft after validation.
func (s *Service) UpdateDraft(ctx context.Context, enterpriseID, workflowID string, def Definition) error {
	def.WorkflowID = workflowID
	def.Version = 0
	if err := s.validator.Validate(def); err != nil {
		return err
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("encode draft: %w", err)
	}
	rows, err := s.store.UpdateWorkflowDraft(ctx, db.UpdateWorkflowDraftParams{
		ID: workflowID, EnterpriseID: enterpriseID, Draft: raw,
	})
	if err != nil {
		return fmt.Errorf("update draft: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("workflow %s not found for enterprise %s", workflowID, enterpriseID)
	}
	return nil
}

// Publish freezes the current draft as the next immutable version and
// materializes node/edge rows for queryability. Runs bind to versions only.
func (s *Service) Publish(ctx context.Context, enterpriseID, workflowID, publishedBy string) (int32, error) {
	wf, err := s.store.GetWorkflow(ctx, workflowID)
	if err != nil {
		return 0, fmt.Errorf("load workflow %s: %w", workflowID, err)
	}
	if wf.EnterpriseID != enterpriseID {
		return 0, fmt.Errorf("workflow %s not found for enterprise %s", workflowID, enterpriseID)
	}
	var def Definition
	if err := json.Unmarshal(wf.Draft, &def); err != nil {
		return 0, fmt.Errorf("decode draft: %w", err)
	}

	next := int32(1)
	latest, err := s.store.GetLatestWorkflowVersion(ctx, workflowID)
	switch {
	case err == nil:
		next = latest.Version + 1
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return 0, fmt.Errorf("latest version: %w", err)
	}

	def.WorkflowID = workflowID
	def.Version = int(next)
	if err := s.validator.Validate(def); err != nil {
		return 0, err
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return 0, fmt.Errorf("encode version: %w", err)
	}
	if _, err := s.store.PublishWorkflowVersion(ctx, db.PublishWorkflowVersionParams{
		WorkflowID: workflowID, Version: next, Definition: raw,
		RiskLevel: string(def.RiskLevel), PublishedBy: publishedBy,
	}); err != nil {
		return 0, fmt.Errorf("publish version: %w", err)
	}
	for _, n := range def.Nodes {
		cfg, err := json.Marshal(n.Config)
		if err != nil {
			return 0, fmt.Errorf("encode node config: %w", err)
		}
		if cfg == nil || string(cfg) == "null" {
			cfg = []byte(`{}`)
		}
		if err := s.store.InsertWorkflowNode(ctx, db.InsertWorkflowNodeParams{
			WorkflowID: workflowID, Version: next, NodeID: n.ID,
			NodeType: string(n.Type), Name: n.Name, Config: cfg,
			RequiresConfirmation: n.RequiresConfirmation,
		}); err != nil {
			return 0, fmt.Errorf("materialize node %s: %w", n.ID, err)
		}
	}
	for _, e := range def.Edges {
		if err := s.store.InsertWorkflowEdge(ctx, db.InsertWorkflowEdgeParams{
			WorkflowID: workflowID, Version: next,
			FromNode: e.From, ToNode: e.To, Condition: e.Condition,
		}); err != nil {
			return 0, fmt.Errorf("materialize edge %s->%s: %w", e.From, e.To, err)
		}
	}
	return next, nil
}

// VersionDefinition loads an immutable published definition.
func (s *Service) VersionDefinition(ctx context.Context, workflowID string, version int32) (Definition, error) {
	row, err := s.store.GetWorkflowVersion(ctx, db.GetWorkflowVersionParams{WorkflowID: workflowID, Version: version})
	if err != nil {
		return Definition{}, fmt.Errorf("load version %s@%d: %w", workflowID, version, err)
	}
	var def Definition
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		return Definition{}, fmt.Errorf("decode version: %w", err)
	}
	return def, nil
}
