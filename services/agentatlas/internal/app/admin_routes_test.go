package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/agent"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/artifacts"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/nexusclient"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/retrieval"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

// --- fakes -------------------------------------------------------------------

// fakeWorkflowStore backs workflow.Service create/publish in memory.
type fakeWorkflowStore struct {
	workflows map[string]db.Workflow
	versions  map[string]db.WorkflowVersion
	nodes     int
	edges     int
}

func newFakeWorkflowStore() *fakeWorkflowStore {
	return &fakeWorkflowStore{workflows: map[string]db.Workflow{}, versions: map[string]db.WorkflowVersion{}}
}

func (f *fakeWorkflowStore) CreateWorkflowDraft(_ context.Context, arg db.CreateWorkflowDraftParams) (db.Workflow, error) {
	wf := db.Workflow{ID: arg.ID, EnterpriseID: arg.EnterpriseID, Name: arg.Name, Kind: arg.Kind, Draft: arg.Draft}
	f.workflows[arg.ID] = wf
	return wf, nil
}

func (f *fakeWorkflowStore) UpdateWorkflowDraft(_ context.Context, arg db.UpdateWorkflowDraftParams) (int64, error) {
	wf, ok := f.workflows[arg.ID]
	if !ok || wf.EnterpriseID != arg.EnterpriseID {
		return 0, nil
	}
	wf.Draft = arg.Draft
	f.workflows[arg.ID] = wf
	return 1, nil
}

func (f *fakeWorkflowStore) GetWorkflow(_ context.Context, id string) (db.Workflow, error) {
	wf, ok := f.workflows[id]
	if !ok {
		return db.Workflow{}, pgx.ErrNoRows
	}
	return wf, nil
}

func (f *fakeWorkflowStore) PublishWorkflowVersion(_ context.Context, arg db.PublishWorkflowVersionParams) (db.WorkflowVersion, error) {
	v := db.WorkflowVersion{WorkflowID: arg.WorkflowID, Version: arg.Version, Definition: arg.Definition}
	f.versions[fmt.Sprintf("%s@%d", arg.WorkflowID, arg.Version)] = v
	return v, nil
}

func (f *fakeWorkflowStore) GetWorkflowVersion(_ context.Context, arg db.GetWorkflowVersionParams) (db.WorkflowVersion, error) {
	v, ok := f.versions[fmt.Sprintf("%s@%d", arg.WorkflowID, arg.Version)]
	if !ok {
		return db.WorkflowVersion{}, pgx.ErrNoRows
	}
	return v, nil
}

func (f *fakeWorkflowStore) GetLatestWorkflowVersion(_ context.Context, workflowID string) (db.WorkflowVersion, error) {
	var latest db.WorkflowVersion
	found := false
	for _, v := range f.versions {
		if v.WorkflowID == workflowID && (!found || v.Version > latest.Version) {
			latest, found = v, true
		}
	}
	if !found {
		return db.WorkflowVersion{}, pgx.ErrNoRows
	}
	return latest, nil
}

func (f *fakeWorkflowStore) InsertWorkflowNode(context.Context, db.InsertWorkflowNodeParams) error {
	f.nodes++
	return nil
}

func (f *fakeWorkflowStore) InsertWorkflowEdge(context.Context, db.InsertWorkflowEdgeParams) error {
	f.edges++
	return nil
}

func (f *fakeWorkflowStore) CreateWorkflowRun(context.Context, db.CreateWorkflowRunParams) (db.WorkflowRun, error) {
	return db.WorkflowRun{}, fmt.Errorf("not used")
}

func (f *fakeWorkflowStore) UpdateWorkflowRunStatus(context.Context, db.UpdateWorkflowRunStatusParams) (int64, error) {
	return 0, fmt.Errorf("not used")
}

func (f *fakeWorkflowStore) GetWorkflowRun(context.Context, string) (db.WorkflowRun, error) {
	return db.WorkflowRun{}, pgx.ErrNoRows
}

func (f *fakeWorkflowStore) InsertWorkflowRunEvent(context.Context, db.InsertWorkflowRunEventParams) (db.WorkflowRunEvent, error) {
	return db.WorkflowRunEvent{}, fmt.Errorf("not used")
}

func (f *fakeWorkflowStore) ListWorkflowRunEvents(context.Context, string) ([]db.WorkflowRunEvent, error) {
	return nil, nil
}

// fakePolicyStore backs dream.PolicyService in memory.
type fakePolicyStore struct {
	policies   map[string]db.DreamPolicy
	versions   map[string]db.DreamPolicyVersion
	operations map[string]db.DreamPolicyOperation
}

func newFakePolicyStore() *fakePolicyStore {
	return &fakePolicyStore{policies: map[string]db.DreamPolicy{}, versions: map[string]db.DreamPolicyVersion{}, operations: map[string]db.DreamPolicyOperation{}}
}

func (f *fakePolicyStore) CreateDreamPolicy(_ context.Context, arg db.CreateDreamPolicyParams) (db.DreamPolicy, error) {
	p := db.DreamPolicy{ID: arg.ID, EnterpriseID: arg.EnterpriseID, OrgScope: arg.OrgScope, Status: arg.Status, Draft: arg.Draft}
	f.policies[arg.ID] = p
	return p, nil
}

