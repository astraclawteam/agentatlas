package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

func TestDreamPolicyLifecyclePostgresCASAndAtomicPublish(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN")
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
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	enterprise := "ent-policy-" + suffix
	if _, err := pool.Exec(ctx, `insert into enterprises(id,name) values($1,'policy lifecycle')`, enterprise); err != nil {
		t.Fatal(err)
	}
	definition := []byte(`{"org_unit_id":"department:d1","timezone":"UTC","schedule":"0 22 * * *","input_sources":["work_brief"],"workflow":{"id":"wf-dream","version":1},"output_space_id":"space-1","visibility_level":"members","masking_rules":[],"risk_signal_rules":[],"evidence_retention":"pointer_only","confirmation_mode":"high_risk_only","max_attempts":3}`)
	policy := "policy-cas-" + suffix
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: policy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "editor", PermissionMode: "direct_edit", AuditRefID: "audit-create-" + suffix}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, e := q.UpdateDreamPolicyDraftIfRevision(ctx, db.UpdateDreamPolicyDraftIfRevisionParams{OrgScope: "department:d1", Draft: definition, AuditRefID: fmt.Sprintf("audit-update-%s-%d", suffix, i), TargetEnterpriseID: enterprise, TargetID: policy, ExpectedRevision: 0, ActorUserID: "editor"})
			errs <- e
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, stale := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, pgx.ErrNoRows) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("concurrent updates success=%d stale=%d", success, stale)
	}
	row, err := q.GetEnterpriseDreamPolicy(ctx, db.GetEnterpriseDreamPolicyParams{EnterpriseID: enterprise, ID: policy})
	if err != nil || row.Revision != 1 {
		t.Fatalf("CAS row=%+v err=%v", row, err)
	}

	publishPolicy := "policy-publish-" + suffix
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: publishPolicy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "editor", PermissionMode: "direct_edit", AuditRefID: "audit-create-publish-" + suffix}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dream_policies set status='approved',risk_level='high',review_mode='upward_review',reviewer_user_id='manager',decision='approve' where id=$1`, publishPolicy); err != nil {
		t.Fatal(err)
	}
	start = make(chan struct{})
	errs = make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, e := q.PublishDreamPolicyGoverned(ctx, db.PublishDreamPolicyGovernedParams{AuditRefID: fmt.Sprintf("audit-publish-%s-%d", suffix, i), TargetEnterpriseID: enterprise, TargetID: publishPolicy, ExpectedRevision: 0, ActorUserID: "editor"})
			errs <- e
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	success, stale = 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, pgx.ErrNoRows) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("concurrent publish success=%d stale=%d", success, stale)
	}
	var versions int
	if err := pool.QueryRow(ctx, `select count(*) from dream_policy_versions where policy_id=$1`, publishPolicy).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("versions=%d err=%v", versions, err)
	}

	auditPolicy := "policy-audit-" + suffix
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: auditPolicy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "editor", PermissionMode: "direct_edit", AuditRefID: "audit-create-audit-" + suffix}); err != nil {
		t.Fatal(err)
	}
	duplicate := "audit-duplicate-" + suffix
	if _, err := pool.Exec(ctx, `insert into dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,audit_ref_id,actor_user_id) values($1,$2,0,'seed',$3,'editor')`, enterprise, auditPolicy, duplicate); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpdateDreamPolicyDraftIfRevision(ctx, db.UpdateDreamPolicyDraftIfRevisionParams{OrgScope: "department:d1", Draft: definition, AuditRefID: duplicate, TargetEnterpriseID: enterprise, TargetID: auditPolicy, ExpectedRevision: 0, ActorUserID: "editor"}); err == nil {
		t.Fatal("duplicate audit unexpectedly committed transition")
	}
	row, err = q.GetEnterpriseDreamPolicy(ctx, db.GetEnterpriseDreamPolicyParams{EnterpriseID: enterprise, ID: auditPolicy})
	if err != nil || row.Revision != 0 {
		t.Fatalf("audit failure mutated row=%+v err=%v", row, err)
	}
}
