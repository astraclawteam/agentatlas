package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/observability"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

// RunnerStore is the persistence surface for executing dream runs.
type RunnerStore interface {
	SchedulerStore
	GetDreamRun(ctx context.Context, id string) (db.DreamRun, error)
	ClaimDreamRun(ctx context.Context, id string) (int64, error)
	UpdateDreamRunStatus(ctx context.Context, arg db.UpdateDreamRunStatusParams) (int64, error)
	InsertDreamInput(ctx context.Context, arg db.InsertDreamInputParams) error
	CreateDreamSummary(ctx context.Context, arg db.CreateDreamSummaryParams) (db.DreamSummary, error)
	InsertDreamEvidencePointer(ctx context.Context, arg db.InsertDreamEvidencePointerParams) error
	CreateEvidencePointer(ctx context.Context, arg db.CreateEvidencePointerParams) (db.EvidencePointer, error)
	GetKnowledgeSpace(ctx context.Context, id string) (db.KnowledgeSpace, error)
	ListSpaceMembers(ctx context.Context, spaceID string) ([]db.SpaceMembershipCache, error)
	ListWorkBriefsForWindow(ctx context.Context, arg db.ListWorkBriefsForWindowParams) ([]db.WorkBrief, error)
	InsertTimelineNode(ctx context.Context, arg db.InsertTimelineNodeParams) (db.TimelineNode, error)
	CreateIndexJob(ctx context.Context, arg db.CreateIndexJobParams) (db.IndexJob, error)
}

// Objects seals detailed summaries into object storage.
type Objects interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
}

// Runner executes claimed dream runs.
type Runner struct {
	store        RunnerStore
	objects      Objects
	policy       *PolicyService
	tasks        *tasks.Runner
	resolver     InputResolver
	orchestrator *Orchestrator
	metrics      *observability.Metrics
}

// NewRunner wires Dream persistence to the published-workflow orchestrator.
func NewRunner(store RunnerStore, objects Objects, policy *PolicyService, taskRunner *tasks.Runner, orchestrator *Orchestrator) *Runner {
	var resolver InputResolver
	if inputStore, ok := any(store).(InputStore); ok {
		resolver = NewInputResolver(inputStore)
	}
	return &Runner{store: store, objects: objects, policy: policy, tasks: taskRunner, resolver: resolver, orchestrator: orchestrator}
}

// SetMetrics wires the optional Prometheus surface (DreamRuns by status).
func (r *Runner) SetMetrics(m *observability.Metrics) { r.metrics = m }

func (r *Runner) RegisterJobHandler() error {
	return r.tasks.Register(JobTypeDream, tasks.Handler{
		Claim: func(ctx context.Context, runID string) (bool, error) {
			rows, err := r.store.ClaimDreamRun(ctx, runID)
			return rows > 0, err
		},
		Execute: r.execute,
		Complete: func(ctx context.Context, runID string, execErr error) error {
			status, msg := "succeeded", ""
			if errors.Is(execErr, ErrDreamWorkflowPaused) {
				status = "waiting_confirmation"
			} else if execErr != nil {
				status, msg = "failed", truncateRunes(execErr.Error(), 1000)
			}
			if r.metrics != nil {
				r.metrics.DreamRuns.WithLabelValues(status).Inc()
			}
			_, err := r.store.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{
				ID: runID, Status: status, Error: msg,
			})
			return err
		},
	})
}

