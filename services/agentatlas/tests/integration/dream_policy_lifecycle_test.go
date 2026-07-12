package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	dbfs "github.com/astraclawteam/agentatlas/services/agentatlas/db"
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
	createKey := "create-cas-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, createKey, "create", policy, "audit-create-"+suffix)
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: policy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "editor", PermissionMode: "direct_edit", OperationKey: createKey}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	updateKeys := make([]string, 2)
	updateAudits := make([]string, 2)
	for i := 0; i < 2; i++ {
		updateKeys[i] = fmt.Sprintf("update-cas-%s-%d", suffix, i)
		updateAudits[i] = fmt.Sprintf("audit-update-%s-%d", suffix, i)
		reserveLifecycleOperation(t, ctx, q, enterprise, updateKeys[i], "update", policy, updateAudits[i])
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, e := q.UpdateDreamPolicyDraftIfRevision(ctx, db.UpdateDreamPolicyDraftIfRevisionParams{OrgScope: "department:d1", Draft: definition, AuditRefID: pgtype.Text{String: updateAudits[i], Valid: true}, TargetEnterpriseID: enterprise, TargetID: policy, ExpectedRevision: 0, ActorUserID: "editor", OperationKey: updateKeys[i]})
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
	publishCreateKey := "create-publish-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, publishCreateKey, "create", publishPolicy, "audit-create-publish-"+suffix)
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: publishPolicy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "editor", PermissionMode: "direct_edit", OperationKey: publishCreateKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dream_policies set review_state='approved',pending_action='publish',risk_level='high',review_mode='upward_review',reviewer_user_id='manager',decision='approve' where id=$1`, publishPolicy); err != nil {
		t.Fatal(err)
	}
	start = make(chan struct{})
	errs = make(chan error, 2)
	publishKeys := make([]string, 2)
	publishAudits := make([]string, 2)
	for i := 0; i < 2; i++ {
		publishKeys[i] = fmt.Sprintf("publish-cas-%s-%d", suffix, i)
		publishAudits[i] = fmt.Sprintf("audit-publish-%s-%d", suffix, i)
		reserveLifecycleOperation(t, ctx, q, enterprise, publishKeys[i], "publish", publishPolicy, publishAudits[i])
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, e := q.PublishDreamPolicyGoverned(ctx, db.PublishDreamPolicyGovernedParams{AuditRefID: pgtype.Text{String: publishAudits[i], Valid: true}, TargetEnterpriseID: enterprise, TargetID: publishPolicy, ExpectedRevision: 0, ActorUserID: "editor", OperationKey: publishKeys[i]})
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
	auditCreateKey := "create-audit-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, auditCreateKey, "create", auditPolicy, "audit-create-audit-"+suffix)
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: auditPolicy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "editor", PermissionMode: "direct_edit", OperationKey: auditCreateKey}); err != nil {
		t.Fatal(err)
	}
	duplicate := "audit-duplicate-" + suffix
	seedKey := "seed-audit-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, seedKey, "update", auditPolicy, duplicate)
	if _, err := pool.Exec(ctx, `insert into dream_policy_transition_audits(enterprise_id,policy_id,revision,transition,operation_key,audit_ref_id,actor_user_id) values($1,$2,0,'seed',$3,$4,'editor')`, enterprise, auditPolicy, seedKey, duplicate); err != nil {
		t.Fatal(err)
	}
	conflictKey := "conflict-audit-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, conflictKey, "update", auditPolicy, duplicate+"-other")
	if _, err := q.UpdateDreamPolicyDraftIfRevision(ctx, db.UpdateDreamPolicyDraftIfRevisionParams{OrgScope: "department:d1", Draft: definition, AuditRefID: pgtype.Text{String: duplicate, Valid: true}, TargetEnterpriseID: enterprise, TargetID: auditPolicy, ExpectedRevision: 0, ActorUserID: "editor", OperationKey: conflictKey}); err == nil {
		t.Fatal("duplicate audit unexpectedly committed transition")
	}
	row, err = q.GetEnterpriseDreamPolicy(ctx, db.GetEnterpriseDreamPolicyParams{EnterpriseID: enterprise, ID: auditPolicy})
	if err != nil || row.Revision != 0 {
		t.Fatalf("audit failure mutated row=%+v err=%v", row, err)
	}

	boundPolicy := "policy-bound-" + suffix
	boundCreateKey := "create-bound-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, boundCreateKey, "create", boundPolicy, "audit-create-bound-"+suffix)
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: boundPolicy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "editor", PermissionMode: "direct_edit", OperationKey: boundCreateKey}); err != nil {
		t.Fatal(err)
	}
	boundKey := "update-bound-" + suffix
	boundAudit := "audit-update-bound-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, boundKey, "update", boundPolicy, boundAudit)
	for name, params := range map[string]db.UpdateDreamPolicyDraftIfRevisionParams{
		"policy": {OrgScope: "department:d1", Draft: definition, AuditRefID: pgtype.Text{String: boundAudit, Valid: true}, TargetEnterpriseID: enterprise, TargetID: auditPolicy, ExpectedRevision: 0, ActorUserID: "editor", OperationKey: boundKey},
		"actor":  {OrgScope: "department:d1", Draft: definition, AuditRefID: pgtype.Text{String: boundAudit, Valid: true}, TargetEnterpriseID: enterprise, TargetID: boundPolicy, ExpectedRevision: 0, ActorUserID: "other", OperationKey: boundKey},
		"audit":  {OrgScope: "department:d1", Draft: definition, AuditRefID: pgtype.Text{String: "different-audit", Valid: true}, TargetEnterpriseID: enterprise, TargetID: boundPolicy, ExpectedRevision: 0, ActorUserID: "editor", OperationKey: boundKey},
	} {
		if _, err := q.UpdateDreamPolicyDraftIfRevision(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s operation substitution err=%v", name, err)
		}
	}
	concurrent := make(chan error, 2)
	start = make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := q.UpdateDreamPolicyDraftIfRevision(ctx, db.UpdateDreamPolicyDraftIfRevisionParams{OrgScope: "department:d1", Draft: definition, AuditRefID: pgtype.Text{String: boundAudit, Valid: true}, TargetEnterpriseID: enterprise, TargetID: boundPolicy, ExpectedRevision: 0, ActorUserID: "editor", OperationKey: boundKey})
			concurrent <- err
		}()
	}
	close(start)
	wg.Wait()
	close(concurrent)
	success, stale = 0, 0
	for err := range concurrent {
		if err == nil {
			success++
		} else {
			stale++
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("same operation transition success=%d stale=%d", success, stale)
	}
	var transitionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM dream_policy_transition_audits WHERE enterprise_id=$1 AND operation_key=$2`, enterprise, boundKey).Scan(&transitionCount); err != nil || transitionCount != 1 {
		t.Fatalf("same operation transition audits=%d err=%v", transitionCount, err)
	}

	refreshPolicy := "policy-refresh-" + suffix
	refreshCreateKey := "create-refresh-" + suffix
	reserveLifecycleOperationActor(t, ctx, q, enterprise, refreshCreateKey, "create", refreshPolicy, "creator", "audit-create-refresh-"+suffix)
	if _, err := q.CreateDreamPolicyLifecycle(ctx, db.CreateDreamPolicyLifecycleParams{ID: refreshPolicy, EnterpriseID: enterprise, OrgScope: "department:d1", Draft: definition, RequesterUserID: "creator", PermissionMode: "direct_edit", OperationKey: refreshCreateKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE dream_policies SET pending_action='publish',review_state='pending',risk_level='high',review_mode='enterprise_knowledge_admin_queue',review_queue='admins' WHERE id=$1`, refreshPolicy); err != nil {
		t.Fatal(err)
	}
	refreshKey := "review-refresh-" + suffix
	refreshAudit := "audit-review-refresh-" + suffix
	reserveLifecycleOperation(t, ctx, q, enterprise, refreshKey, "review", refreshPolicy, refreshAudit)
	if _, err := q.RefreshDreamPolicyReviewRoute(ctx, db.RefreshDreamPolicyReviewRouteParams{TargetEnterpriseID: enterprise, OperationKey: refreshKey, TargetID: refreshPolicy, ActorUserID: "editor", AuditRefID: pgtype.Text{String: refreshAudit, Valid: true}, RiskLevel: "high", RiskReasons: []byte(`[]`), ReviewMode: "upward_review", ReviewerUserID: pgtype.Text{String: "manager", Valid: true}, ReviewOrgPath: []byte(`["department:d1"]`), ExpectedRevision: 0}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM dream_policy_transition_audits WHERE enterprise_id=$1 AND operation_key=$2 AND transition='review-refresh:publish'`, enterprise, refreshKey).Scan(&transitionCount); err != nil || transitionCount != 1 {
		t.Fatalf("refresh audit rows=%d err=%v", transitionCount, err)
	}

	legacyPolicy := "policy-legacy-" + suffix
	if _, err := pool.Exec(ctx, `INSERT INTO dream_policies(id,enterprise_id,org_scope,status,draft,requester_user_id) VALUES($1,$2,'department:d1','draft',$3,'')`, legacyPolicy, enterprise, definition); err != nil {
		t.Fatal(err)
	}
	legacyUpdateKey := "update-legacy-" + suffix
	legacyAudit := "audit-update-legacy-" + suffix
	reserveLifecycleOperationActor(t, ctx, q, enterprise, legacyUpdateKey, "update", legacyPolicy, "legacy-owner", legacyAudit)
	if _, err := q.UpdateDreamPolicyDraftIfRevision(ctx, db.UpdateDreamPolicyDraftIfRevisionParams{OrgScope: "department:d1", Draft: definition, AuditRefID: pgtype.Text{String: legacyAudit, Valid: true}, TargetEnterpriseID: enterprise, TargetID: legacyPolicy, ExpectedRevision: 0, ActorUserID: "legacy-owner", OperationKey: legacyUpdateKey}); err != nil {
		t.Fatal(err)
	}
	legacy, err := q.GetEnterpriseDreamPolicy(ctx, db.GetEnterpriseDreamPolicyParams{EnterpriseID: enterprise, ID: legacyPolicy})
	if err != nil || legacy.RequesterUserID != "legacy-owner" {
		t.Fatalf("legacy requester=%q err=%v", legacy.RequesterUserID, err)
	}
}

