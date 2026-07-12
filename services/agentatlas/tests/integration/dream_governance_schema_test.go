// Integration test for immutable Dream lineage and governed change history.
//
// Run with the production-standard PostgreSQL from deploy/compose:
//
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable \
//	  go test ./tests/integration -run DreamGovernanceSchema -count=1
package integration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pressly/goose/v3"

	dbfs "github.com/astraclawteam/agentatlas/services/agentatlas/db"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

func TestDreamGovernanceSchemaUpgradeFromPopulatedV1(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()
	admin, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := newID("upgrade")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "drop schema "+quotedSchema+" cascade") })

	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1)
	if _, err := sqldb.ExecContext(ctx, "set search_path to "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, sqldb, "migrations", 1); err != nil {
		t.Fatalf("migrate v1: %v", err)
	}
	if _, err := sqldb.ExecContext(ctx, `
		insert into enterprises(id,name) values('upgrade-ent','Upgrade');
		insert into dream_policies(id,enterprise_id,org_scope,status,draft)
		values('upgrade-policy','upgrade-ent','department:upgrade','published','{}');
		insert into dream_runs(id,policy_id,version,enterprise_id,status,window_start,window_end)
		values('upgrade-run','upgrade-policy',7,'upgrade-ent','succeeded',now()-interval '1 day',now());
	`); err != nil {
		t.Fatalf("seed populated v1: %v", err)
	}
	if err := goose.UpContext(ctx, sqldb, "migrations"); err != nil {
		t.Fatalf("upgrade populated v1: %v", err)
	}
}

func TestDreamWorkflowExecutionMigrationContracts(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN")
	}
	seed := `insert into enterprises(id,name) values('e','E');
insert into knowledge_spaces(id,enterprise_id,kind,name,org_scope,org_version) values('s','e','department','S','department:d',1);
insert into workflows(id,enterprise_id,name,kind,draft,created_by) values('wa','e','A','dream','{}','x'),('wb','e','B','dream','{}','x');
insert into workflow_versions(workflow_id,version,definition,risk_level,published_by) values('wa',1,'{"workflow_id":"wa","version":1,"kind":"dream","nodes":[{"id":"a","type":"dream.aggregate"}],"edges":[],"risk_level":"low"}','low','x'),('wb',1,'{"workflow_id":"wb","version":1,"kind":"dream","nodes":[{"id":"a","type":"dream.aggregate"}],"edges":[],"risk_level":"low"}','low','x');
insert into dream_policies(id,enterprise_id,org_scope,status,draft) values('p','e','department:d','published','{}');insert into dream_policy_versions(policy_id,version,definition) values('p',1,'{}');
insert into dream_runs(id,policy_id,version,enterprise_id,status,window_start,window_end,org_unit_id,policy_version,workflow_id,workflow_version,timezone,input_snapshot,visibility_snapshot,model_route,model_version,attempt,coverage,missing_inputs,idempotency_key) values('dr','p',1,'e','waiting_confirmation',now()-interval '1 day',now(),'department:d',1,'wa',1,'UTC','{"source_counts":[],"sanitized_input_ids":[]}','{"visibility_level":"members","org_unit_ids":["department:d"],"masked_field_count":0}','workflow/wa','v1',1,'{"expected_children":0,"completed_children":0,"input_count":0}','[]','dr');`
	t.Run("duplicates_fail_named", func(t *testing.T) {
		ctx, db := openDreamMigrationDB(t, dsn, 4)
		defer db.Close()
		if _, err := db.ExecContext(ctx, seed+`insert into dream_inputs(run_id,source_type,source_id) values('dr','work_brief','x'),('dr','work_brief','x');`); err != nil {
			t.Fatal(err)
		}
		goose.SetBaseFS(dbfs.Migrations)
		err := goose.UpToContext(ctx, db, "migrations", 5)
		if err == nil || !strings.Contains(err.Error(), "000005 duplicate dream_inputs") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("pinned_workflow_fk", func(t *testing.T) {
		ctx, db := openDreamMigrationDB(t, dsn, 4)
		defer db.Close()
		if _, err := db.ExecContext(ctx, seed+`insert into workflow_runs(id,workflow_id,version,enterprise_id,status,input) values('wrb','wb',1,'e','succeeded','{}');`); err != nil {
			t.Fatal(err)
		}
		goose.SetBaseFS(dbfs.Migrations)
		if err := goose.UpToContext(ctx, db, "migrations", 5); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `update dream_runs set workflow_run_id='wrb' where id='dr'`); err == nil {
			t.Fatal("mismatched pinned workflow run accepted")
		}
	})
}

