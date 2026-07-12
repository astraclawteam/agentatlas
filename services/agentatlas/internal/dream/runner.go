package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
	BindDreamWorkflowRun(ctx context.Context, arg db.BindDreamWorkflowRunParams) (db.DreamRun, error)
	GetDreamRunByWorkflowRun(ctx context.Context, arg db.GetDreamRunByWorkflowRunParams) (db.DreamRun, error)
	RequeueDreamRunAfterWorkflow(ctx context.Context, arg db.RequeueDreamRunAfterWorkflowParams) (db.DreamRun, error)
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
	policyRow, err := r.store.GetDreamPolicy(ctx, run.PolicyID)
	if err != nil {
		return fmt.Errorf("load Dream policy owner: %w", err)
	}
	if err := validateRunPolicySnapshot(run, policyRow.EnterpriseID, policy); err != nil {
		return err
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
		Visibility: []string{string(policy.VisibilityLevel), policy.OrgUnitID}, SpaceID: space.ID,
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
		Workflow:              policy.Workflow,
		ExistingWorkflowRunID: run.WorkflowRunID.String,
		Input: WorkflowInput{OrgUnitID: run.OrgUnitID, WindowStart: run.WindowStart.Time, WindowEnd: run.WindowEnd.Time,
			Inputs: inputs, Coverage: coverage, Missing: missing, RiskSignalRules: policy.RiskSignalRules},
	})
	if err != nil {
		return err
	}
	if _, err := r.store.BindDreamWorkflowRun(ctx, db.BindDreamWorkflowRunParams{EnterpriseID: run.EnterpriseID, RunID: run.ID, WorkflowRunID: pgtype.Text{String: execution.WorkflowRunID, Valid: true}}); err != nil {
		return fmt.Errorf("bind Dream workflow run: %w", err)
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
	persist := func(store RunnerStore) error {
		if reader, ok := any(store).(interface {
			GetDreamSummaryForRunLayer(context.Context, db.GetDreamSummaryForRunLayerParams) (db.DreamSummary, error)
		}); ok {
			existing, err := reader.GetDreamSummaryForRunLayer(ctx, db.GetDreamSummaryForRunLayerParams{EnterpriseID: run.EnterpriseID, RunID: run.ID, Layer: "display"})
			if err == nil {
				if existing.SummaryText != out.Display {
					return fmt.Errorf("existing Dream output conflicts with workflow result")
				}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		sealedPointer, err := store.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
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
			summary, err := store.CreateDreamSummary(ctx, db.CreateDreamSummaryParams{
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
						if err := store.InsertDreamEvidencePointer(ctx, db.InsertDreamEvidencePointerParams{
							DreamSummaryID: summary.ID, EvidencePointerID: input.EvidencePointerID,
						}); err != nil {
							return err
						}
					}
				}
				// searchable layers get indexed
				if _, err := store.CreateIndexJob(ctx, db.CreateIndexJobParams{
					ID: newID("idx"), EnterpriseID: run.EnterpriseID,
					SourceType: "dream_summary", SourceID: summary.ID, Status: "pending",
				}); err != nil {
					return err
				}
			} else {
				if err := store.InsertDreamEvidencePointer(ctx, db.InsertDreamEvidencePointerParams{
					DreamSummaryID: summary.ID, EvidencePointerID: sealedPointer.ID,
				}); err != nil {
					return err
				}
			}
		}

		// Timeline node on the output space.
		if _, err := store.InsertTimelineNode(ctx, db.InsertTimelineNodeParams{
			ID: newID("tl"), EnterpriseID: run.EnterpriseID, SpaceID: space.ID,
			OrgScope: space.OrgScope, NodeTime: run.WindowEnd,
			SourceType: "dream_summary", SummaryText: truncateRunes(out.Display, 2000),
			Tags: []string{"dream"}, EvidencePointerID: pgtype.Text{String: sealedPointer.ID, Valid: true},
		}); err != nil {
			return fmt.Errorf("timeline node: %w", err)
		}
		return nil
	}
	txStore, ok := any(r.store).(interface {
		InTransaction(context.Context, func(*db.Queries) error) error
	})
	if !ok {
		return fmt.Errorf("Dream output persistence requires PostgreSQL transaction store")
	}
	return txStore.InTransaction(ctx, func(q *db.Queries) error { return persist(q) })
}

func validateRunPolicySnapshot(run db.DreamRun, policyEnterprise string, policy Policy) error {
	if run.EnterpriseID == "" || run.EnterpriseID != policyEnterprise || run.Version != run.PolicyVersion || run.PolicyVersion < 1 || run.OrgUnitID != policy.OrgUnitID || run.Timezone != policy.Timezone || !run.WindowStart.Valid || !run.WindowEnd.Valid || !run.WindowEnd.Time.After(run.WindowStart.Time) || !run.WorkflowID.Valid || run.WorkflowID.String != policy.Workflow.ID || !run.WorkflowVersion.Valid || run.WorkflowVersion.Int32 != policy.Workflow.Version || run.ModelRoute != "workflow/"+policy.Workflow.ID || run.ModelVersion != fmt.Sprintf("v%d", policy.Workflow.Version) {
		return fmt.Errorf("Dream run immutable snapshot disagrees with published policy")
	}
	var visibility struct {
		VisibilityLevel string   `json:"visibility_level"`
		OrgUnitIDs      []string `json:"org_unit_ids"`
	}
	if err := json.Unmarshal(run.VisibilitySnapshot, &visibility); err != nil {
		return fmt.Errorf("decode Dream visibility snapshot: %w", err)
	}
	if visibility.VisibilityLevel != string(policy.VisibilityLevel) {
		return fmt.Errorf("Dream visibility snapshot disagrees with policy")
	}
	found := false
	for _, id := range visibility.OrgUnitIDs {
		if id == policy.OrgUnitID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("Dream visibility snapshot omits policy org unit")
	}
	var input struct {
		SourceCounts []struct {
			SourceType string `json:"source_type"`
		} `json:"source_counts"`
		SanitizedInputIDs []string `json:"sanitized_input_ids"`
	}
	if err := json.Unmarshal(run.InputSnapshot, &input); err != nil {
		return fmt.Errorf("decode Dream input snapshot: %w", err)
	}
	if len(input.SourceCounts) != len(policy.InputSources) {
		return fmt.Errorf("Dream input snapshot disagrees with policy sources")
	}
	for i, source := range policy.InputSources {
		if input.SourceCounts[i].SourceType != string(source) {
			return fmt.Errorf("Dream input snapshot source mismatch")
		}
	}
	return nil
}

// WorkflowCompleted reconciles a resumed workflow back to its bound Dream.
func (r *Runner) WorkflowCompleted(ctx context.Context, result workflow.RunResult) error {
	if result.Status != workflow.RunSucceeded {
		return nil
	}
	run, err := r.store.GetDreamRunByWorkflowRun(ctx, db.GetDreamRunByWorkflowRunParams{EnterpriseID: result.EnterpriseID, WorkflowRunID: pgtype.Text{String: result.RunID, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := r.store.RequeueDreamRunAfterWorkflow(ctx, db.RequeueDreamRunAfterWorkflowParams{EnterpriseID: run.EnterpriseID, WorkflowRunID: pgtype.Text{String: result.RunID, Valid: true}}); err != nil {
		return err
	}
	return r.tasks.Enqueue(ctx, JobTypeDream, run.ID)
}
