package dream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
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
	PublishDreamWorkflowWait(ctx context.Context, arg db.PublishDreamWorkflowWaitParams) (db.DreamRun, error)
	FailDreamRunAfterWorkflow(ctx context.Context, arg db.FailDreamRunAfterWorkflowParams) (db.DreamRun, error)
	ListPendingDreamWorkflowLifecycle(ctx context.Context, resultLimit int32) ([]db.DreamWorkflowLifecycleOutbox, error)
	RecordDreamWorkflowLifecycleFailure(ctx context.Context, arg db.RecordDreamWorkflowLifecycleFailureParams) error
	CompleteDreamWorkflowLifecycle(ctx context.Context, id int64) error
	ReserveDreamOutputHash(ctx context.Context, arg db.ReserveDreamOutputHashParams) (db.DreamRun, error)
	ReserveDreamOutputHashOwned(ctx context.Context, arg db.ReserveDreamOutputHashOwnedParams) (db.DreamRun, error)
	FenceDreamExecutionOwner(ctx context.Context, arg db.FenceDreamExecutionOwnerParams) (string, error)
	CompleteDreamRunOwned(ctx context.Context, arg db.CompleteDreamRunOwnedParams) (int64, error)
	RecoverExpiredUnboundDreamRuns(ctx context.Context, resultLimit int32) ([]string, error)
	ListPendingDreamRuns(ctx context.Context, resultLimit int32) ([]string, error)
	ClaimDreamRunLease(ctx context.Context, arg db.ClaimDreamRunLeaseParams) (int64, error)
	RenewDreamRunLease(ctx context.Context, arg db.RenewDreamRunLeaseParams) (int64, error)
	RecoverExpiredDreamRunAfterWorkflow(ctx context.Context, arg db.RecoverExpiredDreamRunAfterWorkflowParams) (db.DreamRun, error)
	UpdateDreamRunStatus(ctx context.Context, arg db.UpdateDreamRunStatusParams) (int64, error)
	InsertDreamInput(ctx context.Context, arg db.InsertDreamInputParams) error
	ListDreamInputsForRun(ctx context.Context, runID string) ([]db.DreamInput, error)
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
	store          RunnerStore
	objects        Objects
	policy         *PolicyService
	tasks          *tasks.Runner
	resolver       InputResolver
	orchestrator   *Orchestrator
	metrics        *observability.Metrics
	executionOwner string
}

var errDreamExecutionLeaseActive = errors.New("Dream execution lease is active")

// NewRunner wires Dream persistence to the published-workflow orchestrator.
func NewRunner(store RunnerStore, objects Objects, policy *PolicyService, taskRunner *tasks.Runner, orchestrator *Orchestrator) *Runner {
	var resolver InputResolver
	if inputStore, ok := any(store).(InputStore); ok {
		resolver = NewInputResolver(inputStore)
	}
	return &Runner{store: store, objects: objects, policy: policy, tasks: taskRunner, resolver: resolver, orchestrator: orchestrator, executionOwner: newID("dream-worker")}
}

// SetMetrics wires the optional Prometheus surface (DreamRuns by status).
func (r *Runner) SetMetrics(m *observability.Metrics) { r.metrics = m }

func (r *Runner) RegisterJobHandler() error {
	return r.tasks.Register(JobTypeDream, tasks.Handler{
		Claim: func(ctx context.Context, runID string) (bool, error) {
			rows, err := r.store.ClaimDreamRunLease(ctx, db.ClaimDreamRunLeaseParams{ID: runID, ExecutionOwner: pgtype.Text{String: r.executionOwner, Valid: true}})
			return rows > 0, err
		},
		Execute: r.executeWithLease,
		Complete: func(ctx context.Context, runID string, execErr error) error {
			if errors.Is(execErr, ErrDreamWorkflowPaused) {
				return nil
			}
			status, msg := "succeeded", ""
			if execErr != nil {
				status, msg = "failed", truncateRunes(execErr.Error(), 1000)
			}
			if r.metrics != nil {
				r.metrics.DreamRuns.WithLabelValues(status).Inc()
			}
			rows, err := r.store.CompleteDreamRunOwned(ctx, db.CompleteDreamRunOwnedParams{
				ID: runID, Status: status, Error: msg,
				ExecutionOwner: pgtype.Text{String: r.executionOwner, Valid: true},
			})
			if err == nil && rows != 1 {
				return fmt.Errorf("Dream %s execution ownership lost before terminal completion", runID)
			}
			return err
		},
	})
}

