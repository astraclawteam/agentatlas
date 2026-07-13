package integration

import (
	"context"
	"errors"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/spaces"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

func TestOrgSpaceEventAtomicityPostgres(t *testing.T) {
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := db.New(pool)
	service := spaces.NewService(q)
	enterpriseID := newID("atomic-ent")
	if _, err := q.UpsertEnterprise(ctx, db.UpsertEnterpriseParams{ID: enterpriseID, Name: "Atomic org events"}); err != nil {
		t.Fatal(err)
	}

	insertParent := func(kind nexus.OrgScopeKind, id string) {
		t.Helper()
		spaceID := newID("atomic-parent")
		if _, err := q.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
			ID: spaceID, EnterpriseID: enterpriseID, Kind: string(kind), Name: id,
			OrgScope: string(kind) + ":" + id, OrgVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{
			EnterpriseID: enterpriseID, SpaceID: spaceID, ScopeKind: string(kind), ScopeID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	insertParent(nexus.ScopeCompany, "parent-v1")
	insertParent(nexus.ScopeCompany, "parent-v3")

	t.Run("initial failure and same-version retry", func(t *testing.T) {
		event := atomicOrgEvent(enterpriseID, 1, nexus.ScopeEmployee, "initial-child", nexus.ScopeDepartment, "missing-initial",
			[]nexus.OrgMember{{UserID: "initial-member", DisplayName: "initial"}})
		if _, _, err := service.EnsureSpaceFromEvent(ctx, event); err == nil {
			t.Fatal("missing initial parent did not fail")
		}
		if _, err := q.GetKnowledgeSpaceByScope(ctx, db.GetKnowledgeSpaceByScopeParams{
			EnterpriseID: enterpriseID, OrgScope: "employee:initial-child",
		}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("failed initial event left a space: %v", err)
		}
		insertParent(nexus.ScopeDepartment, "missing-initial")
		if _, _, err := service.EnsureSpaceFromEvent(ctx, event); err != nil {
			t.Fatalf("initial same-version retry: %v", err)
		}
		assertPostgresOrgState(t, ctx, pool, enterpriseID, "employee", "initial-child", 1, "department", "missing-initial", []string{"initial-member"}, 1)
	})

	t.Run("post-space failure and same-version retry", func(t *testing.T) {
		v1 := atomicOrgEvent(enterpriseID, 1, nexus.ScopeDepartment, "retry-child", nexus.ScopeCompany, "parent-v1",
			[]nexus.OrgMember{{UserID: "v1-member", DisplayName: "v1"}})
		if _, _, err := service.EnsureSpaceFromEvent(ctx, v1); err != nil {
			t.Fatal(err)
		}
		v2 := atomicOrgEvent(enterpriseID, 2, nexus.ScopeDepartment, "retry-child", nexus.ScopeBusinessUnit, "missing-v2",
			[]nexus.OrgMember{{UserID: "v2-member", DisplayName: "v2"}})
		if _, _, err := service.EnsureSpaceFromEvent(ctx, v2); err == nil {
			t.Fatal("missing v2 parent did not fail")
		}
		assertPostgresOrgState(t, ctx, pool, enterpriseID, "department", "retry-child", 1, "company", "parent-v1", []string{"v1-member"}, 1)
		insertParent(nexus.ScopeBusinessUnit, "missing-v2")
		if _, _, err := service.EnsureSpaceFromEvent(ctx, v2); err != nil {
			t.Fatalf("v2 same-version retry: %v", err)
		}
		assertPostgresOrgState(t, ctx, pool, enterpriseID, "department", "retry-child", 2, "business_unit", "missing-v2", []string{"v2-member"}, 2)
	})

	t.Run("enforces read committed over session default", func(t *testing.T) {
		operationConn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer operationConn.Release()
		if _, err := operationConn.Exec(ctx, `set default_transaction_isolation = 'repeatable read'`); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = operationConn.Exec(ctx, `reset default_transaction_isolation`) }()
		operationPID := postgresBackendPID(t, ctx, operationConn)

		blocker, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		blockerPID := postgresBackendPID(t, ctx, blocker)
		const scope = "department:isolation-child"
		if _, err := blocker.Exec(ctx, `select pg_advisory_lock(hashtextextended($1 || chr(31) || $2, 0))`, enterpriseID, scope); err != nil {
			t.Fatal(err)
		}
		lockHeld := true
		defer func() {
			if lockHeld {
				_, _ = blocker.Exec(ctx, `select pg_advisory_unlock(hashtextextended($1 || chr(31) || $2, 0))`, enterpriseID, scope)
			}
		}()

		v2 := atomicOrgEvent(enterpriseID, 2, nexus.ScopeDepartment, "isolation-child", nexus.ScopeCompany, "parent-v3",
			[]nexus.OrgMember{{UserID: "v2-member", DisplayName: "v2"}})
		errCh := make(chan error, 1)
		operationService := spaces.NewService(db.New(operationConn))
		go func() { _, _, err := operationService.EnsureSpaceFromEvent(ctx, v2); errCh <- err }()
		waitForAdvisoryLockBlockedBy(t, ctx, pool, operationPID, blockerPID)

		spaceID := newID("isolation-space")
		seedTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seed := q.WithTx(seedTx)
		if _, err := seed.InsertKnowledgeSpace(ctx, db.InsertKnowledgeSpaceParams{
			ID: spaceID, EnterpriseID: enterpriseID, Kind: "department", Name: "isolation-child",
			OrgScope: scope, OrgVersion: 1,
		}); err != nil {
			_ = seedTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := seed.UpsertOrgScopeBinding(ctx, db.UpsertOrgScopeBindingParams{
			EnterpriseID: enterpriseID, SpaceID: spaceID, ScopeKind: "department", ScopeID: "isolation-child",
			ParentScopeKind: pgText("company"), ParentScopeID: pgText("parent-v1"),
		}); err != nil {
			_ = seedTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := seed.InsertKnowledgeSpaceVersion(ctx, db.InsertKnowledgeSpaceVersionParams{
			SpaceID: spaceID, OrgVersion: 1, Snapshot: []byte(`{}`),
		}); err != nil {
			_ = seedTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := seed.UpsertSpaceMember(ctx, db.UpsertSpaceMemberParams{
			SpaceID: spaceID, UserID: "v1-member", DisplayName: "v1", OrgVersion: 1,
		}); err != nil {
			_ = seedTx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := seedTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := blocker.Exec(ctx, `select pg_advisory_unlock(hashtextextended($1 || chr(31) || $2, 0))`, enterpriseID, scope); err != nil {
			t.Fatal(err)
		}
		lockHeld = false
		if err := <-errCh; err != nil {
			t.Fatalf("operation inherited repeatable-read session default: %v", err)
		}
		assertPostgresOrgState(t, ctx, pool, enterpriseID, "department", "isolation-child", 2, "company", "parent-v3", []string{"v2-member"}, 2)
	})

	t.Run("concurrent and out-of-order v2 v3", func(t *testing.T) {
		insertParent(nexus.ScopeBusinessUnit, "parent-v2")
		v1 := atomicOrgEvent(enterpriseID, 1, nexus.ScopeDepartment, "concurrent-child", nexus.ScopeCompany, "parent-v1", nil)
		if _, _, err := service.EnsureSpaceFromEvent(ctx, v1); err != nil {
			t.Fatal(err)
		}
		blocker, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Release()
		blockerPID := postgresBackendPID(t, ctx, blocker)
		const triggerLockKey int64 = 734892137
		if _, err := blocker.Exec(ctx, `select pg_advisory_lock($1)`, triggerLockKey); err != nil {
			t.Fatal(err)
		}
		lockHeld := true
		defer func() {
			if lockHeld {
				_, _ = blocker.Exec(ctx, `select pg_advisory_unlock($1)`, triggerLockKey)
			}
		}()
		if _, err := pool.Exec(ctx, `
			create or replace function block_atomic_v2_binding() returns trigger language plpgsql as $$
			begin
				if new.scope_id = 'concurrent-child' and new.parent_scope_id = 'parent-v2' then
					perform pg_advisory_xact_lock(734892137);
				end if;
				return new;
			end $$;
			create trigger block_atomic_v2_binding_trigger
			before insert or update on org_scope_bindings
			for each row execute function block_atomic_v2_binding();
		`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `drop trigger if exists block_atomic_v2_binding_trigger on org_scope_bindings`)
			_, _ = pool.Exec(ctx, `drop function if exists block_atomic_v2_binding()`)
		})

		v2 := atomicOrgEvent(enterpriseID, 2, nexus.ScopeDepartment, "concurrent-child", nexus.ScopeBusinessUnit, "parent-v2",
			[]nexus.OrgMember{{UserID: "v2-member", DisplayName: "v2"}})
		v3 := atomicOrgEvent(enterpriseID, 3, nexus.ScopeDepartment, "concurrent-child", nexus.ScopeCompany, "parent-v3",
			[]nexus.OrgMember{{UserID: "v3-member", DisplayName: "v3"}})
		v2Conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer v2Conn.Release()
		v3Conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer v3Conn.Release()
		v2PID := postgresBackendPID(t, ctx, v2Conn)
		v3PID := postgresBackendPID(t, ctx, v3Conn)
		errCh := make(chan error, 2)
		v2Service := spaces.NewService(db.New(v2Conn))
		v3Service := spaces.NewService(db.New(v3Conn))
		go func() { _, _, err := v2Service.EnsureSpaceFromEvent(ctx, v2); errCh <- err }()
		waitForAdvisoryLockBlockedBy(t, ctx, pool, v2PID, blockerPID)
		go func() { _, _, err := v3Service.EnsureSpaceFromEvent(ctx, v3); errCh <- err }()
		waitForAdvisoryLockBlockedBy(t, ctx, pool, v3PID, v2PID)
		if _, err := blocker.Exec(ctx, `select pg_advisory_unlock($1)`, triggerLockKey); err != nil {
			t.Fatal(err)
		}
		lockHeld = false
		for i := 0; i < 2; i++ {
			if err := <-errCh; err != nil {
				t.Fatal(err)
			}
		}
		assertPostgresOrgState(t, ctx, pool, enterpriseID, "department", "concurrent-child", 3, "company", "parent-v3", []string{"v3-member"}, 3)
		if _, _, err := service.EnsureSpaceFromEvent(ctx, v2); err != nil {
			t.Fatal(err)
		}
		assertPostgresOrgState(t, ctx, pool, enterpriseID, "department", "concurrent-child", 3, "company", "parent-v3", []string{"v3-member"}, 3)
	})
}

func waitForAdvisoryLockBlockedBy(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, waiterPID, blockerPID int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `
			select exists (
				select 1
				from pg_locks waiting
				join pg_locks holding
				  on holding.locktype = waiting.locktype
				 and holding.database is not distinct from waiting.database
				 and holding.classid is not distinct from waiting.classid
				 and holding.objid is not distinct from waiting.objid
				 and holding.objsubid is not distinct from waiting.objsubid
				where waiting.locktype = 'advisory'
				  and waiting.pid = $1
				  and not waiting.granted
				  and holding.pid = $2
				  and holding.granted
			)
		`, waiterPID, blockerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("PostgreSQL backend %d did not wait on advisory lock held by backend %d", waiterPID, blockerPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func postgresBackendPID(t *testing.T, ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) int32 {
	t.Helper()
	var pid int32
	if err := conn.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

func pgText(value string) pgtype.Text { return pgtype.Text{String: value, Valid: true} }

func atomicOrgEvent(enterpriseID string, version int64, kind nexus.OrgScopeKind, id string, parentKind nexus.OrgScopeKind, parentID string, members []nexus.OrgMember) nexus.OrgEvent {
	return nexus.OrgEvent{
		EventID: "atomic-event", EnterpriseID: enterpriseID, OrgVersion: version, Type: nexus.OrgDepartmentUpserted,
		Scope:   nexus.OrgScope{Kind: kind, ID: id, Name: id, ParentKind: parentKind, ParentID: parentID},
		Members: members, OccurredAt: time.Now(),
	}
}

func assertPostgresOrgState(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, enterpriseID, scopeKind, scopeID string, version int64, parentKind, parentID string, wantMembers []string, wantVersions int) {
	t.Helper()
	var spaceID string
	var gotVersion int64
	if err := pool.QueryRow(ctx, `select id,org_version from knowledge_spaces where enterprise_id=$1 and org_scope=$2`, enterpriseID, scopeKind+":"+scopeID).Scan(&spaceID, &gotVersion); err != nil {
		t.Fatal(err)
	}
	var gotParentKind, gotParentID string
	if err := pool.QueryRow(ctx, `select parent_scope_kind,parent_scope_id from org_scope_bindings where enterprise_id=$1 and scope_kind=$2 and scope_id=$3`, enterpriseID, scopeKind, scopeID).Scan(&gotParentKind, &gotParentID); err != nil {
		t.Fatal(err)
	}
	var versionCount int
	if err := pool.QueryRow(ctx, `select count(*) from knowledge_space_versions where space_id=$1`, spaceID).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `select user_id from space_membership_cache where space_id=$1 order by user_id`, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var members []string
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
	}
	sort.Strings(wantMembers)
	if gotVersion != version || gotParentKind != parentKind || gotParentID != parentID || versionCount != wantVersions || len(members) != len(wantMembers) {
		t.Fatalf("inconsistent atomic state: version=%d parent=%s:%s versions=%d members=%v", gotVersion, gotParentKind, gotParentID, versionCount, members)
	}
	for i := range members {
		if members[i] != wantMembers[i] {
			t.Fatalf("members=%v want=%v", members, wantMembers)
		}
	}
}