func (f *fakePolicyStore) CreateDreamPolicyLifecycle(_ context.Context, arg db.CreateDreamPolicyLifecycleParams) (db.CreateDreamPolicyLifecycleRow, error) {
	op, ok := f.operations[arg.EnterpriseID+"\x00"+arg.OperationKey]
	if !ok || !op.AuditRefID.Valid {
		return db.CreateDreamPolicyLifecycleRow{}, pgx.ErrNoRows
	}
	p := db.DreamPolicy{ID: arg.ID, EnterpriseID: arg.EnterpriseID, OrgScope: arg.OrgScope, Status: "draft", Draft: arg.Draft, RequesterUserID: arg.RequesterUserID, PermissionMode: arg.PermissionMode, RiskReasons: []byte(`[]`), ReviewOrgPath: []byte(`[]`), AuditRefID: op.AuditRefID.String}
	f.policies[p.ID] = p
	return db.CreateDreamPolicyLifecycleRow{ID: p.ID, EnterpriseID: p.EnterpriseID, OrgScope: p.OrgScope, Status: p.Status, Draft: p.Draft, Revision: p.Revision, RequesterUserID: p.RequesterUserID, PermissionMode: p.PermissionMode, RiskReasons: p.RiskReasons, ReviewOrgPath: p.ReviewOrgPath, AuditRefID: p.AuditRefID}, nil
}
func (f *fakePolicyStore) GetEnterpriseDreamPolicy(_ context.Context, arg db.GetEnterpriseDreamPolicyParams) (db.DreamPolicy, error) {
	p, ok := f.policies[arg.ID]
	if !ok || p.EnterpriseID != arg.EnterpriseID {
		return db.DreamPolicy{}, pgx.ErrNoRows
	}
	return p, nil
}
func (f *fakePolicyStore) GetDreamOrgTreeVersion(context.Context, string) (int64, error) {
	return 1, nil
}
func (f *fakePolicyStore) ReserveDreamPolicyOperation(_ context.Context, a db.ReserveDreamPolicyOperationParams) (db.DreamPolicyOperation, error) {
	key := a.EnterpriseID + "\x00" + a.OperationKey
	if old, ok := f.operations[key]; ok {
		if old.OperationKind != a.OperationKind || old.PolicyID != a.PolicyID || old.ActorUserID != a.ActorUserID || old.RequestHash != a.RequestHash {
			return db.DreamPolicyOperation{}, pgx.ErrNoRows
		}
		return old, nil
	}
	now := time.Now().UTC()
	op := db.DreamPolicyOperation{EnterpriseID: a.EnterpriseID, OperationKey: a.OperationKey, OperationKind: a.OperationKind, PolicyID: a.PolicyID, ActorUserID: a.ActorUserID, RequestHash: a.RequestHash, FactsNonce: a.FactsNonce, FactsIssuedAt: pgtype.Timestamptz{Time: now, Valid: true}, FactsExpiresAt: pgtype.Timestamptz{Time: now.Add(5 * time.Minute), Valid: true}, Status: "pending"}
	f.operations[key] = op
	return op, nil
}
func (f *fakePolicyStore) GetDreamPolicyOperation(_ context.Context, a db.GetDreamPolicyOperationParams) (db.DreamPolicyOperation, error) {
	op, ok := f.operations[a.EnterpriseID+"\x00"+a.OperationKey]
	if !ok {
		return db.DreamPolicyOperation{}, pgx.ErrNoRows
	}
	return op, nil
}
func (f *fakePolicyStore) RecordDreamPolicyOperationAudit(_ context.Context, a db.RecordDreamPolicyOperationAuditParams) (db.DreamPolicyOperation, error) {
	key := a.EnterpriseID + "\x00" + a.OperationKey
	op, ok := f.operations[key]
	if !ok || op.Status != "pending" {
		return db.DreamPolicyOperation{}, pgx.ErrNoRows
	}
	op.AuditRefID = a.AuditRefID
	f.operations[key] = op
	return op, nil
}
func (f *fakePolicyStore) CompleteDreamPolicyOperation(_ context.Context, a db.CompleteDreamPolicyOperationParams) (db.DreamPolicyOperation, error) {
	key := a.EnterpriseID + "\x00" + a.OperationKey
	op, ok := f.operations[key]
	if !ok || !op.AuditRefID.Valid {
		return db.DreamPolicyOperation{}, pgx.ErrNoRows
	}
	op.Status = "completed"
	op.Result = a.Result
	f.operations[key] = op
	return op, nil
}