func (r *Runner) executeWithLease(ctx context.Context, runID string) error {
	leaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	renewed := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				renewed <- nil
				return
			case <-leaseCtx.Done():
				renewed <- leaseCtx.Err()
				return
			case <-ticker.C:
				rows, err := r.store.RenewDreamRunLease(leaseCtx, db.RenewDreamRunLeaseParams{
					ID: runID, ExecutionOwner: pgtype.Text{String: r.executionOwner, Valid: true},
				})
				if err != nil || rows != 1 {
					if err == nil {
						err = fmt.Errorf("Dream execution lease ownership lost")
					}
					cancel()
					renewed <- err
					return
				}
			}
		}
	}()
	execErr := r.execute(leaseCtx, runID)
	close(done)
	renewErr := <-renewed
	return errors.Join(execErr, renewErr)
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
	var inputs []ResolvedInput
	var coverage Coverage
	var missing []MissingInput
	dreamExec := DreamExecution{
		EnterpriseID: run.EnterpriseID, DreamRunID: run.ID, PolicyID: run.PolicyID, PolicyVersion: run.PolicyVersion,
		Workflow:              policy.Workflow,
		ExistingWorkflowRunID: run.WorkflowRunID.String,
		ExecutionOwner:        r.executionOwner,
	}
	if !run.WorkflowRunID.Valid {
		inputs, coverage, missing, err = r.resolver.Resolve(ctx, ResolveRequest{EnterpriseID: run.EnterpriseID, OrgUnitID: run.OrgUnitID, WindowStart: run.WindowStart.Time, WindowEnd: run.WindowEnd.Time, Timezone: run.Timezone, Sources: policy.InputSources, MaskingRules: policy.MaskingRules, Visibility: []string{string(policy.VisibilityLevel), policy.OrgUnitID}, SpaceID: space.ID})
		if err != nil {
			return fmt.Errorf("resolve Dream inputs: %w", err)
		}
		dreamExec.Input = WorkflowInput{OrgUnitID: run.OrgUnitID, WindowStart: run.WindowStart.Time, WindowEnd: run.WindowEnd.Time, Inputs: inputs, Coverage: coverage, Missing: missing, RiskSignalRules: policy.RiskSignalRules}
	}
	execution, err := r.orchestrator.Run(ctx, dreamExec)
	if err != nil {
		return err
	}
	if run.WorkflowRunID.Valid {
		inputs = execution.Input.Inputs
		coverage = execution.Input.Coverage
		missing = execution.Input.Missing
	}
	persistedInputs, err := r.store.ListDreamInputsForRun(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("load persisted Dream inputs: %w", err)
	}
	if err := validatePersistedDreamInputs(execution.Input, persistedInputs); err != nil {
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
	hashPayload, err := json.Marshal(struct {
		Output            DreamOutput   `json:"output"`
		Input             WorkflowInput `json:"input"`
		PolicyID          string        `json:"policy_id"`
		PolicyVersion     int32         `json:"policy_version"`
		WorkflowRunID     string        `json:"workflow_run_id"`
		EvidenceRetention string        `json:"evidence_retention"`
	}{out, execution.Input, run.PolicyID, run.PolicyVersion, execution.WorkflowRunID, string(policy.EvidenceRetention)})
	if err != nil {
		return fmt.Errorf("hash Dream output: %w", err)
	}
	sum := sha256.Sum256(hashPayload)
	outputHash := "sha256:" + hex.EncodeToString(sum[:])
	if _, err := r.store.ReserveDreamOutputHashOwned(ctx, db.ReserveDreamOutputHashOwnedParams{
		EnterpriseID: run.EnterpriseID, RunID: run.ID, OutputHash: pgtype.Text{String: outputHash, Valid: true},
		ExecutionOwner: pgtype.Text{String: r.executionOwner, Valid: true},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("Dream output conflicts with reserved committed output")
		}
		return err
	}
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
	return txStore.InTransaction(ctx, func(q *db.Queries) error {
		if _, err := q.FenceDreamExecutionOwner(ctx, db.FenceDreamExecutionOwnerParams{
			ID: run.ID, ExecutionOwner: pgtype.Text{String: r.executionOwner, Valid: true},
		}); err != nil {
			return fmt.Errorf("Dream output ownership fence: %w", err)
		}
		return persist(q)
	})
}