func (r *Runner) execute(ctx context.Context, runID string) error {
	ctx, span := observability.Tracer("dream").Start(ctx, "dream.run")
	span.SetAttributes(attribute.String("run_id", runID))
	defer span.End()

	run, err := r.store.GetDreamRun(ctx, runID)
	if err != nil {
		span.RecordError(err)
		return fmt.Errorf("load run: %w", err)
	}
	if !run.WorkflowID.Valid || run.WorkflowID.String == "" || !run.WorkflowVersion.Valid || run.WorkflowVersion.Int32 < 1 {
		return fmt.Errorf("Dream run %s has no pinned published workflow", runID)
	}
	policy, err := r.policy.LoadVersion(ctx, run.PolicyID, run.PolicyVersion)
	if err != nil {
		return err
	}
	if policy.Workflow.ID != run.WorkflowID.String || policy.Workflow.Version != run.WorkflowVersion.Int32 {
		return fmt.Errorf("Dream run %s workflow snapshot conflicts with policy %s@%d", runID, run.PolicyID, run.PolicyVersion)
	}
	space, err := r.store.GetKnowledgeSpace(ctx, policy.OutputSpaceID)
	if err != nil {
		return fmt.Errorf("output space %s: %w", policy.OutputSpaceID, err)
	}
	if r.resolver == nil || r.orchestrator == nil {
		return fmt.Errorf("Dream runner requires input resolver and workflow orchestrator")
	}
	inputs, coverage, missing, err := r.resolver.Resolve(ctx, ResolveRequest{
		EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID,
		WindowStart: run.WindowStart.Time, WindowEnd: run.WindowEnd.Time, Timezone: run.Timezone,
		Sources: policy.InputSources, MaskingRules: policy.MaskingRules,
		Visibility: []string{string(policy.VisibilityLevel)}, SpaceID: space.ID,
	})
	if err != nil {
		return fmt.Errorf("resolve Dream inputs: %w", err)
	}
	for _, input := range inputs {
		if err := r.store.InsertDreamInput(ctx, db.InsertDreamInputParams{
			RunID: runID, SourceType: string(input.SourceType), SourceID: input.SourceID,
		}); err != nil {
			return err
		}
	}
	execution, err := r.orchestrator.Run(ctx, DreamExecution{
		EnterpriseID: run.EnterpriseID, DreamRunID: run.ID, PolicyID: run.PolicyID, PolicyVersion: run.PolicyVersion,
		Workflow: policy.Workflow,
		Input: WorkflowInput{OrgUnitID: run.OrgUnitID, WindowStart: run.WindowStart.Time, WindowEnd: run.WindowEnd.Time,
			Inputs: inputs, Coverage: coverage, Missing: missing, RiskSignalRules: policy.RiskSignalRules},
	})
	if err != nil {
		return err
	}
	if execution.Status == workflow.RunWaitingConfirmation {
		return ErrDreamWorkflowPaused
	}
	out := execution.Output
	riskJSON, err := json.Marshal(out.Risks)
	if err != nil {
		return err
	}
	if riskJSON == nil || string(riskJSON) == "null" {
		riskJSON = []byte("[]")
	}
	factsJSON, err := json.Marshal(out.Facts)
	if err != nil {
		return err
	}
	themesJSON, err := json.Marshal(out.Themes)
	if err != nil {
		return err
	}
	trendsJSON, err := json.Marshal(out.Trends)
	if err != nil {
		return err
	}
	todosJSON, err := json.Marshal(out.Todos)
	if err != nil {
		return err
	}
	for _, payload := range []*[]byte{&factsJSON, &themesJSON, &trendsJSON, &todosJSON} {
		if *payload == nil || string(*payload) == "null" {
			*payload = []byte("[]")
		}
	}

	// Sealed detailed summary: object storage only, pointer in DB.
	sealedKey := fmt.Sprintf("dreams/%s/%s.md", run.EnterpriseID, runID)
	if err := r.objects.Put(ctx, sealedKey, "text/markdown", []byte(out.SealedDetail)); err != nil {
		return fmt.Errorf("seal detail: %w", err)
	}
	sealedPointer, err := r.store.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
		ID: newID("ev"), EnterpriseID: run.EnterpriseID, ResourceType: "dream_sealed_summary",
		ResourceRef: sealedKey, SourceSystem: "object-storage", RequiredScopes: []string{},
	})
	if err != nil {
		return fmt.Errorf("sealed pointer: %w", err)
	}

	layers := []struct {
		layer string
		text  string
		key   string
		ptr   pgtype.Text
	}{
		{layer: "display", text: out.Display},
		{layer: "retrieval", text: out.Retrieval},
		{layer: "sealed_pointer", text: "", key: sealedKey, ptr: pgtype.Text{String: sealedPointer.ID, Valid: true}},
	}
	for _, l := range layers {
		summary, err := r.store.CreateDreamSummary(ctx, db.CreateDreamSummaryParams{
			ID: newID("dsum"), RunID: runID, EnterpriseID: run.EnterpriseID,
			SpaceID: space.ID, Layer: l.layer, SummaryText: truncateRunes(l.text, 4000),
			SealedObjectKey: l.key, EvidencePointerID: l.ptr, RiskSignals: riskJSON,
			Facts: factsJSON, Themes: themesJSON, Trends: trendsJSON, Todos: todosJSON,
		})
		if err != nil {
			return fmt.Errorf("summary layer %s: %w", l.layer, err)
		}
		// pointer_only retention: only the sealed layer carries evidence links
		if l.layer != "sealed_pointer" {
			if policy.EvidenceRetention == "pointer_plus_display_summary" {
				for _, input := range inputs {
					if input.EvidencePointerID == "" {
						continue
					}
					if err := r.store.InsertDreamEvidencePointer(ctx, db.InsertDreamEvidencePointerParams{
						DreamSummaryID: summary.ID, EvidencePointerID: input.EvidencePointerID,
					}); err != nil {
						return err
					}
				}
			}
			// searchable layers get indexed
			if _, err := r.store.CreateIndexJob(ctx, db.CreateIndexJobParams{
				ID: newID("idx"), EnterpriseID: run.EnterpriseID,
				SourceType: "dream_summary", SourceID: summary.ID, Status: "pending",
			}); err != nil {
				return err
			}
		} else {
			if err := r.store.InsertDreamEvidencePointer(ctx, db.InsertDreamEvidencePointerParams{
				DreamSummaryID: summary.ID, EvidencePointerID: sealedPointer.ID,
			}); err != nil {
				return err
			}
		}
	}

	// Timeline node on the output space.
	if _, err := r.store.InsertTimelineNode(ctx, db.InsertTimelineNodeParams{
		ID: newID("tl"), EnterpriseID: run.EnterpriseID, SpaceID: space.ID,
		OrgScope: space.OrgScope, NodeTime: run.WindowEnd,
		SourceType: "dream_summary", SummaryText: truncateRunes(out.Display, 2000),
		Tags: []string{"dream"}, EvidencePointerID: pgtype.Text{String: sealedPointer.ID, Valid: true},
	}); err != nil {
		return fmt.Errorf("timeline node: %w", err)
	}
	return nil
}
