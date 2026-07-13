// Integration test against the production-standard PostgreSQL from
// deploy/compose. Run:
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable \
//	  go test ./tests/integration -run TestPostgresCore
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func TestPostgresCore(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	q := db.New(pool)

	entID := newID("ent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: entID, Name: "顺视智能制造集团"}); err != nil {
		t.Fatalf("enterprise: %v", err)
	}

	// knowledge space + stale-event guard
	spaceID := newID("spc")
	space, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
		ID: spaceID, EnterpriseID: entID, Kind: "employee",
		Name: "张予安", OrgScope: "employee:" + newID("u"), OrgVersion: 5,
	})
	if err != nil {
		t.Fatalf("space: %v", err)
	}
	rows, err := q.UpdateKnowledgeSpaceIfNewer(ctx, db.UpdateKnowledgeSpaceIfNewerParams{
		ID: spaceID, EnterpriseID: entID, Name: "张予安(旧)", OrgVersion: 3,
	})
	if err != nil {
		t.Fatalf("stale update: %v", err)
	}
	if rows != 0 {
		t.Fatal("stale org event must not overwrite newer space state")
	}

	// evidence pointer + work brief + timeline node
	epID := newID("ev")
	if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
		ID: epID, EnterpriseID: entID, ResourceType: "work_brief",
		ResourceRef: "fs://briefs/2026-07-06.md", SourceSystem: "filesystem",
		ContentHash: "sha256:brief", RequiredScopes: []string{"brief.read"},
	}); err != nil {
		t.Fatalf("evidence: %v", err)
	}
	briefID := newID("wb")
	if _, err := q.CreateWorkBrief(ctx, db.CreateWorkBriefParams{
		ID: briefID, EnterpriseID: entID, EmployeeUserID: "u_zhang",
		BriefDate: pgtype.Date{Time: time.Now(), Valid: true},
		Summary:   "推进 MES 异常工单专项：完成分拣规则联调，风险=接口限流未定。",
		Topics:    []string{"MES", "异常工单"}, ProjectRefs: []string{"prj_mes"},
		SourceHash: newID("sha"), EvidencePointerID: epID,
	}); err != nil {
		t.Fatalf("brief: %v", err)
	}
	if _, err := q.InsertTimelineNode(ctx, db.InsertTimelineNodeParams{
		ID: newID("tl"), EnterpriseID: entID, SpaceID: spaceID,
		OrgScope: space.OrgScope, NodeTime: ts(time.Now()),
		SourceType: "work_brief", SummaryText: "完成分拣规则联调",
		Tags: []string{"MES"}, EvidencePointerID: pgtype.Text{String: epID, Valid: true},
	}); err != nil {
		t.Fatalf("timeline: %v", err)
	}
	nodes, err := q.ListTimelineNodes(ctx, db.ListTimelineNodesParams{SpaceID: spaceID, Limit: 10})
	if err != nil || len(nodes) != 1 {
		t.Fatalf("timeline list: %v (n=%d)", err, len(nodes))
	}

	// data boundary: oversized summary must be rejected by the schema
	longSummary := strings.Repeat("原文", 1500)
	if _, err := q.CreateWorkBrief(ctx, db.CreateWorkBriefParams{
		ID: newID("wb"), EnterpriseID: entID, EmployeeUserID: "u_zhang",
		BriefDate: pgtype.Date{Time: time.Now(), Valid: true},
		Summary:   longSummary, SourceHash: newID("sha"), EvidencePointerID: epID,
	}); err == nil {
		t.Fatal("oversized brief summary must be rejected (no-raw-copy boundary)")
	}

	// workflow draft -> publish -> run -> events
	wfID := newID("wf")
	if _, err := q.CreateWorkflowDraft(ctx, db.CreateWorkflowDraftParams{
		ID: wfID, EnterpriseID: entID, Name: "MES SOP", Kind: "sop",
		CreatedBy: "admin", Draft: []byte(`{"nodes":[]}`),
	}); err != nil {
		t.Fatalf("workflow draft: %v", err)
	}
	if _, err := q.PublishWorkflowVersion(ctx, db.PublishWorkflowVersionParams{
		WorkflowID: wfID, Version: 1, Definition: []byte(`{"nodes":[]}`),
		RiskLevel: "medium", PublishedBy: "admin",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	runID := newID("run")
	if _, err := q.CreateWorkflowRun(ctx, db.CreateWorkflowRunParams{
		ID: runID, WorkflowID: wfID, Version: 1, EnterpriseID: entID,
		Status: "running", Input: []byte(`{}`),
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := q.InsertWorkflowRunEvent(ctx, db.InsertWorkflowRunEventParams{
		RunID: runID, NodeID: "n1", Status: "succeeded", Detail: []byte(`{}`),
	}); err != nil {
		t.Fatalf("run event: %v", err)
	}
	if _, err := q.UpdateWorkflowRunStatus(ctx, db.UpdateWorkflowRunStatusParams{
		ID: runID, Status: "succeeded", Output: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("run status: %v", err)
	}

	// artifact -> job -> atlas document (object_key only) -> summary
	artID := newID("art")
	if _, err := q.CreateArtifact(ctx, db.CreateArtifactParams{
		ID: artID, EnterpriseID: entID, Filename: "mes.pdf",
		ContentType: "application/pdf", ObjectKey: "artifacts/" + artID,
		SourceHash: "sha256:pdf", SizeBytes: 1024,
	}); err != nil {
		t.Fatalf("artifact: %v", err)
	}
	jobID := newID("job")
	if _, err := q.CreateArtifactJob(ctx, db.CreateArtifactJobParams{
		ID: jobID, EnterpriseID: entID, ArtifactID: artID, Status: "pending", ParserHint: "auto",
	}); err != nil {
		t.Fatalf("job: %v", err)
	}
	if _, err := q.UpdateArtifactJobStatus(ctx, db.UpdateArtifactJobStatusParams{
		ID: jobID, Status: "succeeded", Error: "",
	}); err != nil {
		t.Fatalf("job status: %v", err)
	}
	docID := newID("doc")
	if _, err := q.CreateAtlasDocument(ctx, db.CreateAtlasDocumentParams{
		ID: docID, EnterpriseID: entID, ArtifactID: artID, SourceHash: "sha256:pdf",
		ContentType: "application/pdf", Provider: "docling",
		ObjectKey: "atlasdocs/" + docID + ".json", Confidence: 0.9,
	}); err != nil {
		t.Fatalf("atlas doc: %v", err)
	}
	if _, err := q.InsertDocumentSummary(ctx, db.InsertDocumentSummaryParams{
		AtlasDocumentID: docID, Level: "display", Ref: "", SummaryText: "MES 异常工单排查 SOP",
	}); err != nil {
		t.Fatalf("doc summary: %v", err)
	}

	// dream policy -> publish -> run -> layered summary
	polID := newID("pol")
	if _, err := q.CreateDreamPolicy(ctx, db.CreateDreamPolicyParams{
		ID: polID, EnterpriseID: entID, OrgScope: "department:研发一部",
		Status: "draft", Draft: []byte(`{"schedule":"0 22 * * *"}`),
	}); err != nil {
		t.Fatalf("dream policy: %v", err)
	}
	if _, err := q.PublishDreamPolicyVersion(ctx, db.PublishDreamPolicyVersionParams{
		PolicyID: polID, Version: 1, Definition: []byte(`{"schedule":"0 22 * * *"}`),
	}); err != nil {
		t.Fatalf("dream publish: %v", err)
	}
	if _, err := q.UpdateDreamPolicyStatus(ctx, db.UpdateDreamPolicyStatusParams{ID: polID, Status: "published"}); err != nil {
		t.Fatalf("dream status: %v", err)
	}
	drID := newID("dr")
	if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{
		ID: drID, PolicyID: polID, Version: 1, EnterpriseID: entID, Status: "running",
		WindowStart: ts(time.Now().Add(-24 * time.Hour)), WindowEnd: ts(time.Now()),
		OrgUnitID: "department:鐮斿彂涓€閮?", PolicyVersion: 1,
		WorkflowID: wfID, WorkflowVersion: 1, Timezone: "UTC",
		InputSnapshot:      []byte(`{"source_counts":[],"sanitized_input_ids":[]}`),
		VisibilitySnapshot: []byte(`{"visibility_level":"members","org_unit_ids":[],"masked_field_count":0}`),
		ModelRoute:         "workflow/" + wfID, ModelVersion: "v1", Attempt: 1,
		Coverage:      []byte(`{"expected_children":0,"completed_children":0,"input_count":0}`),
		MissingInputs: []byte(`[]`), IdempotencyKey: drID,
	}); err != nil {
		t.Fatalf("dream run: %v", err)
	}
	dsID := newID("dsum")
	if _, err := q.CreateDreamSummary(ctx, db.CreateDreamSummaryParams{
		ID: dsID, RunID: drID, EnterpriseID: entID, SpaceID: spaceID,
		Layer: "display", SummaryText: "项目组风险：接口限流方案未定",
		SealedObjectKey: "", RiskSignals: []byte(`["rate-limit"]`),
	}); err != nil {
		t.Fatalf("dream summary: %v", err)
	}

	// retrieval plan -> answer trace with evidence + audit ref
	planID := newID("rp")
	if _, err := q.CreateRetrievalPlan(ctx, db.CreateRetrievalPlanParams{
		ID: planID, EnterpriseID: entID, QueryHash: "sha256:q",
		SanitizedQuery: "我的工作内容", SpaceIds: []string{spaceID},
		OrgScopes: []string{space.OrgScope}, Filters: []byte(`{}`),
	}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := q.InsertRetrievalPlanStep(ctx, db.InsertRetrievalPlanStepParams{
		PlanID: planID, StepNo: 1, Kind: "keyword", Params: []byte(`{}`),
	}); err != nil {
		t.Fatalf("plan step: %v", err)
	}
	traceID := newID("tr")
	if _, err := q.CreateAnswerTrace(ctx, db.CreateAnswerTraceParams{
		ID: traceID, EnterpriseID: entID, CaseTicketID: "tick_1", ActorUserID: "u_zhang",
		QuestionHash: "sha256:q", SanitizedQuestionSummary: "员工询问当前工作内容",
		SpaceIds: []string{spaceID}, RetrievalPlanID: pgtype.Text{String: planID, Valid: true},
		EvidencePointerIds: []string{epID}, AgentnexusReadGrantIds: []string{"grant_1"},
		ModelRoute: "llmrouter/deepseek-v4", AnswerHash: "sha256:a",
	}); err != nil {
		t.Fatalf("trace: %v", err)
	}
	if err := q.InsertAnswerTraceEvidence(ctx, db.InsertAnswerTraceEvidenceParams{
		TraceID: traceID, EvidencePointerID: epID, GrantID: "grant_1",
	}); err != nil {
		t.Fatalf("trace evidence: %v", err)
	}
	if err := q.InsertAnswerTraceAuditRef(ctx, db.InsertAnswerTraceAuditRefParams{
		TraceID: traceID, AuditRefID: "audit_1",
	}); err != nil {
		t.Fatalf("audit ref: %v", err)
	}

	got, err := q.GetAnswerTrace(ctx, traceID)
	if err != nil {
		t.Fatalf("get trace: %v", err)
	}
	if got.CaseTicketID != "tick_1" || len(got.EvidencePointerIds) != 1 || got.EvidencePointerIds[0] != epID {
		t.Fatalf("trace roundtrip mismatch: %+v", got)
	}
}