func validateRunPolicySnapshot(run db.DreamRun, policyEnterprise string, policy Policy) error {
	if run.EnterpriseID == "" || run.EnterpriseID != policyEnterprise || run.Version != run.PolicyVersion || run.PolicyVersion < 1 || run.OrgUnitID != policy.OrgUnitID || run.Timezone != policy.Timezone || !run.WindowStart.Valid || !run.WindowEnd.Valid || !run.WindowEnd.Time.After(run.WindowStart.Time) || !run.WorkflowID.Valid || run.WorkflowID.String != policy.Workflow.ID || !run.WorkflowVersion.Valid || run.WorkflowVersion.Int32 != policy.Workflow.Version || run.ModelRoute != "workflow/"+policy.Workflow.ID || run.ModelVersion != fmt.Sprintf("v%d", policy.Workflow.Version) {
		return fmt.Errorf("Dream run immutable snapshot disagrees with published policy")
	}
	var visibility sdkdream.VisibilitySnapshotSummary
	decoder := json.NewDecoder(bytes.NewReader(run.VisibilitySnapshot))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&visibility); err != nil {
		return fmt.Errorf("decode Dream visibility snapshot: %w", err)
	}
	if visibility.OrgUnitIDs == nil || len(visibility.OrgUnitIDs) != 1 || visibility.OrgUnitIDs[0] != policy.OrgUnitID || visibility.MaskedFieldCount != 0 || visibility.VisibilityLevel != policy.VisibilityLevel {
		return fmt.Errorf("Dream visibility snapshot disagrees with policy")
	}
	var input sdkdream.InputSnapshotSummary
	decoder = json.NewDecoder(bytes.NewReader(run.InputSnapshot))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return fmt.Errorf("decode Dream input snapshot: %w", err)
	}
	if input.SourceCounts == nil || input.SanitizedInputIDs == nil || len(input.SourceCounts) != len(policy.InputSources) {
		return fmt.Errorf("Dream input snapshot disagrees with policy sources")
	}
	for i, source := range policy.InputSources {
		if input.SourceCounts[i].SourceType != source || input.SourceCounts[i].Count != 0 {
			return fmt.Errorf("Dream input snapshot source mismatch")
		}
	}
	if len(input.SanitizedInputIDs) != 0 {
		return fmt.Errorf("Dream scheduler input snapshot must not claim unresolved IDs")
	}
	return nil
}