func updateRow(p db.DreamPolicy) db.UpdateDreamPolicyDraftIfRevisionRow {
	return db.UpdateDreamPolicyDraftIfRevisionRow{ID: p.ID, EnterpriseID: p.EnterpriseID, OrgScope: p.OrgScope, Status: p.Status, Draft: p.Draft, Revision: p.Revision, RequesterUserID: p.RequesterUserID, PermissionMode: p.PermissionMode, PendingAction: p.PendingAction, ReviewState: p.ReviewState, RiskLevel: p.RiskLevel, RiskReasons: p.RiskReasons, ReviewMode: p.ReviewMode, ReviewerUserID: p.ReviewerUserID, ReviewOrgPath: p.ReviewOrgPath, ReviewQueue: p.ReviewQueue, Decision: p.Decision, AuditRefID: p.AuditRefID}
}
func submitRow(p db.DreamPolicy) db.SubmitDreamPolicyReviewIfRevisionRow {
	return db.SubmitDreamPolicyReviewIfRevisionRow{ID: p.ID, EnterpriseID: p.EnterpriseID, OrgScope: p.OrgScope, Status: p.Status, Draft: p.Draft, Revision: p.Revision, RequesterUserID: p.RequesterUserID, PermissionMode: p.PermissionMode, PendingAction: p.PendingAction, ReviewState: p.ReviewState, RiskLevel: p.RiskLevel, RiskReasons: p.RiskReasons, ReviewMode: p.ReviewMode, ReviewerUserID: p.ReviewerUserID, ReviewOrgPath: p.ReviewOrgPath, ReviewQueue: p.ReviewQueue, Decision: p.Decision, AuditRefID: p.AuditRefID}
}
func decisionRow(p db.DreamPolicy) db.DecideDreamPolicyIfRevisionRow {
	return db.DecideDreamPolicyIfRevisionRow{ID: p.ID, EnterpriseID: p.EnterpriseID, OrgScope: p.OrgScope, Status: p.Status, Draft: p.Draft, Revision: p.Revision, RequesterUserID: p.RequesterUserID, PermissionMode: p.PermissionMode, PendingAction: p.PendingAction, ReviewState: p.ReviewState, RiskLevel: p.RiskLevel, RiskReasons: p.RiskReasons, ReviewMode: p.ReviewMode, ReviewerUserID: p.ReviewerUserID, ReviewOrgPath: p.ReviewOrgPath, ReviewQueue: p.ReviewQueue, Decision: p.Decision, AuditRefID: p.AuditRefID}
}
func disableRow(p db.DreamPolicy) db.DisableDreamPolicyIfRevisionRow {
	return db.DisableDreamPolicyIfRevisionRow{ID: p.ID, EnterpriseID: p.EnterpriseID, OrgScope: p.OrgScope, Status: p.Status, Draft: p.Draft, Revision: p.Revision, RequesterUserID: p.RequesterUserID, PermissionMode: p.PermissionMode, PendingAction: p.PendingAction, ReviewState: p.ReviewState, RiskLevel: p.RiskLevel, RiskReasons: p.RiskReasons, ReviewMode: p.ReviewMode, ReviewerUserID: p.ReviewerUserID, ReviewOrgPath: p.ReviewOrgPath, ReviewQueue: p.ReviewQueue, Decision: p.Decision, AuditRefID: p.AuditRefID}
}
func (f *fakePolicyStore) UpdateDreamPolicyDraftIfRevision(_ context.Context, a db.UpdateDreamPolicyDraftIfRevisionParams) (db.UpdateDreamPolicyDraftIfRevisionRow, error) {
	p, ok := f.policies[a.TargetID]
	if !ok || p.EnterpriseID != a.TargetEnterpriseID || p.Revision != a.ExpectedRevision || p.PermissionMode != "direct_edit" || (p.Status != "draft" && p.Status != "published" && p.Status != "disabled") {
		return db.UpdateDreamPolicyDraftIfRevisionRow{}, pgx.ErrNoRows
	}
	p.OrgScope = a.OrgScope
	p.Draft = a.Draft
	p.Revision++
	p.Status = "draft"
	if p.RequesterUserID == "" {
		p.RequesterUserID = a.ActorUserID
	}
	p.PendingAction = ""
	p.ReviewState = ""
	p.RiskLevel = ""
	p.RiskReasons = []byte(`[]`)
	p.ReviewMode = ""
	p.ReviewerUserID = pgtype.Text{}
	p.ReviewOrgPath = []byte(`[]`)
	p.ReviewQueue = pgtype.Text{}
	p.Decision = ""
	p.AuditRefID = a.AuditRefID
	f.policies[p.ID] = p
	return updateRow(p), nil
}
func (f *fakePolicyStore) SubmitDreamPolicyReviewIfRevision(_ context.Context, a db.SubmitDreamPolicyReviewIfRevisionParams) (db.SubmitDreamPolicyReviewIfRevisionRow, error) {
	p, ok := f.policies[a.TargetID]
	validState := (a.PendingAction == "publish" && p.Status == "draft") || (a.PendingAction == "disable" && p.Status == "published")
	if !ok || p.EnterpriseID != a.TargetEnterpriseID || p.Revision != a.ExpectedRevision || !validState || p.PermissionMode != "direct_edit" || (p.ReviewState != "" && p.ReviewState != "rejected") {
		return db.SubmitDreamPolicyReviewIfRevisionRow{}, pgx.ErrNoRows
	}
	p.PendingAction = a.PendingAction
	p.ReviewState = "pending"
	p.RiskLevel = a.RiskLevel
	p.RiskReasons = a.RiskReasons
	p.ReviewMode = a.ReviewMode
	p.ReviewerUserID = a.ReviewerUserID
	p.ReviewOrgPath = a.ReviewOrgPath
	p.ReviewQueue = a.ReviewQueue
	p.AuditRefID = a.AuditRefID
	f.policies[p.ID] = p
	return submitRow(p), nil
}
func (f *fakePolicyStore) RefreshDreamPolicyReviewRoute(_ context.Context, a db.RefreshDreamPolicyReviewRouteParams) (db.DreamPolicy, error) {
	p, ok := f.policies[a.TargetID]
	if !ok || p.ReviewState != "pending" || p.ReviewMode != "enterprise_knowledge_admin_queue" || p.Revision != a.ExpectedRevision {
		return db.DreamPolicy{}, pgx.ErrNoRows
	}
	p.RiskLevel = a.RiskLevel
	p.RiskReasons = a.RiskReasons
	p.ReviewMode = a.ReviewMode
	p.ReviewerUserID = a.ReviewerUserID
	p.ReviewOrgPath = a.ReviewOrgPath
	p.ReviewQueue = a.ReviewQueue
	f.policies[p.ID] = p
	return p, nil
}
func (f *fakePolicyStore) DecideDreamPolicyIfRevision(_ context.Context, a db.DecideDreamPolicyIfRevisionParams) (db.DecideDreamPolicyIfRevisionRow, error) {
	p, ok := f.policies[a.TargetID]
	actor := a.ActorUserID.String
	validActor := (p.ReviewMode == "upward_review" && p.ReviewerUserID.Valid && p.ReviewerUserID.String == actor && p.RequesterUserID != actor) || (p.RiskLevel == "low" && p.ReviewMode == "single_confirmation" && p.RequesterUserID == actor)
	if !ok || p.EnterpriseID != a.TargetEnterpriseID || p.Revision != a.ExpectedRevision || p.ReviewState != "pending" || !validActor {
		return db.DecideDreamPolicyIfRevisionRow{}, pgx.ErrNoRows
	}
	p.Decision = a.Decision
	if a.Decision == "approve" {
		p.ReviewState = "approved"
	} else {
		p.ReviewState = "rejected"
	}
	p.AuditRefID = a.AuditRefID
	f.policies[p.ID] = p
	return decisionRow(p), nil
}
func (f *fakePolicyStore) PublishDreamPolicyGoverned(_ context.Context, a db.PublishDreamPolicyGovernedParams) (db.PublishDreamPolicyGovernedRow, error) {
	p, ok := f.policies[a.TargetID]
	if !ok || p.EnterpriseID != a.TargetEnterpriseID || p.Revision != a.ExpectedRevision || p.Status != "draft" || p.ReviewState != "approved" || p.PendingAction != "publish" || p.PermissionMode != "direct_edit" {
		return db.PublishDreamPolicyGovernedRow{}, pgx.ErrNoRows
	}
	v := int32(1)
	for _, x := range f.versions {
		if x.PolicyID == p.ID && x.Version >= v {
			v = x.Version + 1
		}
	}
	p.Status = "published"
	p.PendingAction = ""
	p.ReviewState = ""
	p.Revision++
	p.AuditRefID = a.AuditRefID
	f.policies[p.ID] = p
	row := db.DreamPolicyVersion{PolicyID: p.ID, Version: v, Definition: p.Draft}
	f.versions[fmt.Sprintf("%s@%d", p.ID, v)] = row
	return db.PublishDreamPolicyGovernedRow{PolicyID: p.ID, Version: v, Definition: p.Draft}, nil
}
func (f *fakePolicyStore) DisableDreamPolicyIfRevision(_ context.Context, a db.DisableDreamPolicyIfRevisionParams) (db.DisableDreamPolicyIfRevisionRow, error) {
	p, ok := f.policies[a.TargetID]
	if !ok || p.EnterpriseID != a.TargetEnterpriseID || p.Revision != a.ExpectedRevision || p.Status != "published" || p.ReviewState != "approved" || p.PendingAction != "disable" {
		return db.DisableDreamPolicyIfRevisionRow{}, pgx.ErrNoRows
	}
	p.Status = "disabled"
	p.PendingAction = ""
	p.ReviewState = ""
	p.Revision++
	p.AuditRefID = a.AuditRefID
	f.policies[p.ID] = p
	return disableRow(p), nil
}

