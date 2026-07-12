// Dream job integration test: real PostgreSQL + real MinIO from compose.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres minio minio-init
//	ATLAS_TEST_POSTGRES_DSN=... ATLAS_TEST_OBJECT_ENDPOINT=http://localhost:9000 \
//	  go test ./tests/integration -run TestDreamJob
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/config"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
)

func TestDreamJob(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	objEndpoint := os.Getenv("ATLAS_TEST_OBJECT_ENDPOINT")
	if dsn == "" || objEndpoint == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN and ATLAS_TEST_OBJECT_ENDPOINT")
	}
	ctx := context.Background()

	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	q := db.New(pool)

	objects, err := storage.NewObjectStore(config.ObjectStorage{
		Endpoint: objEndpoint, Bucket: "agentatlas",
		AccessKey: envOr("ATLAS_TEST_OBJECT_ACCESS_KEY", "atlas"),
		SecretKey: envOr("ATLAS_TEST_OBJECT_SECRET_KEY", "atlas-secret-key"),
	})
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	if err := objects.EnsureBucket(ctx); err != nil {
		t.Fatalf("bucket: %v", err)
	}

	// org fixture: enterprise + project group space + 2 members + briefs
	entID := newID("ent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: entID, Name: "测试企业"}); err != nil {
		t.Fatal(err)
	}
	spaceID := newID("spc")
	if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
		ID: spaceID, EnterpriseID: entID, Kind: "project_group",
		Name: "MES 异常工单专项组", OrgScope: "project_group:pg1", OrgVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"u_zhang", "u_li"} {
		if err := q.UpsertSpaceMember(ctx, db.UpsertSpaceMemberParams{
			SpaceID: spaceID, UserID: u, DisplayName: u, OrgVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	windowEnd := time.Date(2026, 7, 6, 22, 0, 0, 0, time.UTC)
	for i, item := range []struct{ user, summary string }{
		{"u_zhang", "完成分拣规则联调，风险:接口限流未定，电话13800138000。"},
		{"u_li", "整理 MES 工单模板。"},
	} {
		epID := newID("ev")
		if _, err := q.CreateEvidencePointer(ctx, db.CreateEvidencePointerParams{
			ID: epID, EnterpriseID: entID, ResourceType: "work_brief",
			ResourceRef: "fs://briefs/" + item.user, SourceSystem: "filesystem",
			RequiredScopes: []string{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateWorkBrief(ctx, db.CreateWorkBriefParams{
			ID: newID("wb"), EnterpriseID: entID, EmployeeUserID: item.user,
			BriefDate: pgtype.Date{Time: windowEnd.Add(-6 * time.Hour), Valid: true},
			Summary:   item.summary, SourceHash: newID("sha"), EvidencePointerID: epID,
			Topics: []string{}, ProjectRefs: []string{},
		}); err != nil {
			t.Fatalf("brief %d: %v", i, err)
		}
	}
	if _, err := q.CreateWorkflowDraft(ctx, db.CreateWorkflowDraftParams{
		ID: "legacy-direct-dream", EnterpriseID: entID, Name: "Legacy direct Dream",
		Kind: "dream", CreatedBy: "integration", Draft: []byte(`{"nodes":[]}`),
	}); err != nil {
		t.Fatalf("Dream workflow: %v", err)
	}
	if _, err := q.PublishWorkflowVersion(ctx, db.PublishWorkflowVersionParams{
		WorkflowID: "legacy-direct-dream", Version: 1, Definition: []byte(`{"nodes":[]}`),
		RiskLevel: "low", PublishedBy: "integration",
	}); err != nil {
		t.Fatalf("Dream workflow version: %v", err)
	}

	// policy: draft -> publish
	policySvc := dream.NewPolicyService(q)
	policyID, err := policySvc.CreateDraft(ctx, entID, dream.Policy(sdkdream.DreamPolicyDefinition{
		OrgUnitID: "project_group:pg1", Timezone: "UTC", Schedule: "0 22 * * *",
		InputSources:      []sdkdream.Source{sdkdream.SourceWorkBrief},
		Workflow:          sdkdream.WorkflowRef{ID: "legacy-direct-dream", Version: 1},
		VisibilityLevel:   sdkdream.VisibilityMembers,
		MaskingRules:      []string{`1[3-9]\d{9}`},
		RiskSignalRules:   []string{`风险[:：]\S+`},
		EvidenceRetention: sdkdream.EvidencePointerPlusDisplaySummary,
		ConfirmationMode:  sdkdream.ConfirmationHighRiskOnly,
		OutputSpaceID:     spaceID,
	}))
	if err != nil {
		t.Fatalf("policy draft: %v", err)
	}
	if _, err := policySvc.Publish(ctx, policyID); err != nil {
		t.Fatalf("policy publish: %v", err)
	}

	// scheduler tick dispatches; MemBus executes synchronously
	runner := tasks.NewRunner(tasks.NewMemBus())
	dreamRunner := dream.NewRunner(q, objects, policySvc, runner, nil)
	if err := dreamRunner.RegisterJobHandler(); err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	scheduler := dream.NewScheduler(q, policySvc, runner)
	now := windowEnd.Add(30 * time.Minute)
	n, err := scheduler.Tick(ctx, entID, now)
	if err != nil || n != 1 {
		t.Fatalf("tick dispatched %d err=%v", n, err)
	}
	// idempotent double tick
	n2, err := scheduler.Tick(ctx, entID, now)
	if err != nil || n2 != 0 {
		t.Fatalf("double tick dispatched %d err=%v", n2, err)
	}

	// layered summaries
	display, err := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
		SpaceID: spaceID, Layer: "display", Limit: 5,
	})
	if err != nil || len(display) != 1 {
		t.Fatalf("display summaries: %v n=%d", err, len(display))
	}
	if !strings.Contains(display[0].SummaryText, "2 名成员") || !strings.Contains(display[0].SummaryText, "风险信号 1 项") {
		t.Fatalf("display = %s", display[0].SummaryText)
	}
	retrieval, _ := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
		SpaceID: spaceID, Layer: "retrieval", Limit: 5,
	})
	if len(retrieval) != 1 || strings.Contains(retrieval[0].SummaryText, "13800138000") {
		t.Fatalf("retrieval layer: %+v", retrieval)
	}

	// sealed detail lives in object storage; DB keeps pointer only
	sealed, _ := q.ListDreamSummariesBySpace(ctx, db.ListDreamSummariesBySpaceParams{
		SpaceID: spaceID, Layer: "sealed_pointer", Limit: 5,
	})
	if len(sealed) != 1 || sealed[0].SummaryText != "" || sealed[0].SealedObjectKey == "" {
		t.Fatalf("sealed layer: %+v", sealed)
	}
	raw, err := objects.Get(ctx, sealed[0].SealedObjectKey)
	if err != nil {
		t.Fatalf("sealed object: %v", err)
	}
	if !strings.Contains(string(raw), "## u_zhang") || strings.Contains(string(raw), "13800138000") {
		t.Fatalf("sealed content wrong or unmasked: %s", raw)
	}

	// timeline node on the space
	nodes, err := q.ListTimelineNodes(ctx, db.ListTimelineNodesParams{SpaceID: spaceID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range nodes {
		if n.SourceType == "dream_summary" {
			found = true
		}
	}
	if !found {
		t.Fatal("dream timeline node missing")
	}
}