func TestDreamPolicyLifecycleMigrationDownRestoresV8(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	defer func() {
		if err := storage.Migrate(ctx, dsn); err != nil {
			t.Errorf("restore v9: %v", err)
		}
	}()
	enterprise := newID("ent_down")
	policy := newID("policy_down")
	if _, err := sqldb.ExecContext(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'down test')`, enterprise); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.ExecContext(ctx, `INSERT INTO dream_policies(id,enterprise_id,org_scope,status,draft,review_state) VALUES($1,$2,'department:d1','draft','{}','pending')`, policy, enterprise); err != nil {
		t.Fatal(err)
	}
	// Simulate the prerelease v9 representation that placed review state in
	// status; Down must map it before restoring the v8 status constraint.
	if _, err := sqldb.ExecContext(ctx, `ALTER TABLE dream_policies DROP CONSTRAINT dream_policies_status_check`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.ExecContext(ctx, `UPDATE dream_policies SET status='review_pending' WHERE id=$1`, policy); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, sqldb, "migrations", 8); err != nil {
		t.Fatalf("down to v8: %v", err)
	}
	version, err := goose.GetDBVersion(sqldb)
	if err != nil || version != 8 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var status string
	if err := sqldb.QueryRowContext(ctx, `SELECT status FROM dream_policies WHERE id=$1`, policy).Scan(&status); err != nil || status != "draft" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	var lifecycleColumns int
	if err := sqldb.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='dream_policies' AND column_name IN ('review_state','pending_action','revision')`).Scan(&lifecycleColumns); err != nil || lifecycleColumns != 0 {
		t.Fatalf("lifecycle columns=%d err=%v", lifecycleColumns, err)
	}
}

func reserveLifecycleOperation(t *testing.T, ctx context.Context, q *db.Queries, enterprise, key, kind, policy, audit string) {
	reserveLifecycleOperationActor(t, ctx, q, enterprise, key, kind, policy, "editor", audit)
}

func reserveLifecycleOperationActor(t *testing.T, ctx context.Context, q *db.Queries, enterprise, key, kind, policy, actor, audit string) {
	t.Helper()
	_, err := q.ReserveDreamPolicyOperation(ctx, db.ReserveDreamPolicyOperationParams{EnterpriseID: enterprise, OperationKey: key, OperationKind: kind, PolicyID: policy, ActorUserID: actor, RequestHash: strings.Repeat("a", 64), FactsNonce: "facts-" + key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.RecordDreamPolicyOperationAudit(ctx, db.RecordDreamPolicyOperationAuditParams{AuditRefID: pgtype.Text{String: audit, Valid: true}, EnterpriseID: enterprise, OperationKey: key}); err != nil {
		t.Fatal(err)
	}
}