func TestDreamWorkflowLifecycleOutbox(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN")
	}
	ctx, conn, queries := openDreamMigrationQueries(t, dsn, 5)
	seed := `insert into enterprises(id,name) values('outbox-ent','Outbox');
insert into workflows(id,enterprise_id,name,kind,draft,created_by) values('outbox-wf','outbox-ent','Dream','dream','{}','x');
insert into workflow_versions(workflow_id,version,definition,risk_level,published_by) values('outbox-wf',1,'{"workflow_id":"outbox-wf","version":1,"kind":"dream","nodes":[{"id":"confirm","type":"human.confirm"}],"edges":[],"risk_level":"low"}','low','x');
insert into dream_policies(id,enterprise_id,org_scope,status,draft) values('outbox-policy','outbox-ent','department:outbox','published','{}');
insert into dream_policy_versions(policy_id,version,definition) values('outbox-policy',1,'{}');
insert into dream_runs(id,policy_id,version,enterprise_id,status,window_start,window_end,org_unit_id,policy_version,workflow_id,workflow_version,timezone,input_snapshot,visibility_snapshot,model_route,model_version,attempt,coverage,missing_inputs,idempotency_key)
values('outbox-dream','outbox-policy',1,'outbox-ent','running',now()-interval '1 day',now(),'department:outbox',1,'outbox-wf',1,'UTC','{"source_counts":[],"sanitized_input_ids":[]}','{"visibility_level":"members","org_unit_ids":["department:outbox"],"masked_field_count":0}','workflow/outbox-wf','v1',1,'{"expected_children":0,"completed_children":0,"input_count":0}','[]','outbox-dream');`
	if _, err := conn.Exec(ctx, seed); err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"next_index":0,"outputs":{},"input":{},"dream":{"enterprise_id":"outbox-ent","dream_run_id":"outbox-dream","policy_id":"outbox-policy","policy_version":1,"workflow_id":"outbox-wf","workflow_version":1,"org_unit_id":"department:outbox"}}`)
	created, err := queries.CreateBoundDreamWorkflowRun(ctx, db.CreateBoundDreamWorkflowRunParams{
		ID: "outbox-run", WorkflowID: pgtype.Text{String: "outbox-wf", Valid: true}, Version: pgtype.Int4{Int32: 1, Valid: true}, EnterpriseID: "outbox-ent",
		Status: "running", Input: []byte(`{}`), Output: state, DreamRunID: "outbox-dream",
		PolicyID: "outbox-policy", PolicyVersion: 1, OrgUnitID: "department:outbox",
	})
	if err != nil || created.ID != "outbox-run" {
		t.Fatalf("atomic create/bind: run=%+v err=%v", created, err)
	}
	if _, err := queries.CreateBoundDreamWorkflowRun(ctx, db.CreateBoundDreamWorkflowRunParams{
		ID: "forged-run", WorkflowID: pgtype.Text{String: "outbox-wf", Valid: true}, Version: pgtype.Int4{Int32: 1, Valid: true}, EnterpriseID: "outbox-ent",
		Status: "running", Input: []byte(`{}`), Output: state, DreamRunID: "outbox-dream",
		PolicyID: "forged-policy", PolicyVersion: 1, OrgUnitID: "department:outbox",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("forged bound context created workflow run: %v", err)
	}
	if _, err := queries.TransitionDreamWorkflowRun(ctx, db.TransitionDreamWorkflowRunParams{
		ID: "outbox-run", EnterpriseID: "outbox-ent", Status: "failed", RunOutput: state,
		LifecycleError: strings.Repeat("x", 1001),
	}); err == nil {
		t.Fatal("outbox constraint failure did not roll back workflow transition")
	}
	var status string
	if err := conn.QueryRow(ctx, `select status from workflow_runs where id='outbox-run'`).Scan(&status); err != nil || status != "running" {
		t.Fatalf("failed atomic transition leaked workflow status=%s err=%v", status, err)
	}
	if _, err := queries.TransitionDreamWorkflowRun(ctx, db.TransitionDreamWorkflowRunParams{
		ID: "outbox-run", EnterpriseID: "outbox-ent", Status: "waiting_confirmation", RunOutput: state,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.TransitionDreamWorkflowRun(ctx, db.TransitionDreamWorkflowRunParams{
		ID: "outbox-run", EnterpriseID: "outbox-ent", Status: "waiting_confirmation", RunOutput: state,
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := queries.ListPendingDreamWorkflowLifecycle(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Status != "waiting_confirmation" {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := queries.RecordDreamWorkflowLifecycleFailure(ctx, db.RecordDreamWorkflowLifecycleFailureParams{ID: pending[0].ID, LastError: "injected"}); err != nil {
		t.Fatal(err)
	}
	pending, err = queries.ListPendingDreamWorkflowLifecycle(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].Attempts != 1 || pending[0].LastError != "injected" {
		t.Fatalf("retry metadata not durable: pending=%+v err=%v", pending, err)
	}
	if err := queries.CompleteDreamWorkflowLifecycle(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	pending, err = queries.ListPendingDreamWorkflowLifecycle(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed lifecycle remained pending: %+v err=%v", pending, err)
	}
}

func openDreamMigrationQueries(t *testing.T, dsn string, version int64) (context.Context, *pgx.Conn, *db.Queries) {
	t.Helper()
	ctx := context.Background()
	admin, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := newID("migration_queries")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quotedSchema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	sqldb.SetMaxOpenConns(1)
	if _, err := sqldb.ExecContext(ctx, "set search_path to "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, sqldb, "migrations", version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	_ = sqldb.Close()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close(ctx)
		_, _ = admin.Exec(ctx, "drop schema "+quotedSchema+" cascade")
		admin.Close()
	})
	return ctx, conn, db.New(conn)
}

func openDreamMigrationDB(t *testing.T, dsn string, version int64) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	admin, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := newID("migration")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quotedSchema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	sqldb.SetMaxOpenConns(1)
	if _, err := sqldb.ExecContext(ctx, "set search_path to "+quotedSchema); err != nil {
		_ = sqldb.Close()
		admin.Close()
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, sqldb, "migrations", version); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
	t.Cleanup(func() {
		_ = sqldb.Close()
		_, _ = admin.Exec(ctx, "drop schema "+quotedSchema+" cascade")
		admin.Close()
	})
	return ctx, sqldb
}

func TestOrgParentScopeKindMigrationContracts(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx, sqldb := openDreamMigrationDB(t, dsn, 3)
	if _, err := sqldb.ExecContext(ctx, `
		insert into enterprises(id,name) values('parent-migration-ent','Parent migration');
		insert into knowledge_spaces(id,enterprise_id,kind,name,org_scope) values
			('unique-parent-space','parent-migration-ent','company','Unique parent','company:unique-parent'),
			('unique-child-space','parent-migration-ent','department','Unique child','department:unique-child'),
			('collision-company-space','parent-migration-ent','company','Collision company','company:collision'),
			('collision-department-space','parent-migration-ent','department','Collision department','department:collision'),
			('ambiguous-child-space','parent-migration-ent','employee','Ambiguous child','employee:ambiguous-child'),
			('dangling-child-space','parent-migration-ent','employee','Dangling child','employee:dangling-child');
		insert into org_scope_bindings(enterprise_id,space_id,scope_kind,scope_id,parent_scope_id) values
			('parent-migration-ent','unique-parent-space','company','unique-parent',null),
			('parent-migration-ent','unique-child-space','department','unique-child','unique-parent'),
			('parent-migration-ent','collision-company-space','company','collision',null),
			('parent-migration-ent','collision-department-space','department','collision',null),
			('parent-migration-ent','ambiguous-child-space','employee','ambiguous-child','collision'),
			('parent-migration-ent','dangling-child-space','employee','dangling-child','missing-parent');
	`); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, sqldb, "migrations", 4); err != nil {
		t.Fatal(err)
	}

	var uniqueKind, uniqueID sql.NullString
	if err := sqldb.QueryRowContext(ctx, `select parent_scope_kind,parent_scope_id from org_scope_bindings where scope_id='unique-child'`).Scan(&uniqueKind, &uniqueID); err != nil {
		t.Fatal(err)
	}
	if !uniqueKind.Valid || uniqueKind.String != "company" || !uniqueID.Valid || uniqueID.String != "unique-parent" {
		t.Fatalf("unique legacy parent was not backfilled: kind=%+v id=%+v", uniqueKind, uniqueID)
	}
	for _, scopeID := range []string{"ambiguous-child", "dangling-child"} {
		var kind, id sql.NullString
		if err := sqldb.QueryRowContext(ctx, `select parent_scope_kind,parent_scope_id from org_scope_bindings where scope_id=$1`, scopeID).Scan(&kind, &id); err != nil {
			t.Fatal(err)
		}
		if kind.Valid || id.Valid {
			t.Fatalf("%s legacy edge did not fail closed: kind=%+v id=%+v", scopeID, kind, id)
		}
	}

	if _, err := sqldb.ExecContext(ctx, `insert into knowledge_spaces(id,enterprise_id,kind,name,org_scope) values
		('fk-child-space','parent-migration-ent','employee','FK child','employee:fk-child')`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.ExecContext(ctx, `insert into org_scope_bindings(
		enterprise_id,space_id,scope_kind,scope_id,parent_scope_kind,parent_scope_id
	) values('parent-migration-ent','fk-child-space','employee','fk-child','business_unit','unique-parent')`); err == nil {
		t.Fatal("composite parent FK accepted the right ID under the wrong kind")
	}
}

func TestDreamMigrationDownContracts(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}

	t.Run("000003 safe", func(t *testing.T) {
		ctx, db := openDreamMigrationDB(t, dsn, 3)
		if err := goose.DownToContext(ctx, db, "migrations", 2); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("000003 fail closed", func(t *testing.T) {
		ctx, db := openDreamMigrationDB(t, dsn, 3)
		if _, err := db.ExecContext(ctx, `
			insert into enterprises(id,name) values('down3-ent','Down 3');
			insert into knowledge_spaces(id,enterprise_id,kind,name,org_scope) values('down3-space','down3-ent','department','Down 3','department:down3');
			insert into timeline_nodes(id,enterprise_id,space_id,org_scope,node_time,source_type,summary_text)
			values('down3-node','down3-ent','down3-space','department:down3',now(),'risk_event','risk');
		`); err != nil {
			t.Fatal(err)
		}
		if err := goose.DownToContext(ctx, db, "migrations", 2); err == nil {
			t.Fatal("000003 down accepted an extended timeline source row")
		} else if !strings.Contains(err.Error(), "cannot roll back migration 000003") {
			t.Fatalf("000003 down failed for the wrong reason: %v", err)
		}
	})
	t.Run("000004 safe", func(t *testing.T) {
		ctx, db := openDreamMigrationDB(t, dsn, 4)
		if err := goose.DownToContext(ctx, db, "migrations", 3); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("000004 fail closed", func(t *testing.T) {
		ctx, db := openDreamMigrationDB(t, dsn, 4)
		if _, err := db.ExecContext(ctx, `
			insert into enterprises(id,name) values('down4-ent','Down 4');
			insert into knowledge_spaces(id,enterprise_id,kind,name,org_scope) values
				('down4-company','down4-ent','company','Company','company:collision'),
				('down4-department','down4-ent','department','Department','department:collision'),
				('down4-child','down4-ent','employee','Child','employee:child');
			insert into org_scope_bindings(enterprise_id,space_id,scope_kind,scope_id,parent_scope_kind,parent_scope_id) values
				('down4-ent','down4-company','company','collision',null,null),
				('down4-ent','down4-department','department','collision',null,null),
				('down4-ent','down4-child','employee','child','department','collision');
		`); err != nil {
			t.Fatal(err)
		}
		if err := goose.DownToContext(ctx, db, "migrations", 3); err == nil {
			t.Fatal("000004 down restored ambiguous bare parent identities")
		} else if !strings.Contains(err.Error(), "cannot roll back migration 000004") {
			t.Fatalf("000004 down failed for the wrong reason: %v", err)
		}
	})
}

func TestDreamGovernancePublishOperationConcurrency(t *testing.T) {
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
	enterpriseID, changeID := newID("concurrent_ent"), newID("concurrent_change")
	if _, err := pool.Exec(ctx, `insert into enterprises(id,name) values($1,'Concurrency')`, enterpriseID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from publish_operations where enterprise_id=$1`, enterpriseID)
		_, _ = pool.Exec(ctx, `delete from change_drafts where enterprise_id=$1`, enterpriseID)
		_, _ = pool.Exec(ctx, `delete from enterprises where id=$1`, enterpriseID)
	})
	if _, err := q.CreateChangeDraft(ctx, db.CreateChangeDraftParams{
		ID: changeID, EnterpriseID: enterpriseID, OrgUnitID: "department:concurrency",
		ResourceType: "dream_policy", ResourceID: "policy-concurrency", Action: "publish",
		RequesterUserID: "requester", Origin: "direct_edit", PermissionMode: "direct_edit",
		Revision: 1, State: "approved", BaseVersion: 0, ProposedContent: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	txq := db.New(tx)
	first, err := txq.GetOrCreatePublishOperation(ctx, db.GetOrCreatePublishOperationParams{
		ID: newID("publish"), EnterpriseID: enterpriseID, ChangeID: changeID,
		ChangeRevision: 1, IdempotencyKey: "same-key", Status: "pending",
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	type result struct {
		row db.PublishOperation
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		row, err := q.GetOrCreatePublishOperation(ctx, db.GetOrCreatePublishOperationParams{
			ID: newID("publish"), EnterpriseID: enterpriseID, ChangeID: changeID,
			ChangeRevision: 1, IdempotencyKey: "same-key", Status: "pending",
		})
		resultCh <- result{row: row, err: err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `select exists(
			select 1 from pg_stat_activity
			where pid <> pg_backend_pid()
			  and wait_event_type = 'Lock'
			  and query like '%publish_operations%'
		)`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		_ = tx.Rollback(ctx)
		t.Fatal("concurrent publish operation did not reach conflict wait")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	second := <-resultCh
	if second.err != nil || second.row.ID != first.ID {
		t.Fatalf("concurrent get-or-create: first=%+v second=%+v err=%v", first, second.row, second.err)
	}
}

func TestDreamGovernanceSchema(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbPool, err := storage.NewPool(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer dbPool.Close()
	pool, err := dbPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = pool.Rollback(ctx) }()
	assertRejected := func(label, statement string) {
		t.Helper()
		if _, err := pool.Exec(ctx, "savepoint expected_rejection"); err != nil {
			t.Fatalf("%s savepoint: %v", label, err)
		}
		_, execErr := pool.Exec(ctx, statement)
		if _, err := pool.Exec(ctx, "rollback to savepoint expected_rejection"); err != nil {
			t.Fatalf("%s rollback: %v (original error: %v)", label, err, execErr)
		}
		if _, err := pool.Exec(ctx, "release savepoint expected_rejection"); err != nil {
			t.Fatalf("%s release: %v", label, err)
		}
		if execErr == nil {
			t.Error(label)
		}
	}

	for _, statement := range []string{
		`insert into enterprises(id,name) values('ent-dream-a','Dream A'),('ent-dream-b','Dream B') on conflict (id) do nothing`,
		`insert into knowledge_spaces(id,enterprise_id,kind,name,org_scope) values
			('space-parent','ent-dream-a','department','Parent','department:parent'),
			('space-child','ent-dream-a','project_group','Child','project_group:child'),
			('space-foreign','ent-dream-b','project_group','Foreign','project_group:foreign')
			on conflict (id) do nothing`,
		`insert into org_scope_bindings(enterprise_id,space_id,scope_kind,scope_id,parent_scope_kind,parent_scope_id) values
			('ent-dream-a','space-parent','department','parent',null,null),
			('ent-dream-a','space-child','project_group','child','department','parent'),
			('ent-dream-b','space-foreign','project_group','foreign',null,null)
			on conflict (enterprise_id,scope_kind,scope_id) do nothing`,
		`insert into workflows(id,enterprise_id,name,kind,draft) values
			('dream-workflow','ent-dream-a','Dream workflow','dream','{}'),
			('foreign-workflow','ent-dream-b','Foreign workflow','dream','{}')
			on conflict (id) do nothing`,
		`insert into workflow_versions(workflow_id,version,definition,risk_level) values
			('dream-workflow',1,'{}','low'),('foreign-workflow',1,'{}','low')
			on conflict (workflow_id,version) do nothing`,
		`insert into dream_policies(id,enterprise_id,org_scope,status,draft) values
			('policy-parent','ent-dream-a','department:parent','published','{}'),
			('policy-child','ent-dream-a','project_group:child','published','{}'),
			('policy-foreign','ent-dream-b','project_group:foreign','published','{}')
			on conflict (id) do nothing`,
		`insert into dream_policy_versions(policy_id,version,definition) values
			('policy-parent',1,'{}'),('policy-child',1,'{}'),('policy-foreign',1,'{}')
			on conflict (policy_id,version) do nothing`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}

	windowStart := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	windowEnd := windowStart.Add(24 * time.Hour)
	for _, run := range []struct {
		id, policyID, enterpriseID, orgUnitID, workflowID, idempotencyKey string
	}{
		{"parent", "policy-parent", "ent-dream-a", "parent", "dream-workflow", "dream-parent-window"},
		{"child", "policy-child", "ent-dream-a", "child", "dream-workflow", "dream-child-window"},
		{"foreign", "policy-foreign", "ent-dream-b", "foreign", "foreign-workflow", "dream-foreign-window"},
	} {
		_, err := pool.Exec(ctx, `insert into dream_runs(
			id,policy_id,version,enterprise_id,status,window_start,window_end,
			org_unit_id,policy_version,workflow_id,workflow_version,timezone,
			input_snapshot,visibility_snapshot,model_route,model_version,attempt,
			coverage,missing_inputs,idempotency_key
		) values($1,$2,1,$3,'succeeded',$4,$5,$6,1,$7,1,'UTC',
			'{"source_counts":[],"sanitized_input_ids":[]}',
			'{"visibility_level":"managers","org_unit_ids":[],"masked_field_count":0}',
			'reasoning','v1',1,
			'{"expected_children":0,"completed_children":0,"input_count":0}','[]',$8)
		on conflict (id) do nothing`, run.id, run.policyID, run.enterpriseID, windowStart, windowEnd,
			run.orgUnitID, run.workflowID, run.idempotencyKey)
		if err != nil {
			t.Fatalf("dream run %s: %v", run.id, err)
		}
	}

	_, err = pool.Exec(ctx, `insert into dream_summaries(
		id,run_id,enterprise_id,space_id,layer,summary_text,facts,themes,trends,todos,risk_signals
	) values('child-summary','child','ent-dream-a','space-child','display','sanitized child summary','[]','[]','[]','[]','[]')
	on conflict (id) do nothing`)
	if err != nil {
		t.Fatalf("structured child summary: %v", err)
	}
	assertRejected("cross-enterprise Dream summary accepted", `insert into dream_summaries(
		id,run_id,enterprise_id,space_id,layer,summary_text,risk_signals
	) values('summary-cross','child','ent-dream-b','space-foreign','display','injected','[]')`)
	assertRejected("cross-enterprise org binding accepted", `insert into org_scope_bindings(
		enterprise_id,space_id,scope_kind,scope_id,parent_scope_kind,parent_scope_id
	) values('ent-dream-a','space-foreign','project_group','malformed-child','department','parent')`)

	_, err = pool.Exec(ctx, `insert into dream_run_lineage(run_id,parent_run_id,relation) values('parent','child','child_summary')`)
	if err != nil {
		t.Fatal(err)
	}
	assertRejected("immutable run accepted update", `update dream_runs set policy_version=2 where id='parent'`)
	assertRejected("immutable published Dream policy version accepted update", `update dream_policy_versions set definition='{"changed":true}' where policy_id='policy-parent' and version=1`)
	if _, err = pool.Exec(ctx, `update dream_runs set status='running', error='', finished_at=null where id='parent'`); err != nil {
		t.Fatalf("mutable run lifecycle fields rejected: %v", err)
	}
	assertRejected("cross-enterprise Dream lineage accepted", `insert into dream_run_lineage(run_id,parent_run_id,relation) values('parent','foreign','child_summary')`)
	assertRejected("Dream lineage accepted update", `update dream_run_lineage set relation='child_summary' where run_id='parent' and parent_run_id='child'`)
	assertRejected("Dream lineage accepted delete", `delete from dream_run_lineage where run_id='parent' and parent_run_id='child'`)
	assertRejected("published workflow version accepted update", `update workflow_versions set definition='{"changed":true}' where workflow_id='dream-workflow' and version=1`)

	queries := db.New(pool)
	children, err := queries.ListChildSpaces(ctx, db.ListChildSpacesParams{
		EnterpriseID: "ent-dream-a", ParentScopeKind: "department", ParentScopeID: "parent",
	})
	if err != nil || len(children) != 1 || children[0].ID != "space-child" {
		t.Fatalf("canonical parent org query: children=%+v err=%v", children, err)
	}
	completed, err := queries.ListCompletedChildDreamRuns(ctx, db.ListCompletedChildDreamRunsParams{
		EnterpriseID: "ent-dream-a", ParentScopeKind: "department", ParentScopeID: "parent",
		WindowStart: ts(windowStart), WindowEnd: ts(windowEnd),
	})
	if err != nil || len(completed) != 1 || completed[0].ID != "child" {
		t.Fatalf("completed sanitized child Dream runs: runs=%+v err=%v", completed, err)
	}
	if _, err := queries.GetDreamRunView(ctx, db.GetDreamRunViewParams{
		EnterpriseID: "ent-dream-b", RunID: "parent",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-enterprise Dream run view returned err=%v", err)
	}
	view, err := queries.GetDreamRunView(ctx, db.GetDreamRunViewParams{
		EnterpriseID: "ent-dream-a", RunID: "parent",
	})
	if err != nil {
		t.Fatalf("Dream run view: %v", err)
	}
	var parentRunIDs []string = view.ParentRunIds
	if len(parentRunIDs) != 1 || parentRunIDs[0] != "child" {
		t.Fatalf("typed parent run IDs: %+v", parentRunIDs)
	}
	fullRun, err := queries.CreateDreamRun(ctx, db.CreateDreamRunParams{
		ID: "created-full", PolicyID: "policy-child", Version: 1,
		EnterpriseID: "ent-dream-a", Status: "pending",
		WindowStart: ts(windowStart), WindowEnd: ts(windowEnd),
		OrgUnitID: "child", PolicyVersion: 1,
		WorkflowID: "dream-workflow", WorkflowVersion: 1, Timezone: "UTC",
		InputSnapshot:      []byte(`{"source_counts":[],"sanitized_input_ids":[]}`),
		VisibilitySnapshot: []byte(`{"visibility_level":"managers","org_unit_ids":["child"],"masked_field_count":0}`),
		ModelRoute:         "reasoning", ModelVersion: "v1", Attempt: 1,
		RerunOfRunID:  pgtype.Text{},
		Coverage:      []byte(`{"expected_children":0,"completed_children":0,"input_count":0}`),
		MissingInputs: []byte(`[]`), IdempotencyKey: "full-create-key",
	})
	if err != nil || fullRun.InputSnapshot == nil || fullRun.WorkflowID.String != "dream-workflow" {
		t.Fatalf("full Dream run create: run=%+v err=%v", fullRun, err)
	}
	orgRuns, err := queries.ListDreamRunsByOrg(ctx, db.ListDreamRunsByOrgParams{
		EnterpriseID: "ent-dream-a", OrgUnitID: "child", ResultLimit: 10,
	})
	if err != nil || len(orgRuns) != 2 {
		t.Fatalf("list Dream runs by org: runs=%+v err=%v", orgRuns, err)
	}
	for _, run := range orgRuns {
		if run.EnterpriseID != "ent-dream-a" || run.OrgUnitID != "child" {
			t.Fatalf("list Dream runs by org leaked scope: %+v", run)
		}
	}
	foreignOrgRuns, err := queries.ListDreamRunsByOrg(ctx, db.ListDreamRunsByOrgParams{
		EnterpriseID: "ent-dream-b", OrgUnitID: "child", ResultLimit: 10,
	})
	if err != nil || len(foreignOrgRuns) != 0 {
		t.Fatalf("cross-enterprise org run query: runs=%+v err=%v", foreignOrgRuns, err)
	}

	_, err = pool.Exec(ctx, `insert into dream_run_annotations(
		id,enterprise_id,run_id,annotation_type,body,created_by
	) values('annotation-1','ent-dream-a','parent','review_note','sanitized note','reviewer-1')
	on conflict (id) do nothing`)
	if err != nil {
		t.Fatalf("Dream annotation: %v", err)
	}
	assertRejected("cross-enterprise Dream annotation accepted", `insert into dream_run_annotations(
		id,enterprise_id,run_id,annotation_type,body,created_by
	) values('annotation-cross','ent-dream-b','parent','review_note','bad scope','reviewer-1')`)

	_, err = pool.Exec(ctx, `insert into change_drafts(
		id,enterprise_id,org_unit_id,resource_type,resource_id,action,requester_user_id,
		origin,permission_mode,revision,state,base_version,proposed_content
	) values('change-1','ent-dream-a','parent','dream_policy','policy-parent','update','requester-1',
		'direct_edit','direct_edit',1,'draft',1,'{"schedule":"0 1 * * *"}')
	on conflict (id) do nothing`)
	if err != nil {
		t.Fatalf("change draft: %v", err)
	}
	command, err := pool.Exec(ctx, `update change_drafts
		set proposed_content='{"schedule":"0 2 * * *"}', revision=revision+1, updated_at=now()
		where id='change-1' and enterprise_id='ent-dream-a' and revision=1`)
	if err != nil || command.RowsAffected() != 1 {
		t.Fatalf("revision-guarded draft update: rows=%d err=%v", command.RowsAffected(), err)
	}
	command, err = pool.Exec(ctx, `update change_drafts
		set proposed_content='{}', revision=revision+1, updated_at=now()
		where id='change-1' and enterprise_id='ent-dream-a' and revision=1`)
	if err != nil || command.RowsAffected() != 0 {
		t.Fatalf("stale draft revision accepted: rows=%d err=%v", command.RowsAffected(), err)
	}

	_, err = pool.Exec(ctx, `insert into change_versions(
		id,enterprise_id,change_id,version,content,published_by
	) values('change-version-1','ent-dream-a','change-1',1,'{"schedule":"0 2 * * *"}','publisher-1')`)
	if err != nil {
		t.Fatalf("change version: %v", err)
	}
	assertRejected("immutable published change version accepted update", `update change_versions set content='{}' where id='change-version-1'`)
	assertRejected("cross-enterprise change version accepted", `insert into change_versions(
		id,enterprise_id,change_id,version,content,published_by
	) values('change-version-cross','ent-dream-b','change-1',2,'{}','publisher-1')`)

	_, err = pool.Exec(ctx, `insert into change_reviews(
		id,enterprise_id,change_id,change_revision,reviewer_user_id,risk_level,risk_reasons,
		review_mode,state,org_path,decision
	) values('review-1','ent-dream-a','change-1',2,'reviewer-1','high','["policy change"]',
		'upward_review','approved','["parent"]','approved')`)
	if err != nil {
		t.Fatalf("change review: %v", err)
	}
	_, err = pool.Exec(ctx, `insert into change_reviews(
		id,enterprise_id,change_id,change_revision,risk_level,risk_reasons,
		review_mode,state,org_path,queue,decision
	) values('review-admin','ent-dream-a','change-1',2,'high','["enterprise scope"]',
		'enterprise_knowledge_admin_queue','pending','[]','knowledge-admins','')`)
	if err != nil {
		t.Fatalf("enterprise knowledge admin queue review: %v", err)
	}
	assertRejected("invalid low-risk upward review accepted", `insert into change_reviews(
		id,enterprise_id,change_id,change_revision,reviewer_user_id,risk_level,risk_reasons,
		review_mode,state,org_path,decision
	) values('review-invalid','ent-dream-a','change-1',2,'reviewer-1','low','[]',
		'upward_review','pending','["parent"]','')`)
	assertRejected("stale change revision review accepted", `insert into change_reviews(
		id,enterprise_id,change_id,change_revision,reviewer_user_id,risk_level,risk_reasons,
		review_mode,state,org_path,decision
	) values('review-stale','ent-dream-a','change-1',1,'reviewer-1','high','[]',
		'upward_review','pending','["parent"]','')`)
	assertRejected("requester accepted as upward reviewer", `insert into change_reviews(
		id,enterprise_id,change_id,change_revision,reviewer_user_id,risk_level,risk_reasons,
		review_mode,state,org_path,decision
	) values('review-self','ent-dream-a','change-1',2,'requester-1','high','[]',
		'upward_review','pending','["parent"]','')`)

	_, err = pool.Exec(ctx, `insert into publish_operations(
		id,enterprise_id,change_id,change_revision,idempotency_key,status
	) values('publish-1','ent-dream-a','change-1',2,'publish-key-1','pending')`)
	if err != nil {
		t.Fatalf("publish operation: %v", err)
	}
	assertRejected("duplicate enterprise publish idempotency key accepted", `insert into publish_operations(
		id,enterprise_id,change_id,change_revision,idempotency_key,status
	) values('publish-duplicate','ent-dream-a','change-1',2,'publish-key-1','pending')`)
	assertRejected("cross-enterprise publish operation accepted", `insert into publish_operations(
		id,enterprise_id,change_id,change_revision,idempotency_key,status
	) values('publish-cross','ent-dream-b','change-1',2,'publish-key-2','pending')`)
	assertRejected("stale change revision publish accepted", `insert into publish_operations(
		id,enterprise_id,change_id,change_revision,idempotency_key,status
	) values('publish-stale','ent-dream-a','change-1',1,'publish-key-stale','pending')`)

	_, err = queries.CreateChangeDraft(ctx, db.CreateChangeDraftParams{
		ID: "change-lifecycle", EnterpriseID: "ent-dream-a", OrgUnitID: "parent",
		ResourceType: "dream_policy", ResourceID: "policy-parent", Action: "publish",
		RequesterUserID: "requester-1", Origin: "direct_edit", PermissionMode: "direct_edit",
		Revision: 1, State: "approved", BaseVersion: 0, ProposedContent: []byte(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("lifecycle change draft: %v", err)
	}
	firstOperation, err := queries.GetOrCreatePublishOperation(ctx, db.GetOrCreatePublishOperationParams{
		ID: "publish-lifecycle", EnterpriseID: "ent-dream-a", ChangeID: "change-lifecycle",
		ChangeRevision: 1, IdempotencyKey: "publish-lifecycle-key", Status: "pending",
	})
	if err != nil {
		t.Fatalf("initial lifecycle publish operation: %v", err)
	}
	if _, err := queries.UpdateChangeDraftIfRevision(ctx, db.UpdateChangeDraftIfRevisionParams{
		ID: "change-lifecycle", EnterpriseID: "ent-dream-a", ExpectedRevision: 1,
		State: "approved", ProposedContent: []byte(`{"version":2}`),
	}); err != nil {
		t.Fatalf("advance lifecycle draft: %v", err)
	}
	if _, err := pool.Exec(ctx, "savepoint stable_retry"); err != nil {
		t.Fatal(err)
	}
	retriedOperation, retryErr := queries.GetOrCreatePublishOperation(ctx, db.GetOrCreatePublishOperationParams{
		ID: "publish-lifecycle-retry", EnterpriseID: "ent-dream-a", ChangeID: "change-lifecycle",
		ChangeRevision: 1, IdempotencyKey: "publish-lifecycle-key", Status: "pending",
	})
	if _, err := pool.Exec(ctx, "rollback to savepoint stable_retry"); err != nil {
		t.Fatalf("stable retry rollback: %v (original error: %v)", err, retryErr)
	}
	if _, err := pool.Exec(ctx, "release savepoint stable_retry"); err != nil {
		t.Fatal(err)
	}
	if retryErr != nil || retriedOperation.ID != firstOperation.ID {
		t.Errorf("stable retry after draft advance: first=%+v retry=%+v err=%v", firstOperation, retriedOperation, retryErr)
	}
	if _, err := pool.Exec(ctx, "savepoint lifecycle_update"); err != nil {
		t.Fatal(err)
	}
	command, lifecycleErr := pool.Exec(ctx, `update publish_operations
		set status='succeeded', result='{"published_version":1}', finished_at=now()
		where id='publish-lifecycle' and enterprise_id='ent-dream-a'`)
	if lifecycleErr != nil {
		if _, err := pool.Exec(ctx, "rollback to savepoint lifecycle_update"); err != nil {
			t.Fatalf("lifecycle update rollback: %v (original error: %v)", err, lifecycleErr)
		}
	}
	if _, err := pool.Exec(ctx, "release savepoint lifecycle_update"); err != nil {
		t.Fatal(err)
	}
	if lifecycleErr != nil || command.RowsAffected() != 1 {
		t.Errorf("publish lifecycle update: rows=%d err=%v", command.RowsAffected(), lifecycleErr)
	} else {
		var status string
		var result []byte
		var finished bool
		if err := pool.QueryRow(ctx, `select status,result,finished_at is not null
			from publish_operations where id='publish-lifecycle' and enterprise_id='ent-dream-a'`).Scan(
			&status, &result, &finished,
		); err != nil {
			t.Fatal(err)
		}
		if status != "succeeded" || string(result) != `{"published_version": 1}` || !finished {
			t.Errorf("publish lifecycle fields not persisted: status=%s result=%s finished=%v", status, result, finished)
		}
	}
	assertRejected("publish operation identity rewrite accepted", `update publish_operations
		set change_revision=2 where id='publish-lifecycle' and enterprise_id='ent-dream-a'`)
	assertRejected("new-key stale publish revision accepted", `insert into publish_operations(
		id,enterprise_id,change_id,change_revision,idempotency_key,status
	) values('publish-lifecycle-stale','ent-dream-a','change-lifecycle',1,'publish-lifecycle-new-key','pending')`)
	if _, err := pool.Exec(ctx, "savepoint payload_mismatch"); err != nil {
		t.Fatal(err)
	}
	_, mismatchErr := queries.GetOrCreatePublishOperation(ctx, db.GetOrCreatePublishOperationParams{
		ID: "publish-lifecycle-mismatch", EnterpriseID: "ent-dream-a", ChangeID: "change-lifecycle",
		ChangeRevision: 2, IdempotencyKey: "publish-lifecycle-key", Status: "pending",
	})
	if _, err := pool.Exec(ctx, "rollback to savepoint payload_mismatch"); err != nil {
		t.Fatalf("payload mismatch rollback: %v (original error: %v)", err, mismatchErr)
	}
	if _, err := pool.Exec(ctx, "release savepoint payload_mismatch"); err != nil {
		t.Fatal(err)
	}
	if mismatchErr == nil {
		t.Error("same-key payload mismatch accepted")
	}
}
