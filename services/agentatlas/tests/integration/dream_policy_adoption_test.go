package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDreamPolicyAdoptionPostgresConcurrencyReplayAndLineage(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (dedicated PostgreSQL 17 database required)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)
	suffix := time.Now().UTC().Format("150405.000000000")
	ent, sourceID, targetID := "ent-adopt-"+suffix, "policy-suggestion-"+suffix, "policy-adopted-"+suffix
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Adoption integration')`, ent); err != nil {
		t.Fatal(err)
	}
	definition := sdkdream.DreamPolicyDefinition{OrgUnitID: "team", Timezone: "UTC", Schedule: "0 22 * * *", InputSources: []sdkdream.Source{sdkdream.SourceWorkBrief}, Workflow: sdkdream.WorkflowRef{ID: "wf-published", Version: 1}, OutputSpaceID: "space-team", VisibilityLevel: sdkdream.VisibilityManagers, EvidenceRetention: sdkdream.EvidencePointerOnly, ConfirmationMode: sdkdream.ConfirmationHighRiskOnly, MaxAttempts: 3}
	raw, _ := json.Marshal(definition)
	if _, err := pool.Exec(ctx, `INSERT INTO dream_policies(id,enterprise_id,org_scope,status,draft,requester_user_id,permission_mode,audit_ref_id) VALUES($1,$2,'team','draft',$3,'employee','suggestion_only','audit-source')`, sourceID, ent, raw); err != nil {
		t.Fatal(err)
	}
	svc := dream.NewPolicyService(q)
	key := "postgres-adoption-operation-0001-" + suffix
	if _, err := svc.BeginOperation(ctx, ent, key, "adopt", sourceID, "editor", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordOperationAudit(ctx, ent, key, "audit-adopt-"+suffix); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, callErr := svc.AdoptSuggestion(ctx, ent, sourceID, targetID, "editor", "audit-adopt-"+suffix, key, 0)
			errs <- callErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for callErr := range errs {
		if callErr != nil {
			t.Fatalf("concurrent replay failed: %v", callErr)
		}
	}
	source, err := q.GetEnterpriseDreamPolicy(ctx, db.GetEnterpriseDreamPolicyParams{EnterpriseID: ent, ID: sourceID})
	if err != nil || source.PermissionMode != "suggestion_only" || source.RequesterUserID != "employee" {
		t.Fatalf("source mutated: %+v err=%v", source, err)
	}
	target, err := q.GetEnterpriseDreamPolicy(ctx, db.GetEnterpriseDreamPolicyParams{EnterpriseID: ent, ID: targetID})
	if err != nil || target.PermissionMode != "direct_edit" || target.RequesterUserID != "editor" {
		t.Fatalf("target=%+v err=%v", target, err)
	}
	lineage, err := q.GetDreamPolicyAdoptionBySource(ctx, db.GetDreamPolicyAdoptionBySourceParams{EnterpriseID: ent, SourcePolicyID: sourceID, SourceRevision: 0})
	if err != nil || lineage.TargetPolicyID != targetID || lineage.AdopterUserID != "editor" || lineage.OperationKey != key {
		t.Fatalf("lineage=%+v err=%v", lineage, err)
	}

	competingSource := "policy-competing-source-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO dream_policies(id,enterprise_id,org_scope,status,draft,requester_user_id,permission_mode,audit_ref_id) VALUES($1,$2,'team','draft',$3,'employee-2','suggestion_only','audit-source-2')`, competingSource, ent, raw); err != nil {
		t.Fatal(err)
	}
	keys := []string{"postgres-adoption-compete-a-" + suffix, "postgres-adoption-compete-b-" + suffix}
	actors := []string{"editor-a", "editor-b"}
	targets := []string{"policy-competing-target-a-" + suffix, "policy-competing-target-b-" + suffix}
	for index := range keys {
		if _, err := svc.BeginOperation(ctx, ent, keys[index], "adopt", competingSource, actors[index], strings.Repeat(string(rune('b'+index)), 64)); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RecordOperationAudit(ctx, ent, keys[index], "audit-"+keys[index]); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		index int
		err   error
	}
	competingStart := make(chan struct{})
	results := make(chan result, 2)
	for index := range keys {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-competingStart
			_, callErr := svc.AdoptSuggestion(ctx, ent, competingSource, targets[index], actors[index], "audit-"+keys[index], keys[index], 0)
			results <- result{index: index, err: callErr}
		}(index)
	}
	close(competingStart)
	wg.Wait()
	close(results)
	winners := 0
	for outcome := range results {
		if outcome.err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("competing adopters winners=%d, want exactly one", winners)
	}
	var targetCount, lineageCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM dream_policies WHERE enterprise_id=$1 AND id=ANY($2)`, ent, targets).Scan(&targetCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM dream_policy_adoptions WHERE enterprise_id=$1 AND source_policy_id=$2 AND source_revision=0`, ent, competingSource).Scan(&lineageCount); err != nil {
		t.Fatal(err)
	}
	if targetCount != 1 || lineageCount != 1 {
		t.Fatalf("competing adoption target_count=%d lineage_count=%d", targetCount, lineageCount)
	}
}
