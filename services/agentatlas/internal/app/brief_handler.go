package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

// BriefStore is the ingestion persistence surface (db/generated satisfies it).
type BriefStore interface {
	GetKnowledgeSpaceByScope(ctx context.Context, arg db.GetKnowledgeSpaceByScopeParams) (db.KnowledgeSpace, error)
	GetWorkBriefBySourceHash(ctx context.Context, arg db.GetWorkBriefBySourceHashParams) (db.WorkBrief, error)
	CreateEvidencePointer(ctx context.Context, arg db.CreateEvidencePointerParams) (db.EvidencePointer, error)
	CreateWorkBrief(ctx context.Context, arg db.CreateWorkBriefParams) (db.WorkBrief, error)
	InsertTimelineNode(ctx context.Context, arg db.InsertTimelineNodeParams) (db.TimelineNode, error)
	CreateIndexJob(ctx context.Context, arg db.CreateIndexJobParams) (db.IndexJob, error)
}

type briefDeps struct {
	nexus  nexus.Client
	store  BriefStore
	runner *tasks.Runner
}

type WorkBriefIngest struct {
	EnterpriseID    string   `json:"enterprise_id"`
	EmployeeUserID  string   `json:"employee_user_id"`
	BriefDate       string   `json:"brief_date"` // YYYY-MM-DD
	Summary         string   `json:"summary"`
	Topics          []string `json:"topics,omitempty"`
	ProjectRefs     []string `json:"project_refs,omitempty"`
	SourceHash      string   `json:"source_hash,omitempty"`
	EvidencePointer struct {
		ResourceType          string   `json:"resource_type"`
		ResourceRef           string   `json:"resource_ref"`
		SourceSystem          string   `json:"source_system"`
		ContentHash           string   `json:"content_hash,omitempty"`
		AgentNexusResourceURI string   `json:"agentnexus_resource_uri,omitempty"`
		RequiredScopes        []string `json:"required_scopes,omitempty"`
	} `json:"evidence_pointer"`
}

func (d *briefDeps) handleIngest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, _, err := verifyTicket(ctx, d.nexus, r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
		return
	}
	var req WorkBriefIngest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	summary := strings.TrimSpace(req.Summary)
	switch {
	case req.EnterpriseID == "" || req.EmployeeUserID == "":
		writeError(w, http.StatusUnprocessableEntity, "invalid_brief", "enterprise_id and employee_user_id are required")
		return
	case summary == "" || len([]rune(summary)) > 2000:
		writeError(w, http.StatusUnprocessableEntity, "invalid_brief", "summary must be 1-2000 chars (raw brief content stays in the source system)")
		return
	case req.EvidencePointer.ResourceRef == "" || req.EvidencePointer.SourceSystem == "":
		writeError(w, http.StatusUnprocessableEntity, "invalid_brief", "evidence_pointer.resource_ref and source_system are required")
		return
	}
	briefDate, err := time.Parse("2006-01-02", req.BriefDate)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_brief", "brief_date must be YYYY-MM-DD")
		return
	}

	// dedupe by source hash
	if req.SourceHash != "" {
		if existing, err := d.store.GetWorkBriefBySourceHash(ctx, db.GetWorkBriefBySourceHashParams{
			EnterpriseID: req.EnterpriseID, SourceHash: req.SourceHash,
		}); err == nil {
			writeJSON(w, http.StatusOK, map[string]string{
				"work_brief_id": existing.ID, "timeline_node_id": "",
				"note": "duplicate source_hash; brief already ingested",
			})
			return
		}
	}

	// briefs attach to the employee space created by org sync — fail loud if
	// the org graph hasn't been synced yet.
	space, err := d.store.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
		EnterpriseID: req.EnterpriseID,
		OrgScope:     "employee:" + req.EmployeeUserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnprocessableEntity, "space_missing",
				"employee knowledge space not found — sync the AgentNexus org graph first")
			return
		}
		writeError(w, http.StatusInternalServerError, "space_lookup_failed", err.Error())
		return
	}

	resourceType := req.EvidencePointer.ResourceType
	if resourceType == "" {
		resourceType = "work_brief"
	}
	pointer, err := d.store.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
		ID: newID("ev"), EnterpriseID: req.EnterpriseID,
		ResourceType: resourceType, ResourceRef: req.EvidencePointer.ResourceRef,
		SourceSystem: req.EvidencePointer.SourceSystem,
		ContentHash:  req.EvidencePointer.ContentHash,
		AgentnexusResourceUri: req.EvidencePointer.AgentNexusResourceURI,
		RequiredScopes:        orEmptySlice(req.EvidencePointer.RequiredScopes),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pointer_failed", err.Error())
		return
	}
	brief, err := d.store.CreateWorkBrief(ctx, db.CreateWorkBriefParams{
		ID: newID("wb"), EnterpriseID: req.EnterpriseID, EmployeeUserID: req.EmployeeUserID,
		BriefDate: pgtype.Date{Time: briefDate, Valid: true}, Summary: summary,
		Topics: orEmptySlice(req.Topics), ProjectRefs: orEmptySlice(req.ProjectRefs),
		SourceHash: req.SourceHash, EvidencePointerID: pointer.ID,
	})
	if err != nil {
		writeError(w, http.StatusConflict, "brief_conflict", err.Error())
		return
	}
	node, err := d.store.InsertTimelineNode(ctx, db.InsertTimelineNodeParams{
		ID: newID("tl"), EnterpriseID: req.EnterpriseID, SpaceID: space.ID,
		OrgScope: space.OrgScope,
		NodeTime: pgtype.Timestamptz{Time: briefDate.Add(12 * time.Hour), Valid: true},
		SourceType: "work_brief", SummaryText: summary,
		Tags: orEmptySlice(req.Topics), EvidencePointerID: pgtype.Text{String: pointer.ID, Valid: true},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "timeline_failed", err.Error())
		return
	}
	idxJob, err := d.store.CreateIndexJob(ctx, db.CreateIndexJobParams{
		ID: newID("idx"), EnterpriseID: req.EnterpriseID,
		SourceType: "timeline_node", SourceID: node.ID, Status: "pending",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index_job_failed", err.Error())
		return
	}
	if err := d.runner.Enqueue(ctx, retrieval.JobTypeIndex, idxJob.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "index_dispatch_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"work_brief_id": brief.ID, "timeline_node_id": node.ID,
	})
}

func orEmptySlice(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
