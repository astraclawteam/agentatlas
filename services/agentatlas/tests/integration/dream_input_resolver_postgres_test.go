package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	dreamresolver "github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

func TestDreamInputResolverPostgresContracts(t *testing.T) {
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

	ent := newID("ent-input")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: ent, Name: "Dream input contract"}); err != nil {
		t.Fatal(err)
	}
	parentID, childID, grandchildID := newID("space-parent"), newID("space-child"), newID("space-grandchild")
	for _, space := range []db.InsertKnowledgeSpaceParams{
		{ID: parentID, EnterpriseID: ent, Kind: "company", Name: "Parent", OrgScope: "company:parent", OrgVersion: 1},
		{ID: childID, EnterpriseID: ent, Kind: "department", Name: "Child", OrgScope: "department:child", OrgVersion: 1},
		{ID: grandchildID, EnterpriseID: ent, Kind: "department", Name: "Grandchild", OrgScope: "department:grandchild", OrgVersion: 1},
	} {
		if _, err := q.InsertKnowledgeSpace(ctx, space); err != nil {
			t.Fatal(err)
		}
	}
	for _, binding := range []db.UpsertOrgScopeBindingParams{
		{EnterpriseID: ent, SpaceID: parentID, ScopeKind: "company", ScopeID: "parent"},
		{EnterpriseID: ent, SpaceID: childID, ScopeKind: "department", ScopeID: "child", ParentScopeKind: pgtype.Text{String: "company", Valid: true}, ParentScopeID: pgtype.Text{String: "parent", Valid: true}},
		{EnterpriseID: ent, SpaceID: grandchildID, ScopeKind: "department", ScopeID: "grandchild", ParentScopeKind: pgtype.Text{String: "department", Valid: true}, ParentScopeID: pgtype.Text{String: "child", Valid: true}},
	} {
		if err := q.UpsertOrgScopeBinding(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}

	children, err := q.ListDreamImmediateChildren(ctx, db.ListDreamImmediateChildrenParams{
		EnterpriseID: ent, ParentScopeKind: "company", ParentScopeID: "parent", ResultLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ID != childID || children[0].ParentSpaceID != parentID || children[0].ParentScopeKind != "company" || children[0].ParentScopeID != "parent" || children[0].ParentOrgScope != "company:parent" {
		t.Fatalf("immediate-child provenance: %+v", children)
	}

	collisionCompanyID, collisionDepartmentID, collisionChildID := newID("space-collision-company"), newID("space-collision-department"), newID("space-collision-child")
	for _, space := range []db.InsertKnowledgeSpaceParams{
		{ID: collisionCompanyID, EnterpriseID: ent, Kind: "company", Name: "Collision company", OrgScope: "company:collision", OrgVersion: 1},
		{ID: collisionDepartmentID, EnterpriseID: ent, Kind: "department", Name: "Collision department", OrgScope: "department:collision", OrgVersion: 1},
		{ID: collisionChildID, EnterpriseID: ent, Kind: "employee", Name: "Department child", OrgScope: "employee:collision-child", OrgVersion: 1},
	} {
		if _, err := q.InsertKnowledgeSpace(ctx, space); err != nil {
			t.Fatal(err)
		}
	}
	for _, binding := range []db.UpsertOrgScopeBindingParams{
		{EnterpriseID: ent, SpaceID: collisionCompanyID, ScopeKind: "company", ScopeID: "collision"},
		{EnterpriseID: ent, SpaceID: collisionDepartmentID, ScopeKind: "department", ScopeID: "collision"},
		// This child belongs to department:collision, never company:collision.
		{EnterpriseID: ent, SpaceID: collisionChildID, ScopeKind: "employee", ScopeID: "collision-child", ParentScopeKind: pgtype.Text{String: "department", Valid: true}, ParentScopeID: pgtype.Text{String: "collision", Valid: true}},
	} {
		if err := q.UpsertOrgScopeBinding(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	collisionChildren, err := q.ListDreamImmediateChildren(ctx, db.ListDreamImmediateChildrenParams{
		EnterpriseID: ent, ParentScopeKind: "company", ParentScopeID: "collision", ResultLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(collisionChildren) != 0 {
		t.Fatalf("cross-kind parent collision leaked child: %+v", collisionChildren)
	}
	// Drives ApplyOrgSpaceEvent directly. It used to go through
	// spaces.EnsureSpaceFromEvent, which was removed because the frozen
	// OrgEvent carries no org unit identity. The SQL and the reparenting
	// guarantee it asserts are unchanged; only the Go caller is.
	applyScope := func(version int64, parentKind string) {
		t.Helper()
		if _, err := q.ApplyOrgSpaceEvent(ctx, db.ApplyOrgSpaceEventParams{
			EventEnterpriseID: ent, EventOrgScope: "employee:reparent-user",
			NewSpaceID:     fmt.Sprintf("space-reparent-%d", version),
			EventScopeKind: "employee", EventSpaceName: "Reparent user",
			EventOrgVersion: version, EventScopeID: "reparent-user",
			EventParentScopeKind: pgtype.Text{String: parentKind, Valid: true},
			EventParentScopeID:   pgtype.Text{String: "collision", Valid: true},
			EventVersionSnapshot: []byte("{}"), EventMemberSnapshot: []byte("[]"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	applyScope(1, "department")
	applyScope(2, "company")
	var reparentKind, reparentID string
	if err := pool.QueryRow(ctx, `select parent_scope_kind,parent_scope_id from org_scope_bindings
		where enterprise_id=$1 and scope_kind='employee' and scope_id='reparent-user'`, ent).Scan(&reparentKind, &reparentID); err != nil {
		t.Fatal(err)
	}
	if reparentKind != "company" || reparentID != "collision" {
		t.Fatalf("newer PostgreSQL reparenting not persisted: kind=%q id=%q", reparentKind, reparentID)
	}
	bareCollisionInputs, bareCollisionCoverage, _, err := dreamresolver.NewInputResolver(q).Resolve(ctx, dreamresolver.ResolveRequest{
		EnterpriseID: ent, OrgUnitID: "collision", OrgUnitKind: "company", SpaceID: collisionCompanyID,
		Sources:     []sdkdream.Source{sdkdream.SourceChildDreamSummary},
		WindowStart: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		Visibility: []string{"collision", "company_sanitized"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bareCollisionInputs) != 0 || bareCollisionCoverage.ExpectedChildren != 1 {
		t.Fatalf("bare company parent crossed kind boundary: inputs=%+v coverage=%+v", bareCollisionInputs, bareCollisionCoverage)
	}

	workflowID := newID("wf-input")
	if _, err := q.CreateWorkflowDraft(ctx, db.CreateWorkflowDraftParams{
		ID: workflowID, EnterpriseID: ent, Name: "Dream input", Kind: "dream", CreatedBy: "integration", Draft: []byte(`{"nodes":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PublishWorkflowVersion(ctx, db.PublishWorkflowVersionParams{
		WorkflowID: workflowID, Version: 1, Definition: []byte(`{"nodes":[]}`), RiskLevel: "low", PublishedBy: "integration",
	}); err != nil {
		t.Fatal(err)
	}
	policyID := newID("policy-input")
	if _, err := q.CreateDreamPolicy(ctx, db.CreateDreamPolicyParams{ID: policyID, EnterpriseID: ent, OrgScope: "department:child", Status: "draft", Draft: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PublishDreamPolicyVersion(ctx, db.PublishDreamPolicyVersionParams{PolicyID: policyID, Version: 1, Definition: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateDreamPolicyStatus(ctx, db.UpdateDreamPolicyStatusParams{ID: policyID, Status: "published"}); err != nil {
		t.Fatal(err)
	}
	windowStart := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)
	runID := newID("run-input")
	otherRunID := newID("run-input-other")
	for _, run := range []struct {
		id    string
		start time.Time
		end   time.Time
	}{
		{id: runID, start: windowStart, end: windowEnd},
		{id: otherRunID, start: windowStart.Add(-24 * time.Hour), end: windowStart},
	} {
		if _, err := q.CreateDreamRun(ctx, db.CreateDreamRunParams{
			ID: run.id, PolicyID: policyID, Version: 1, EnterpriseID: ent, Status: "running",
			WindowStart: ts(run.start), WindowEnd: ts(run.end), OrgUnitID: "department:child", PolicyVersion: 1,
			WorkflowID: workflowID, WorkflowVersion: 1, Timezone: "UTC",
			InputSnapshot: []byte(`{"source_counts":[],"sanitized_input_ids":[]}`), VisibilitySnapshot: []byte(`{"visibility_level":"company_sanitized","org_unit_ids":["company:parent"]}`),
			ModelRoute: "reasoning", ModelVersion: "v1", Attempt: 1,
			Coverage: []byte(`{"expected_children":0,"completed_children":0,"input_count":0}`), MissingInputs: []byte(`[]`), IdempotencyKey: run.id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	summaryID := newID("summary-input")
	for _, summary := range []db.CreateDreamSummaryParams{
		{ID: newID("summary-other"), RunID: otherRunID, EnterpriseID: ent, SpaceID: childID, Layer: "retrieval", SummaryText: "other run", RiskSignals: []byte(`[]`)},
		{ID: summaryID, RunID: runID, EnterpriseID: ent, SpaceID: childID, Layer: "retrieval", SummaryText: "exact run", RiskSignals: []byte(`[]`)},
	} {
		if _, err := q.CreateDreamSummary(ctx, summary); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{runID, otherRunID} {
		if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: id, Status: "succeeded", Error: ""}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := q.ListDreamCompletedChildRuns(ctx, db.ListDreamCompletedChildRunsParams{
		EnterpriseID: ent, ParentScopeKind: "company", ParentScopeID: "parent", WindowStart: ts(windowStart), WindowEnd: ts(windowEnd), ResultLimit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != runID || runs[0].ChildSpaceID != childID || runs[0].ParentSpaceID != parentID || runs[0].ParentScopeKind != "company" || runs[0].ParentScopeID != "parent" || runs[0].ParentOrgScope != "company:parent" {
		t.Fatalf("completed-run provenance: %+v", runs)
	}
	summary, err := q.GetDreamSummaryForRunLayer(ctx, db.GetDreamSummaryForRunLayerParams{
		EnterpriseID: ent, RunID: runID, SpaceID: childID, Layer: "retrieval",
	})
	if err != nil || summary.ID != summaryID {
		t.Fatalf("exact-run summary: %+v err=%v", summary, err)
	}

	for i, user := range []string{"u3", "u1", "u2"} {
		if err := q.UpsertSpaceMember(ctx, db.UpsertSpaceMemberParams{SpaceID: childID, UserID: user, DisplayName: user, OrgVersion: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	members, err := q.ListDreamSpaceMembers(ctx, db.ListDreamSpaceMembersParams{SpaceID: childID, ResultLimit: 2})
	if err != nil || len(members) != 2 || members[0].UserID != "u1" || members[1].UserID != "u2" {
		t.Fatalf("bounded members: %+v err=%v", members, err)
	}

	evidenceID := newID("ev-input")
	if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
		ID: evidenceID, EnterpriseID: ent, ResourceType: "work_brief", ResourceRef: "pointer", SourceSystem: "integration", RequiredScopes: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	for i, date := range []time.Time{windowStart.AddDate(0, 0, -1), windowStart, windowEnd} {
		if _, err := q.CreateWorkBrief(ctx, db.CreateWorkBriefParams{
			ID: newID("brief-input"), EnterpriseID: ent, EmployeeUserID: "u1", BriefDate: pgtype.Date{Time: date, Valid: true},
			Summary: "bounded brief", SourceHash: newID("hash-input"), EvidencePointerID: evidenceID, Topics: []string{}, ProjectRefs: []string{},
		}); err != nil {
			t.Fatalf("brief %d: %v", i, err)
		}
	}
	briefs, err := q.ListDreamWorkBriefsForWindow(ctx, db.ListDreamWorkBriefsForWindowParams{
		EnterpriseID: ent, EmployeeUserIds: []string{"u1"}, WindowStart: pgtype.Date{Time: windowStart, Valid: true},
		WindowEnd: pgtype.Date{Time: windowEnd, Valid: true}, ResultLimit: 10,
	})
	if err != nil || len(briefs) != 1 || !briefs[0].BriefDate.Time.Equal(windowStart) {
		t.Fatalf("exact brief dates: %+v err=%v", briefs, err)
	}

	timezoneSpaceID := newID("space-timezone-employee")
	if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
		ID: timezoneSpaceID, EnterpriseID: ent, Kind: "employee", Name: "Timezone employee", OrgScope: "employee:tz-user", OrgVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{
		EnterpriseID: ent, SpaceID: timezoneSpaceID, ScopeKind: "employee", ScopeID: "tz-user",
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertSpaceMember(ctx, db.UpsertSpaceMemberParams{SpaceID: timezoneSpaceID, UserID: "tz-user", DisplayName: "Timezone user", OrgVersion: 1}); err != nil {
		t.Fatal(err)
	}
	for i, date := range []time.Time{
		time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
	} {
		if _, err := q.CreateWorkBrief(ctx, db.CreateWorkBriefParams{
			ID: newID("brief-timezone"), EnterpriseID: ent, EmployeeUserID: "tz-user", BriefDate: pgtype.Date{Time: date, Valid: true},
			Summary: date.Format("2006-01-02"), SourceHash: newID("hash-timezone"), EvidencePointerID: evidenceID, Topics: []string{}, ProjectRefs: []string{},
		}); err != nil {
			t.Fatalf("timezone brief %d: %v", i, err)
		}
	}
	timezoneInputs, _, _, err := dreamresolver.NewInputResolver(q).Resolve(ctx, dreamresolver.ResolveRequest{
		EnterpriseID: ent, OrgUnitID: "employee:tz-user", SpaceID: timezoneSpaceID, Sources: []sdkdream.Source{sdkdream.SourceWorkBrief},
		WindowStart: time.Date(2026, 7, 11, 1, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC),
		Timezone: "Asia/Shanghai", Visibility: []string{"employee:tz-user"},
	})
	if err != nil || len(timezoneInputs) != 1 || timezoneInputs[0].SanitizedText != "2026-07-11" {
		t.Fatalf("Asia/Shanghai non-midnight brief window: inputs=%+v err=%v", timezoneInputs, err)
	}
	dstInputs, _, _, err := dreamresolver.NewInputResolver(q).Resolve(ctx, dreamresolver.ResolveRequest{
		EnterpriseID: ent, OrgUnitID: "employee:tz-user", SpaceID: timezoneSpaceID, Sources: []sdkdream.Source{sdkdream.SourceWorkBrief},
		WindowStart: time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), WindowEnd: time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC),
		Timezone: "America/New_York", Visibility: []string{"employee:tz-user"},
	})
	if err != nil || len(dstInputs) != 1 || dstInputs[0].SanitizedText != "2026-03-08" {
		t.Fatalf("America/New_York DST brief window: inputs=%+v err=%v", dstInputs, err)
	}

	for _, node := range []db.InsertTimelineNodeParams{
		{ID: newID("timeline-input"), EnterpriseID: ent, SpaceID: childID, OrgScope: "department:child", NodeTime: ts(windowStart), SourceType: "external_evidence", SummaryText: "inside", Tags: []string{}},
		{ID: newID("timeline-input"), EnterpriseID: ent, SpaceID: childID, OrgScope: "department:child", NodeTime: ts(windowEnd), SourceType: "external_evidence", SummaryText: "outside", Tags: []string{}},
		{ID: newID("timeline-input"), EnterpriseID: ent, SpaceID: childID, OrgScope: "department:child", NodeTime: ts(windowStart), SourceType: "agent_answer", SummaryText: "wrong source", Tags: []string{}},
		{ID: newID("timeline-input"), EnterpriseID: ent, SpaceID: childID, OrgScope: "department:child", NodeTime: ts(windowStart), SourceType: "project_record", SummaryText: "project", Tags: []string{}},
		{ID: newID("timeline-input"), EnterpriseID: ent, SpaceID: childID, OrgScope: "department:child", NodeTime: ts(windowStart), SourceType: "completed_task", SummaryText: "task", Tags: []string{}},
		{ID: newID("timeline-input"), EnterpriseID: ent, SpaceID: childID, OrgScope: "department:child", NodeTime: ts(windowStart), SourceType: "risk_event", SummaryText: "risk", Tags: []string{}},
	} {
		if _, err := q.InsertTimelineNode(ctx, node); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := q.ListDreamTimelineNodes(ctx, db.ListDreamTimelineNodesParams{
		EnterpriseID: ent, SpaceID: childID, OrgScope: "department:child", SourceType: "external_evidence",
		WindowStart: ts(windowStart), WindowEnd: ts(windowEnd), ResultLimit: 10,
	})
	if err != nil || len(nodes) != 1 || nodes[0].SummaryText != "inside" {
		t.Fatalf("exact timeline window/source: %+v err=%v", nodes, err)
	}
}