func validatePersistedDreamInputs(input WorkflowInput, rows []db.DreamInput) error {
	if len(input.Inputs) > maxResolvedInputs || len(rows) != len(input.Inputs) {
		return fmt.Errorf("persisted Dream inputs disagree with workflow input")
	}
	expected := make(map[string]struct{}, len(input.Inputs))
	for _, item := range input.Inputs {
		key := string(item.SourceType) + "\x00" + item.SourceID
		if item.SourceID == "" || len([]rune(item.SourceID)) > 256 {
			return fmt.Errorf("workflow Dream input identity is invalid")
		}
		expected[key] = struct{}{}
	}
	if len(expected) != len(input.Inputs) {
		return fmt.Errorf("workflow Dream inputs contain duplicate identities")
	}
	for _, row := range rows {
		key := row.SourceType + "\x00" + row.SourceID
		if _, ok := expected[key]; !ok {
			return fmt.Errorf("persisted Dream inputs disagree with workflow input")
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		return fmt.Errorf("persisted Dream inputs disagree with workflow input")
	}
	return nil
}

// WorkflowLifecycle is an eager drain hint. Correctness comes from the
// PostgreSQL outbox written in the same statement as the workflow transition.
func (r *Runner) WorkflowLifecycle(ctx context.Context, _ workflow.RunResult) error {
	return r.ReconcileWorkflowLifecycle(ctx)
}

// ReconcileWorkflowLifecycle idempotently drains durable paired workflow/Dream
// transitions. Failed store or publish work remains pending with retry metadata.
func (r *Runner) ReconcileWorkflowLifecycle(ctx context.Context) error {
	events, err := r.store.ListPendingDreamWorkflowLifecycle(ctx, 100)
	if err != nil {
		return fmt.Errorf("list Dream workflow lifecycle: %w", err)
	}
	var failures []error
	for _, event := range events {
		if err := r.reconcileWorkflowLifecycleEvent(ctx, event); err != nil {
			if errors.Is(err, errDreamExecutionLeaseActive) {
				continue
			}
			recorded := r.store.RecordDreamWorkflowLifecycleFailure(ctx, db.RecordDreamWorkflowLifecycleFailureParams{
				ID: event.ID, LastError: truncateRunes(err.Error(), 1000),
			})
			failures = append(failures, errors.Join(err, recorded))
			continue
		}
		if err := r.store.CompleteDreamWorkflowLifecycle(ctx, event.ID); err != nil {
			completionErr := fmt.Errorf("complete Dream workflow lifecycle %d: %w", event.ID, err)
			recorded := r.store.RecordDreamWorkflowLifecycleFailure(ctx, db.RecordDreamWorkflowLifecycleFailureParams{
				ID: event.ID, LastError: truncateRunes(completionErr.Error(), 1000),
			})
			failures = append(failures, errors.Join(completionErr, recorded))
		}
	}
	return errors.Join(failures...)
}

// RecoverExpiredExecutions republishes Dream claims that expired before a
// workflow was bound. Bound runs are recovered at the workflow layer.
func (r *Runner) RecoverExpiredExecutions(ctx context.Context) error {
	if _, err := r.store.RecoverExpiredUnboundDreamRuns(ctx, 100); err != nil {
		return fmt.Errorf("recover expired unbound Dream runs: %w", err)
	}
	ids, err := r.store.ListPendingDreamRuns(ctx, 100)
	if err != nil {
		return fmt.Errorf("list pending Dream runs: %w", err)
	}
	var failures []error
	for _, id := range ids {
		if err := r.tasks.Enqueue(ctx, JobTypeDream, id); err != nil {
			failures = append(failures, fmt.Errorf("redispatch recovered Dream %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (r *Runner) reconcileWorkflowLifecycleEvent(ctx context.Context, event db.DreamWorkflowLifecycleOutbox) error {
	wfID := pgtype.Text{String: event.WorkflowRunID, Valid: true}
	switch event.Status {
	case workflow.RunWaitingConfirmation:
		_, err := r.store.PublishDreamWorkflowWait(ctx, db.PublishDreamWorkflowWaitParams{
			EnterpriseID: event.EnterpriseID, RunID: event.DreamRunID, WorkflowRunID: wfID,
		})
		return err
	case workflow.RunFailed, workflow.RunCancelled:
		message := event.LifecycleError
		if message == "" {
			message = "Dream workflow " + event.Status
		}
		_, err := r.store.FailDreamRunAfterWorkflow(ctx, db.FailDreamRunAfterWorkflowParams{
			EnterpriseID: event.EnterpriseID, WorkflowRunID: wfID, Error: truncateRunes(message, 1000),
		})
		return err
	case workflow.RunSucceeded:
		run, err := r.store.GetDreamRunByWorkflowRun(ctx, db.GetDreamRunByWorkflowRunParams{
			EnterpriseID: event.EnterpriseID, WorkflowRunID: wfID,
		})
		if err != nil {
			return err
		}
		switch run.Status {
		case "waiting_confirmation":
			if _, err := r.store.RequeueDreamRunAfterWorkflow(ctx, db.RequeueDreamRunAfterWorkflowParams{
				EnterpriseID: event.EnterpriseID, WorkflowRunID: wfID,
			}); err != nil {
				return err
			}
		case "pending":
		case "running":
			if run.ExecutionLeaseExpiresAt.Valid && run.ExecutionLeaseExpiresAt.Time.After(time.Now()) {
				return errDreamExecutionLeaseActive
			}
			if _, err := r.store.RecoverExpiredDreamRunAfterWorkflow(ctx, db.RecoverExpiredDreamRunAfterWorkflowParams{
				EnterpriseID: event.EnterpriseID, WorkflowRunID: wfID,
			}); err != nil {
				return err
			}
		case "succeeded", "failed":
			return nil
		default:
			return fmt.Errorf("Dream %s has invalid lifecycle state %s", run.ID, run.Status)
		}
		if err := r.tasks.Enqueue(ctx, JobTypeDream, run.ID); err != nil {
			return fmt.Errorf("publish Dream redispatch: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported Dream workflow lifecycle status %q", event.Status)
	}
}