func (f *fakePolicyStore) GetDreamPolicy(_ context.Context, id string) (db.DreamPolicy, error) {
	p, ok := f.policies[id]
	if !ok {
		return db.DreamPolicy{}, pgx.ErrNoRows
	}
	return p, nil
}

func (f *fakePolicyStore) UpdateDreamPolicyStatus(_ context.Context, arg db.UpdateDreamPolicyStatusParams) (int64, error) {
	p, ok := f.policies[arg.ID]
	if !ok {
		return 0, nil
	}
	p.Status = arg.Status
	f.policies[arg.ID] = p
	return 1, nil
}

func (f *fakePolicyStore) PublishDreamPolicyVersion(_ context.Context, arg db.PublishDreamPolicyVersionParams) (db.DreamPolicyVersion, error) {
	v := db.DreamPolicyVersion{PolicyID: arg.PolicyID, Version: arg.Version, Definition: arg.Definition}
	f.versions[fmt.Sprintf("%s@%d", arg.PolicyID, arg.Version)] = v
	return v, nil
}

func (f *fakePolicyStore) GetLatestDreamPolicyVersion(_ context.Context, policyID string) (db.DreamPolicyVersion, error) {
	var latest db.DreamPolicyVersion
	found := false
	for _, v := range f.versions {
		if v.PolicyID == policyID && (!found || v.Version > latest.Version) {
			latest, found = v, true
		}
	}
	if !found {
		return db.DreamPolicyVersion{}, pgx.ErrNoRows
	}
	return latest, nil
}

