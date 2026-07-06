package dream

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
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
	store   RunnerStore
	objects Objects
	policy  *PolicyService
	tasks   *tasks.Runner
	synth   *Synthesizer
}

// NewRunner wires a dream runner. synth may be nil (or model-less) — that is
// the explicit deterministic degraded mode.
func NewRunner(store RunnerStore, objects Objects, policy *PolicyService, taskRunner *tasks.Runner, synth *Synthesizer) *Runner {
	return &Runner{store: store, objects: objects, policy: policy, tasks: taskRunner, synth: synth}
}

func (r *Runner) RegisterJobHandler() error {
	return r.tasks.Register(JobTypeDream, tasks.Handler{
		Claim: func(ctx context.Context, runID string) (bool, error) {
			rows, err := r.store.ClaimDreamRun(ctx, runID)
			return rows > 0, err
		},
		Execute: r.execute,
		Complete: func(ctx context.Context, runID string, execErr error) error {
			status, msg := "succeeded", ""
			if execErr != nil {
				status, msg = "failed", truncateRunes(execErr.Error(), 1000)
			}
			_, err := r.store.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{
				ID: runID, Status: status, Error: msg,
			})
			return err
		},
	})
}

func (r *Runner) execute(ctx context.Context, runID string) error {
	run, err := r.store.GetDreamRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	policy, _, err := r.policy.LoadPublished(ctx, run.PolicyID)
	if err != nil {
		return err
	}
	space, err := r.store.GetKnowledgeSpace(ctx, policy.OutputSpaceID)
	if err != nil {
		return fmt.Errorf("output space %s: %w", policy.OutputSpaceID, err)
	}
	masker, err := NewMasker(policy.MaskingRules)
	if err != nil {
		return err
	}
	risks, err := NewRiskExtractor(policy.RiskSignalRules)
	if err != nil {
		return err
	}

	// Gather inputs: work briefs of the output space's members in the window.
	members, err := r.store.ListSpaceMembers(ctx, space.ID)
	if err != nil {
		return fmt.Errorf("space members: %w", err)
	}
	userIDs := make([]string, 0, len(members))
	for _, m := range members {
		userIDs = append(userIDs, m.UserID)
	}
	var briefs []db.WorkBrief
	if len(userIDs) > 0 {
		briefs, err = r.store.ListWorkBriefsForWindow(ctx, db.ListWorkBriefsForWindowParams{
			EnterpriseID: run.EnterpriseID, Column2: userIDs,
			BriefDate: pgtype.Date{Time: run.WindowStart.Time, Valid: true},
			BriefDate_2: pgtype.Date{Time: run.WindowEnd.Time, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("window briefs: %w", err)
		}
	}
	for _, b := range briefs {
		if err := r.store.InsertDreamInput(ctx, db.InsertDreamInputParams{
			RunID: runID, SourceType: "work_brief", SourceID: b.ID,
		}); err != nil {
			return err
		}
	}

	agg, err := r.synth.Aggregate(ctx, space.Name, run.WindowStart.Time, run.WindowEnd.Time, briefs, masker, risks)
	if err != nil {
		return fmt.Errorf("aggregate: %w", err)
	}
	riskJSON, err := json.Marshal(agg.RiskSignals)
	if err != nil {
		return err
	}
	if riskJSON == nil || string(riskJSON) == "null" {
		riskJSON = []byte("[]")
	}

	// Sealed detailed summary: object storage only, pointer in DB.
	sealedKey := fmt.Sprintf("dreams/%s/%s.md", run.EnterpriseID, runID)
	if err := r.objects.Put(ctx, sealedKey, "text/markdown", []byte(agg.SealedDetail)); err != nil {
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
		{layer: "display", text: agg.Display},
		{layer: "retrieval", text: agg.Retrieval},
		{layer: "sealed_pointer", text: "", key: sealedKey, ptr: pgtype.Text{String: sealedPointer.ID, Valid: true}},
	}
	for _, l := range layers {
		summary, err := r.store.CreateDreamSummary(ctx, db.CreateDreamSummaryParams{
			ID: newID("dsum"), RunID: runID, EnterpriseID: run.EnterpriseID,
			SpaceID: space.ID, Layer: l.layer, SummaryText: truncateRunes(l.text, 4000),
			SealedObjectKey: l.key, EvidencePointerID: l.ptr, RiskSignals: riskJSON,
		})
		if err != nil {
			return fmt.Errorf("summary layer %s: %w", l.layer, err)
		}
		// pointer_only retention: only the sealed layer carries evidence links
		if l.layer != "sealed_pointer" {
			if policy.EvidenceRetention == "pointer_plus_display_summary" {
				for _, b := range briefs {
					if err := r.store.InsertDreamEvidencePointer(ctx, db.InsertDreamEvidencePointerParams{
						DreamSummaryID: summary.ID, EvidencePointerID: b.EvidencePointerID,
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
		SourceType: "dream_summary", SummaryText: truncateRunes(agg.Display, 2000),
		Tags: []string{"dream"}, EvidencePointerID: pgtype.Text{String: sealedPointer.ID, Valid: true},
	}); err != nil {
		return fmt.Errorf("timeline node: %w", err)
	}
	return nil
}
