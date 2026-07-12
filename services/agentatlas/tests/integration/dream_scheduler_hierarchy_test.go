package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/tasks"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDreamSchedulerHierarchyPostgresConcurrentTicks(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	ent, workflowID := "ent-scheduler-"+suffix, "wf-scheduler-"+suffix
	parentSpace, childSpace := "space-parent-"+suffix, "space-child-"+suffix
	parentPolicy, childPolicy := "policy-parent-"+suffix, "policy-child-"+suffix
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: ent, Name: "Dream scheduler integration"}); err != nil {
		t.Fatal(err)
	}
	for _, space := range []db.InsertKnowledgeSpaceParams{
		{ID: parentSpace, EnterpriseID: ent, Kind: "department", Name: "Parent", OrgScope: "department:parent-" + suffix, OrgVersion: 1},
		{ID: childSpace, EnterpriseID: ent, Kind: "project_group", Name: "Child", OrgScope: "project_group:child-" + suffix, OrgVersion: 1},
	} {
		if _, err := q.InsertKnowledgeSpace(ctx, space); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{EnterpriseID: ent, SpaceID: parentSpace, ScopeKind: "department", ScopeID: "parent-" + suffix}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{EnterpriseID: ent, SpaceID: childSpace, ScopeKind: "project_group", ScopeID: "child-" + suffix, ParentScopeKind: pgtype.Text{String: "department", Valid: true}, ParentScopeID: pgtype.Text{String: "parent-" + suffix, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateWorkflowDraft(ctx, db.CreateWorkflowDraftParams{ID: workflowID, EnterpriseID: ent, Name: "Dream scheduler", Kind: "dream", CreatedBy: "integration", Draft: []byte(`{"nodes":[]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.PublishWorkflowVersion(ctx, db.PublishWorkflowVersionParams{WorkflowID: workflowID, Version: 1, Definition: []byte(`{"nodes":[]}`), RiskLevel: "low", PublishedBy: "integration"}); err != nil {
		t.Fatal(err)
	}
	addPolicy := func(id, org, output string, sources []sdkdream.Source) {
		t.Helper()
		definition := sdkdream.DreamPolicyDefinition{OrgUnitID: org, Timezone: "Asia/Shanghai", Schedule: "0 22 * * *", InputSources: sources, Workflow: sdkdream.WorkflowRef{ID: workflowID, Version: 1}, OutputSpaceID: output, VisibilityLevel: sdkdream.VisibilityMembers, ConfirmationMode: sdkdream.ConfirmationNever, MaxAttempts: 2}
		raw, err := json.Marshal(definition)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.CreateDreamPolicy(ctx, db.CreateDreamPolicyParams{ID: id, EnterpriseID: ent, OrgScope: org, Status: "published", Draft: raw}); err != nil {
			t.Fatal(err)
		}
		if _, err := q.PublishDreamPolicyVersion(ctx, db.PublishDreamPolicyVersionParams{PolicyID: id, Version: 1, Definition: raw}); err != nil {
			t.Fatal(err)
		}
	}
	addPolicy(parentPolicy, "department:parent-"+suffix, parentSpace, []sdkdream.Source{sdkdream.SourceChildDreamSummary})
	addPolicy(childPolicy, "project_group:child-"+suffix, childSpace, []sdkdream.Source{sdkdream.SourceWorkBrief})
	bus := &schedulerIntegrationBus{}
	runner := tasks.NewRunner(bus)
	runner.AllowEnqueue(dream.JobTypeDream)
	scheduler := dream.NewScheduler(q, dream.NewPolicyService(q), runner)
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	runConcurrentTicks(t, func() (int, error) { return scheduler.Tick(ctx, ent, now) })
	childRuns, err := q.ListDreamRunsByOrg(ctx, db.ListDreamRunsByOrgParams{EnterpriseID: ent, OrgUnitID: "project_group:child-" + suffix, ResultLimit: 10})
	if err != nil || len(childRuns) != 1 || childRuns[0].Status != "pending" {
		t.Fatalf("child runs=%+v err=%v", childRuns, err)
	}
	parentRuns, err := q.ListDreamRunsByOrg(ctx, db.ListDreamRunsByOrgParams{EnterpriseID: ent, OrgUnitID: "department:parent-" + suffix, ResultLimit: 10})
	if err != nil || len(parentRuns) != 0 {
		t.Fatalf("parent ran early: %+v err=%v", parentRuns, err)
	}
	if _, err := q.UpdateDreamRunStatus(ctx, db.UpdateDreamRunStatusParams{ID: childRuns[0].ID, Status: "succeeded", Error: ""}); err != nil {
		t.Fatal(err)
	}
	runConcurrentTicks(t, func() (int, error) { return scheduler.Tick(ctx, ent, now) })
	parentRuns, err = q.ListDreamRunsByOrg(ctx, db.ListDreamRunsByOrgParams{EnterpriseID: ent, OrgUnitID: "department:parent-" + suffix, ResultLimit: 10})
	var coverage sdkdream.Coverage
	if len(parentRuns) == 1 {
		_ = json.Unmarshal(parentRuns[0].Coverage, &coverage)
	}
	if err != nil || len(parentRuns) != 1 || coverage.ExpectedChildren != 1 || coverage.CompletedChildren != 1 {
		t.Fatalf("parent runs=%+v err=%v", parentRuns, err)
	}
	if bus.count() != 2 {
		t.Fatalf("duplicate concurrent publish count=%d", bus.count())
	}
}

func runConcurrentTicks(t *testing.T, tick func() (int, error)) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := tick(); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

type schedulerIntegrationBus struct {
	mu        sync.Mutex
	published []string
}

func (b *schedulerIntegrationBus) Publish(_ context.Context, _, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, id)
	return nil
}
func (*schedulerIntegrationBus) Subscribe(context.Context, string, func(context.Context, string)) (func(), error) {
	return func() {}, nil
}
func (b *schedulerIntegrationBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.published)
}