func (f *fakePolicyStore) ListPublishedDreamPolicies(_ context.Context, enterpriseID string) ([]db.DreamPolicy, error) {
	var out []db.DreamPolicy
	for _, p := range f.policies {
		if p.EnterpriseID == enterpriseID && p.Status == "published" {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakePolicyStore) ListPublishedDreamPoliciesBounded(ctx context.Context, a db.ListPublishedDreamPoliciesBoundedParams) ([]db.DreamPolicy, error) {
	rows, err := f.ListPublishedDreamPolicies(ctx, a.EnterpriseID)
	if int32(len(rows)) > a.ResultLimit {
		return rows[:a.ResultLimit], err
	}
	return rows, err
}

func (f *fakePolicyStore) GetWorkflow(_ context.Context, id string) (db.Workflow, error) {
	if id != "wf-dream" {
		return db.Workflow{}, pgx.ErrNoRows
	}
	return db.Workflow{ID: id, EnterpriseID: "ent_1", Kind: string(sdkworkflow.KindDream)}, nil
}
func (f *fakePolicyStore) GetWorkflowVersion(_ context.Context, a db.GetWorkflowVersionParams) (db.WorkflowVersion, error) {
	if a.WorkflowID != "wf-dream" || a.Version != 3 {
		return db.WorkflowVersion{}, pgx.ErrNoRows
	}
	raw, _ := json.Marshal(sdkworkflow.Workflow{WorkflowID: "wf-dream", Version: 3, Kind: sdkworkflow.KindDream, Nodes: []sdkworkflow.Node{{ID: "aggregate", Type: sdkworkflow.NodeDreamAggregate}, {ID: "trace", Type: sdkworkflow.NodeTraceAppend}}, Edges: []sdkworkflow.Edge{{From: "aggregate", To: "trace"}}})
	return db.WorkflowVersion{WorkflowID: a.WorkflowID, Version: a.Version, Definition: raw}, nil
}

// fakeOutlineStore backs the method-outline routes in memory.
type fakeOutlineStore struct {
	outlines map[string]db.MethodOutline
	indexed  []db.CreateIndexJobParams
}

func (f *fakeOutlineStore) CreateMethodOutline(_ context.Context, arg db.CreateMethodOutlineParams) (db.MethodOutline, error) {
	row := db.MethodOutline{ID: arg.ID, EnterpriseID: arg.EnterpriseID, Title: arg.Title, Outline: arg.Outline, OrgScope: arg.OrgScope}
	f.outlines[arg.ID] = row
	return row, nil
}

func (f *fakeOutlineStore) ListMethodOutlines(_ context.Context, enterpriseID string) ([]db.MethodOutline, error) {
	var out []db.MethodOutline
	for _, row := range f.outlines {
		if row.EnterpriseID == enterpriseID {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeOutlineStore) CreateIndexJob(_ context.Context, arg db.CreateIndexJobParams) (db.IndexJob, error) {
	f.indexed = append(f.indexed, arg)
	return db.IndexJob{ID: arg.ID, SourceType: arg.SourceType, SourceID: arg.SourceID}, nil
}

// fakeArtifactStore backs IngestUpload + CreateJob.
type fakeArtifactStore struct {
	artifacts map[string]db.Artifact
	jobs      map[string]db.ArtifactProcessingJob
}

func newFakeArtifactStore() *fakeArtifactStore {
	return &fakeArtifactStore{artifacts: map[string]db.Artifact{}, jobs: map[string]db.ArtifactProcessingJob{}}
}

func (f *fakeArtifactStore) CreateArtifact(_ context.Context, arg db.CreateArtifactParams) (db.Artifact, error) {
	a := db.Artifact{ID: arg.ID, EnterpriseID: arg.EnterpriseID, Filename: arg.Filename, ContentType: arg.ContentType, ObjectKey: arg.ObjectKey, SourceHash: arg.SourceHash}
	f.artifacts[arg.ID] = a
	return a, nil
}

func (f *fakeArtifactStore) GetArtifact(_ context.Context, id string) (db.Artifact, error) {
	a, ok := f.artifacts[id]
	if !ok {
		return db.Artifact{}, pgx.ErrNoRows
	}
	return a, nil
}

func (f *fakeArtifactStore) CreateArtifactJob(_ context.Context, arg db.CreateArtifactJobParams) (db.ArtifactProcessingJob, error) {
	j := db.ArtifactProcessingJob{ID: arg.ID, EnterpriseID: arg.EnterpriseID, ArtifactID: arg.ArtifactID, Status: arg.Status, ParserHint: arg.ParserHint}
	f.jobs[arg.ID] = j
	return j, nil
}

func (f *fakeArtifactStore) GetArtifactJob(_ context.Context, id string) (db.ArtifactProcessingJob, error) {
	j, ok := f.jobs[id]
	if !ok {
		return db.ArtifactProcessingJob{}, pgx.ErrNoRows
	}
	return j, nil
}

func (f *fakeArtifactStore) UpdateArtifactJobStatus(context.Context, db.UpdateArtifactJobStatusParams) (int64, error) {
	return 1, nil
}
func (f *fakeArtifactStore) InsertArtifactStep(context.Context, db.InsertArtifactStepParams) error {
	return nil
}
func (f *fakeArtifactStore) InsertParserProviderRun(context.Context, db.InsertParserProviderRunParams) error {
	return nil
}
func (f *fakeArtifactStore) CreateAtlasDocument(context.Context, db.CreateAtlasDocumentParams) (db.AtlasDocument, error) {
	return db.AtlasDocument{}, fmt.Errorf("not used")
}
func (f *fakeArtifactStore) GetAtlasDocument(context.Context, string) (db.AtlasDocument, error) {
	return db.AtlasDocument{}, pgx.ErrNoRows
}
func (f *fakeArtifactStore) GetAtlasDocumentByArtifact(context.Context, string) (db.AtlasDocument, error) {
	return db.AtlasDocument{}, pgx.ErrNoRows
}
func (f *fakeArtifactStore) InsertDocumentBlock(context.Context, db.InsertDocumentBlockParams) error {
	return nil
}
func (f *fakeArtifactStore) InsertDocumentSummary(context.Context, db.InsertDocumentSummaryParams) (db.DocumentSummary, error) {
	return db.DocumentSummary{}, fmt.Errorf("not used")
}
func (f *fakeArtifactStore) CreateEvidencePointer(context.Context, db.CreateEvidencePointerParams) (db.EvidencePointer, error) {
	return db.EvidencePointer{}, fmt.Errorf("not used")
}
func (f *fakeArtifactStore) CreateIndexJob(context.Context, db.CreateIndexJobParams) (db.IndexJob, error) {
	return db.IndexJob{}, fmt.Errorf("not used")
}

// memObjects is an in-memory object store.
type memObjects struct{ data map[string][]byte }

func (m *memObjects) Put(_ context.Context, key, _ string, data []byte) error {
	m.data[key] = data
	return nil
}
func (m *memObjects) Get(_ context.Context, key string) ([]byte, error) {
	d, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	return d, nil
}

// fakePlanStore backs retrieval.Service.CreatePlan + the step lister.
type fakePlanStore struct {
	steps map[string][]db.RetrievalPlanStep
}

func (f *fakePlanStore) CreateRetrievalPlan(_ context.Context, arg db.CreateRetrievalPlanParams) (db.RetrievalPlan, error) {
	return db.RetrievalPlan{ID: arg.ID, EnterpriseID: arg.EnterpriseID}, nil
}

func (f *fakePlanStore) InsertRetrievalPlanStep(_ context.Context, arg db.InsertRetrievalPlanStepParams) error {
	f.steps[arg.PlanID] = append(f.steps[arg.PlanID], db.RetrievalPlanStep{
		PlanID: arg.PlanID, StepNo: arg.StepNo, Kind: arg.Kind, Params: arg.Params,
	})
	return nil
}

func (f *fakePlanStore) InsertRetrievalResult(context.Context, db.InsertRetrievalResultParams) error {
	return nil
}

func (f *fakePlanStore) ListRetrievalPlanSteps(_ context.Context, planID string) ([]db.RetrievalPlanStep, error) {
	return f.steps[planID], nil
}

// adminToolLLM emits one draft_retrieval_plan call then final text.
type adminToolLLM struct{}

func (adminToolLLM) Name() string { return "test/admin-tool" }

func (adminToolLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	sawToolResult := false
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				sawToolResult = true
			}
		}
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if !sawToolResult {
			yield(&adkmodel.LLMResponse{
				Content: &genai.Content{Role: "model", Parts: []*genai.Part{{
					FunctionCall: &genai.FunctionCall{
						ID: "call_plan", Name: "draft_retrieval_plan",
						Args: map[string]any{"query": "我的工作内容"},
					},
				}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      genai.NewContentFromText("检索计划草稿已生成。", "model"),
			TurnComplete: true,
		}, nil)
	}
}

// failingAuditNexus wraps the mock but fails audit appends.
type failingAuditNexus struct{ *nexusclient.Mock }

func (f *failingAuditNexus) AppendAuditEvidence(context.Context, nexus.AppendAuditEvidenceRequest) (nexus.AppendAuditEvidenceResponse, error) {
	return nexus.AppendAuditEvidenceResponse{}, fmt.Errorf("audit chain down")
}

// --- tests -------------------------------------------------------------------

func adminMock() *nexusclient.Mock {
	mock := nexusclient.NewMock()
	mock.Tickets["tick_admin"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: "ent_1", ActorUserID: "admin",
		Scopes: []string{"admin"}, ExpiresAt: time.Now().Add(time.Hour),
	}
	mock.Tickets["tick_other"] = nexus.VerifyTicketResponse{
		Valid: true, EnterpriseID: "ent_1", ActorUserID: "u_li",
		Scopes: []string{"space.read"}, ExpiresAt: time.Now().Add(time.Hour),
	}
	return mock
}

func postJSONReq(t *testing.T, url, ticket string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-operation-default-key")
	if ticket != "" {
		req.Header.Set("X-Nexus-Ticket", ticket)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func newAgentTestServer(t *testing.T, nexusClient nexus.Client) (*httptest.Server, *fakeOutlineStore) {
	srv, outlines, _ := newAgentTestServerWithPolicyStore(t, nexusClient)
	return srv, outlines
}

func newAgentTestServerWithPolicyStore(t *testing.T, nexusClient nexus.Client) (*httptest.Server, *fakeOutlineStore, *fakePolicyStore) {
	t.Helper()
	wfSvc, err := workflow.NewService(newFakeWorkflowStore())
	if err != nil {
		t.Fatal(err)
	}
	agentRunner, err := agent.NewRunner(adminToolLLM{})
	if err != nil {
		t.Fatal(err)
	}
	outlines := &fakeOutlineStore{outlines: map[string]db.MethodOutline{}}
	policies := newFakePolicyStore()
	producer := tasks.NewRunner(tasks.NewMemBus())
	producer.AllowEnqueue(retrieval.JobTypeIndex)
	router := NewAgentRouter(AgentRouterDeps{
		Nexus: nexusClient, Agent: agentRunner, Workflows: wfSvc,
		Dreams:   dream.NewPolicyService(policies),
		Outlines: outlines, Runner: producer,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, outlines, policies
}

func TestAdminRoutesFailClosedWithoutTicket(t *testing.T) {
	srv, _ := newAgentTestServer(t, adminMock())

	// atlas-api routes
	planStore := &fakePlanStore{steps: map[string][]db.RetrievalPlanStep{}}
	apiRouter := NewRouter(RouterDeps{
		Nexus:     adminMock(),
		Retrieval: retrieval.NewService(planStore, nil, nil, nil),
		PlanSteps: planStore,
	})
	apiSrv := httptest.NewServer(apiRouter)
	defer apiSrv.Close()

	for _, path := range []string{
		srv.URL + "/v1/workflows",
		srv.URL + "/v1/workflows/wf_x/publish",
		srv.URL + "/v1/workflows/wf_x/runs",
		srv.URL + "/v1/dream-policies",
		srv.URL + "/v1/method-outlines",
		srv.URL + "/v1/agent/runs",
		srv.URL + "/v1/agent/runs/r1/messages",
		srv.URL + "/v1/agent/runs/r1/confirmations",
		apiSrv.URL + "/v1/retrieval/plans",
		apiSrv.URL + "/v1/artifacts/jobs",
	} {
		resp := postJSONReq(t, path, "", map[string]any{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without ticket = %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestWorkflowCreatePublishRoute(t *testing.T) {
	mock := adminMock()
	srv, _ := newAgentTestServer(t, mock)

	def := workflow.Definition{
		Kind: sdkworkflow.KindSOP, RiskLevel: sdkworkflow.RiskMedium,
		Nodes: []sdkworkflow.Node{
			{ID: "in", Type: sdkworkflow.NodeInputManual},
			{ID: "confirm", Type: sdkworkflow.NodeHumanConfirm, RequiresConfirmation: true},
		},
		Edges: []sdkworkflow.Edge{{From: "in", To: "confirm"}},
	}
	resp := postJSONReq(t, srv.URL+"/v1/workflows", "tick_admin", map[string]any{"name": "MES SOP", "definition": def})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	created := decodeBody(t, resp)
	wfID, _ := created["workflow_id"].(string)
	if wfID == "" {
		t.Fatalf("create response: %v", created)
	}

	resp = postJSONReq(t, srv.URL+"/v1/workflows/"+wfID+"/publish", "tick_admin", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish = %d", resp.StatusCode)
	}
	published := decodeBody(t, resp)
	if published["version"].(float64) != 1 {
		t.Fatalf("publish response: %v", published)
	}
	// audit chain received the publish event
	var audited bool
	for _, e := range mock.AuditLog {
		if e.Action == nexus.AuditWorkflowVersionPublished {
			if e.ResourceType != "workflow" || e.ResourceID != wfID {
				t.Fatalf("workflow audit binding=%+v", e)
			}
			audited = true
		}
	}
	if !audited {
		t.Fatal("publish must append audit evidence")
	}
}

func TestWorkflowPublishFailsClosedWhenAuditDown(t *testing.T) {
	srv, _ := newAgentTestServer(t, &failingAuditNexus{Mock: adminMock()})
	def := workflow.Definition{
		Kind: sdkworkflow.KindSOP, RiskLevel: sdkworkflow.RiskLow,
		Nodes: []sdkworkflow.Node{{ID: "in", Type: sdkworkflow.NodeInputManual}},
		Edges: []sdkworkflow.Edge{},
	}
	resp := postJSONReq(t, srv.URL+"/v1/workflows", "tick_admin", map[string]any{"name": "X", "definition": def})
	wfID := decodeBody(t, resp)["workflow_id"].(string)

	resp = postJSONReq(t, srv.URL+"/v1/workflows/"+wfID+"/publish", "tick_admin", map[string]any{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("publish with audit down = %d, want 500 (fail loud)", resp.StatusCode)
	}
}

func TestDreamPolicyRouteCreatesCanonicalDraftWithoutPublishing(t *testing.T) {
	mock := adminMock()
	srv, _, store := newAgentTestServerWithPolicyStore(t, mock)
	resp := postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_admin", map[string]any{
		"org_unit_id": "pg_mes", "timezone": "Asia/Shanghai", "schedule": "0 22 * * *",
		"input_sources": []string{"work_brief"}, "workflow": map[string]any{"id": "wf-dream", "version": 3},
		"visibility_level": "members", "confirmation_mode": "high_risk_only",
		"masking_rules": []string{`1[3-9]\d{9}`}, "risk_signal_rules": []string{`风险[:：]\S+`},
		"evidence_retention": "pointer_plus_display_summary", "output_space_id": "spc_1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("dream policy = %d", resp.StatusCode)
	}
	out := decodeBody(t, resp)
	if out["dream_policy_id"] == "" {
		t.Fatalf("response: %v", out)
	}
	if out["status"] != "draft" {
		t.Fatalf("create status = %v, want draft", out["status"])
	}
	if out["version"] != float64(0) || out["revision"] != float64(0) {
		t.Fatalf("draft create lifecycle = %v", out)
	}
	if len(store.versions) != 0 {
		t.Fatalf("create invoked Publish: %+v", store.versions)
	}
	policyID := out["dream_policy_id"].(string)
	stored := store.policies[policyID]
	if stored.Status != "draft" || stored.OrgScope != "pg_mes" {
		t.Fatalf("stored policy = %+v", stored)
	}
	var definition map[string]any
	if err := json.Unmarshal(stored.Draft, &definition); err != nil {
		t.Fatal(err)
	}
	if definition["org_unit_id"] != "pg_mes" || definition["timezone"] != "Asia/Shanghai" {
		t.Fatalf("stored non-canonical draft: %v", definition)
	}
	if _, legacy := definition["org_scope"]; legacy {
		t.Fatalf("stored draft retained legacy org_scope: %v", definition)
	}
	var audited bool
	for _, e := range mock.AuditLog {
		if e.Action == nexus.AuditDreamPolicyCreateRequested {
			if e.ResourceType != "dream_policy" || e.ResourceID != policyID {
				t.Fatalf("dream policy audit binding=%+v", e)
			}
			audited = true
		}
	}
	if !audited {
		t.Fatal("dream policy creation must append audit evidence")
	}
	auditID := mock.AuditLog[len(mock.AuditLog)-1].Details["dream_policy_id"]
	if auditID != policyID {
		t.Fatalf("audit id %v != response id %s", auditID, policyID)
	}
	if phase := mock.AuditLog[len(mock.AuditLog)-1].Details["phase"]; phase != "create_attempt" {
		t.Fatalf("audit phase = %v", phase)
	}
}

func TestDreamPolicyCreateAuditFailureLeavesNoDraftOrVersion(t *testing.T) {
	srv, _, store := newAgentTestServerWithPolicyStore(t, &failingAuditNexus{Mock: adminMock()})
	resp := postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_admin", map[string]any{
		"org_unit_id": "pg_mes", "timezone": "Asia/Shanghai", "schedule": "0 22 * * *",
		"input_sources": []string{"work_brief"}, "workflow": map[string]any{"id": "wf-dream", "version": 3},
		"visibility_level": "members", "confirmation_mode": "high_risk_only", "output_space_id": "spc_1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("audit failure status = %d, want 500", resp.StatusCode)
	}
	if len(store.policies) != 0 || len(store.versions) != 0 {
		t.Fatalf("audit failure persisted state: policies=%v versions=%v", store.policies, store.versions)
	}
}

func TestDreamPolicyListMigratesLegacyPublishedDraftJSON(t *testing.T) {
	srv, _, store := newAgentTestServerWithPolicyStore(t, adminMock())
	store.policies["legacy"] = db.DreamPolicy{
		ID: "legacy", EnterpriseID: "ent_1", OrgScope: "department:legacy", Status: "published",
		Draft: []byte(`{"org_scope":"department:legacy","schedule":"0 22 * * *","input_sources":["work_briefs"],"visibility_level":"members","evidence_retention":"pointer_only","output_space_id":"space-old"}`),
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/dream-policies", nil)
	req.Header.Set("X-Nexus-Ticket", "tick_admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out := decodeBody(t, resp)
	policies := out["dream_policies"].([]any)
	if len(policies) != 1 {
		t.Fatalf("legacy list = %v", out)
	}
	listed := policies[0].(map[string]any)
	definition := listed["policy"].(map[string]any)
	if listed["status"] != "published" || definition["org_unit_id"] != "department:legacy" {
		t.Fatalf("legacy policy was not migrated: %v", listed)
	}
	if sources := definition["input_sources"].([]any); len(sources) != 1 || sources[0] != "work_brief" {
		t.Fatalf("legacy sources were not normalized: %v", definition)
	}
}

func TestAgentRunRoutes(t *testing.T) {
	srv, _ := newAgentTestServer(t, adminMock())

	resp := postJSONReq(t, srv.URL+"/v1/agent/runs", "tick_admin", map[string]any{"message": "给我一个检索计划"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("run = %d", resp.StatusCode)
	}
	out := decodeBody(t, resp)
	runID, _ := out["run_id"].(string)
	tools, _ := out["tool_calls"].([]any)
	if runID == "" || len(tools) == 0 {
		t.Fatalf("agent run must record a tool call: %v", out)
	}
	first := tools[0].(map[string]any)
	if first["name"] != "draft_retrieval_plan" {
		t.Fatalf("tool call = %v", first)
	}

	// continuation by the same actor works
	resp = postJSONReq(t, srv.URL+"/v1/agent/runs/"+runID+"/messages", "tick_admin", map[string]any{"message": "继续"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("message = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// a different actor may not continue the run — fail closed
	resp = postJSONReq(t, srv.URL+"/v1/agent/runs/"+runID+"/messages", "tick_other", map[string]any{"message": "偷看"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-actor continuation = %d, want 403", resp.StatusCode)
	}

	// unknown run id
	resp = postJSONReq(t, srv.URL+"/v1/agent/runs/ghost/messages", "tick_admin", map[string]any{"message": "x"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404", resp.StatusCode)
	}
}

func TestArtifactJobAndRetrievalPlanRoutes(t *testing.T) {
	mock := adminMock()
	producer := tasks.NewRunner(tasks.NewMemBus())
	producer.AllowEnqueue(artifacts.JobTypeArtifact)
	artifactSvc := artifacts.NewService(newFakeArtifactStore(), &memObjects{data: map[string][]byte{}}, nil, producer, nil)
	planStore := &fakePlanStore{steps: map[string][]db.RetrievalPlanStep{}}

	router := NewRouter(RouterDeps{
		Nexus:     mock,
		Retrieval: retrieval.NewService(planStore, nil, nil, nil),
		Artifacts: artifactSvc,
		PlanSteps: planStore,
	})
	// planStepLister comes from RouterDeps.Store (*db.Queries) in prod; the
	// test router wires the fake through the same handler surface.
	srv := httptest.NewServer(router)
	defer srv.Close()

	// artifact job (multipart)
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("parser_hint", "auto")
	_ = w.WriteField("content_type", "text/markdown")
	part, _ := w.CreateFormFile("file", "sop.md")
	_, _ = part.Write([]byte("# SOP"))
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/artifacts/jobs", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Nexus-Ticket", "tick_admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("artifact job = %d", resp.StatusCode)
	}
	out := decodeBody(t, resp)
	if out["artifact_id"] == "" || out["job_id"] == "" {
		t.Fatalf("artifact response: %v", out)
	}

	// retrieval plan
	resp = postJSONReq(t, srv.URL+"/v1/retrieval/plans", "tick_admin", map[string]any{"query": "我的工作内容是什么"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("plan = %d", resp.StatusCode)
	}
	plan := decodeBody(t, resp)
	steps, _ := plan["steps"].([]any)
	// keyword-only deployment (nil embedder/reranker) plans keyword + filter
	if plan["plan_id"] == "" || len(steps) < 2 {
		t.Fatalf("plan response: %v", plan)
	}
	first := steps[0].(map[string]any)
	if first["kind"] != "keyword" {
		t.Fatalf("first step = %v", first)
	}
}

var _ = strings.TrimSpace // keep strings import if assertions change

func TestMethodOutlineAndDreamListRoutes(t *testing.T) {
	srv, outlines := newAgentTestServer(t, adminMock())

	// import an outline -> stored + queued for indexing
	resp := postJSONReq(t, srv.URL+"/v1/method-outlines", "tick_admin", map[string]any{
		"title":     "MES 异常工单排查方法",
		"org_scope": "department:d1",
		"outline": map[string]any{
			"sections": []any{
				map[string]any{"title": "定位", "steps": []any{"确认工单编号", "核对产线"}},
				map[string]any{"title": "处置", "steps": []any{"排查原因", "上报限流风险"}},
			},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("outline create = %d", resp.StatusCode)
	}
	created := decodeBody(t, resp)
	if created["method_outline_id"] == "" {
		t.Fatalf("create response: %v", created)
	}
	if len(outlines.indexed) != 1 || outlines.indexed[0].SourceType != "method_outline" {
		t.Fatalf("outline must enqueue an index job: %+v", outlines.indexed)
	}

	// invalid outline JSON fails loud
	raw := []byte(`{"title":"x","outline":"not-an-object`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/method-outlines", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nexus-Ticket", "tick_admin")
	badResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("broken outline = %d, want 400", badResp.StatusCode)
	}

	// list returns the imported outline
	listReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/method-outlines", nil)
	listReq.Header.Set("X-Nexus-Ticket", "tick_admin")
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	listed := decodeBody(t, listResp)
	items, _ := listed["method_outlines"].([]any)
	if len(items) != 1 {
		t.Fatalf("outline list: %v", listed)
	}

	// Dream policy creation remains a draft and does not enter the published list.
	resp = postJSONReq(t, srv.URL+"/v1/dream-policies", "tick_admin", map[string]any{
		"org_unit_id": "pg_mes", "timezone": "Asia/Shanghai", "schedule": "0 22 * * *",
		"input_sources": []string{"work_brief"}, "workflow": map[string]any{"id": "wf-dream", "version": 3},
		"visibility_level": "members", "confirmation_mode": "high_risk_only",
		"masking_rules": []string{`1[3-9]\d{9}`}, "risk_signal_rules": []string{`风险[:：]\S+`},
		"evidence_retention": "pointer_plus_display_summary", "output_space_id": "spc_1",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("dream create = %d", resp.StatusCode)
	}
	resp.Body.Close()
	dlReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/dream-policies", nil)
	dlReq.Header.Set("X-Nexus-Ticket", "tick_admin")
	dlResp, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		t.Fatal(err)
	}
	dreamList := decodeBody(t, dlResp)
	policies, _ := dreamList["dream_policies"].([]any)
	if len(policies) != 0 {
		t.Fatalf("draft leaked into published list: %v", dreamList)
	}
}
