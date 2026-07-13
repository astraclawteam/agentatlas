package workcase_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	dbfs "github.com/astraclawteam/agentatlas/services/agentatlas/db"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

// testDSN gates every test in this file on ATLAS_TEST_POSTGRES_DSN, exactly
// like tests/integration/dream_input_resolver_postgres_test.go and
// internal/browsersession/postgres_rotation_integration_test.go.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	return dsn
}

// runID mirrors internal/browsersession's nanosecond-suffixed unique ID
// helper so concurrent/sequential integration tests never collide on
// enterprise or idempotency-key values within the shared scratch database.
func runID(prefix string) string {
	return prefix + "-" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
}

func newPostgresService(t *testing.T, dsn string) (*workcase.Service, *workcase.PostgresStore, string) {
	t.Helper()
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ent := runID("ent")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Workcase Integration')`, ent); err != nil {
		t.Fatalf("seed enterprise: %v", err)
	}
	store, err := workcase.NewPostgresStore(pool, nil)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	svc, err := workcase.NewService(store, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, store, ent
}

// TestPostgresWorkcaseMigrationUpDownUp exercises migration 000014 up, down
// (to the immediately prior version) and up again, following the mechanism
// in internal/browsersession/migration_down_integration_test.go: goose
// direct against database/sql, asserting both tables are actually gone
// after Down and actually usable again after the second Up.
func TestPostgresWorkcaseMigrationUpDownUp(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ent := runID("ent-migration")
	if _, err := db.ExecContext(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Migration')`, ent); err != nil {
		t.Fatal(err)
	}
	caseID := runID("case-migration")
	if _, err := db.ExecContext(ctx, `INSERT INTO workcases(id,enterprise_id,org_scope,actor_ref,status,revision,plans) VALUES($1,$2,'org:team','actor-1','draft',1,'[]'::jsonb)`, caseID, ent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workcase_events(case_id,seq,enterprise_id,event_type,payload,idempotency_key) VALUES($1,1,$2,'case_created','{}'::jsonb,$3)`, caseID, ent, runID("idem-migration-seed")); err != nil {
		t.Fatal(err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.DownToContext(ctx, db, "migrations", 13); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	var tableCount int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('workcases','workcase_events')`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatalf("down migration left %d workcase table(s) behind", tableCount)
	}

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('workcases','workcase_events')`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 2 {
		t.Fatalf("up migration did not recreate both workcase tables: found %d", tableCount)
	}
	// Prove the recreated schema is actually usable, and that Down truly
	// dropped (not merely emptied) the prior data.
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM workcases WHERE id=$1`, caseID).Scan(&count); err != nil {
		t.Fatalf("recreated workcases table not queryable: %v", err)
	}
	if count != 0 {
		t.Fatalf("recreated table unexpectedly retained pre-down data: %d", count)
	}
}

// TestPostgresWorkcaseLifecycleRestartAndEventReplay drives the full
// Create -> ProposePlan -> StartReview -> StartExecution -> TransitionStep
// lifecycle, simulates a process restart (closes and reopens the pool), and
// rebuilds the aggregate purely from the append-only workcase_events log to
// compare against the live snapshot.
func TestPostgresWorkcaseLifecycleRestartAndEventReplay(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	svc, _, ent := newPostgresService(t, dsn)

	c, err := svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: runID("idem-lifecycle-create"),
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	afterPlan, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: runID("idem-lifecycle-plan")},
		Plan:    validPlan(),
	})
	if err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}
	afterReview, err := svc.StartReview(ctx, workcase.StartReviewCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterPlan.Revision, IdempotencyKey: runID("idem-lifecycle-review"),
	}})
	if err != nil {
		t.Fatalf("StartReview: %v", err)
	}
	afterExec, err := svc.StartExecution(ctx, workcase.StartExecutionCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterReview.Revision, IdempotencyKey: runID("idem-lifecycle-exec"),
	}})
	if err != nil {
		t.Fatalf("StartExecution: %v", err)
	}
	final, err := svc.TransitionStep(ctx, workcase.TransitionStepCommand{
		Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: afterExec.Revision, IdempotencyKey: runID("idem-lifecycle-run")},
		StepID:  "step-1", Status: sdkworkcase.StepRunning,
	})
	if err != nil {
		t.Fatalf("TransitionStep running: %v", err)
	}

	// Simulate a process restart: open a brand new pool/store against the
	// same database instead of reusing any in-process state.
	restarted, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("reopen pool after restart: %v", err)
	}
	defer restarted.Close()
	restartedStore, err := workcase.NewPostgresStore(restarted, nil)
	if err != nil {
		t.Fatal(err)
	}
	fromFreshConn, err := restartedStore.Get(ctx, ent, "org:team-a", c.ID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if fromFreshConn.Revision != final.Revision || fromFreshConn.Status != final.Status {
		t.Fatalf("snapshot not durable across restart: got %+v want %+v", fromFreshConn, final)
	}

	// Rebuild state purely from the append-only event log and compare to
	// the live snapshot. Each event's payload is the full resulting
	// WorkCase, so folding is "take the last row in seq order" -- but we
	// also assert the log itself is gapless and internally consistent
	// (event N's own payload reports revision N), which is what actually
	// proves the log is a faithful, replayable history and not just an
	// incidental duplicate of the snapshot.
	rows, err := restarted.Query(ctx, `SELECT seq,payload FROM workcase_events WHERE case_id=$1 ORDER BY seq`, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var rebuilt sdkworkcase.WorkCase
	var seqs []uint64
	for rows.Next() {
		var seq uint64
		var payload []byte
		if err := rows.Scan(&seq, &payload); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(payload, &rebuilt); err != nil {
			t.Fatalf("decode event seq=%d payload: %v", seq, err)
		}
		if rebuilt.Revision != seq {
			t.Fatalf("event seq=%d recorded a mismatched revision=%d", seq, rebuilt.Revision)
		}
		seqs = append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if uint64(len(seqs)) != final.Revision {
		t.Fatalf("event log has %d events, want %d (one per revision, gapless from 1)", len(seqs), final.Revision)
	}
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("event log seq gap or reorder: %v", seqs)
		}
	}
	if rebuilt.Status != fromFreshConn.Status || rebuilt.Revision != fromFreshConn.Revision || len(rebuilt.Plans) != len(fromFreshConn.Plans) {
		t.Fatalf("event replay diverged from live snapshot: replay=%+v snapshot=%+v", rebuilt, fromFreshConn)
	}
}

// TestPostgresConcurrentWritersOneWinsOneGetsStaleRevision has two
// goroutines race a ProposePlan command against the same ExpectedRevision:
// exactly one must win, the other must get the typed stale-revision error,
// and the aggregate must end up mutated exactly once (never corrupted,
// never double-applied).
func TestPostgresConcurrentWritersOneWinsOneGetsStaleRevision(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	svc, _, ent := newPostgresService(t, dsn)

	c, err := svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: runID("idem-concurrent-create"),
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 2)
	cases := make([]sdkworkcase.WorkCase, 2)
	keys := []string{runID("idem-concurrent-a"), runID("idem-concurrent-b")}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plan := validPlan()
			plan.Steps[0].ID = fmt.Sprintf("step-writer-%d", i)
			cases[i], results[i] = svc.ProposePlan(ctx, workcase.ProposePlanCommand{
				Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: keys[i]},
				Plan:    plan,
			})
		}(i)
	}
	wg.Wait()

	successes, staleFailures := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, workcase.ErrStaleRevision):
			staleFailures++
		default:
			t.Fatalf("unexpected error from concurrent writer: %v", err)
		}
	}
	if successes != 1 || staleFailures != 1 {
		t.Fatalf("want exactly one winner and one stale-revision loser, got successes=%d staleFailures=%d (results=%v, cases=%+v)", successes, staleFailures, results, cases)
	}
	final, err := svc.Get(ctx, ent, "org:team-a", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Revision != c.Revision+1 || len(final.Plans) != 1 {
		t.Fatalf("aggregate corrupted by concurrent writers: %+v", final)
	}
}

// TestPostgresTransactionalEventAppendRollsBackSnapshotOnEventInsertFailure
// forces a real mid-command failure with no test-only hooks: it pre-seeds
// the exact (case_id, seq) row that the next legitimate command's event
// insert will target. PostgresStore.Apply runs the workcases snapshot
// UPDATE first and the workcase_events INSERT second inside ONE
// transaction, so the pre-seeded primary-key collision fails the INSERT
// after the UPDATE already ran -- proving the two writes are genuinely
// atomic only if, after the command reports failure, the snapshot is
// unchanged (proving the UPDATE was rolled back, not partially committed).
func TestPostgresTransactionalEventAppendRollsBackSnapshotOnEventInsertFailure(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	svc, _, ent := newPostgresService(t, dsn)
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	c, err := svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: runID("idem-txn-create"),
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	nextSeq := c.Revision + 1
	poisonKey := runID("idem-txn-poison")
	if _, err := pool.Exec(ctx, `INSERT INTO workcase_events(case_id,seq,enterprise_id,event_type,payload,idempotency_key) VALUES($1,$2,$3,'plan_proposed','{}'::jsonb,$4)`,
		c.ID, nextSeq, ent, poisonKey); err != nil {
		t.Fatalf("seed poison event at the next seq: %v", err)
	}

	realKey := runID("idem-txn-real")
	_, err = svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: realKey},
		Plan:    validPlan(),
	})
	if err == nil {
		t.Fatal("expected the poisoned (case_id,seq) collision to fail the command")
	}
	if !errors.Is(err, workcase.ErrEventLogConflict) {
		t.Fatalf("a (case_id,seq) collision means snapshot/event divergence and must surface as the distinct ErrEventLogConflict sentinel, got %v", err)
	}

	after, getErr := svc.Get(ctx, ent, "org:team-a", c.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.Revision != c.Revision || len(after.Plans) != 0 {
		t.Fatalf("snapshot UPDATE was not rolled back together with the failed event INSERT: %+v", after)
	}
	// Query by the REAL command's own idempotency key, not by event_type:
	// the pre-seeded poison row is itself a (harmless, expected) real
	// plan_proposed row, so counting by event_type alone would count our
	// own fixture. What must be absent is specifically the event the
	// failed command tried to append.
	var realEventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workcase_events WHERE case_id=$1 AND idempotency_key=$2`, c.ID, realKey).Scan(&realEventCount); err != nil {
		t.Fatal(err)
	}
	if realEventCount != 0 {
		t.Fatalf("the failed command's event was persisted despite the transaction failing: %d", realEventCount)
	}
	// And exactly one row remains at (case_id, nextSeq): our poison
	// fixture, proving the real INSERT never landed a second row there
	// either (no silent seq renumbering / duplicate insert path).
	var seqCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workcase_events WHERE case_id=$1 AND seq=$2`, c.ID, nextSeq).Scan(&seqCount); err != nil {
		t.Fatal(err)
	}
	if seqCount != 1 {
		t.Fatalf("want exactly 1 row (the poison fixture) at seq=%d, got %d", nextSeq, seqCount)
	}
}

func TestPostgresCrossTenantAccessLooksLikeNotFound(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	svc, _, entA := newPostgresService(t, dsn)
	entB := runID("ent-tenant-b")
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Tenant B')`, entB); err != nil {
		t.Fatal(err)
	}

	c, err := svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: entA, OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: runID("idem-tenant-create"),
	}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Get(ctx, entB, "org:team-a", c.ID); !errors.Is(err, workcase.ErrNotFound) {
		t.Fatalf("wrong enterprise Get: got %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(ctx, entA, "org:team-b", c.ID); !errors.Is(err, workcase.ErrNotFound) {
		t.Fatalf("wrong org scope Get: got %v, want ErrNotFound", err)
	}
	_, err = svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: entB, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: runID("idem-tenant-plan")},
		Plan:    validPlan(),
	})
	if !errors.Is(err, workcase.ErrNotFound) {
		t.Fatalf("cross-tenant ProposePlan: got %v, want ErrNotFound", err)
	}
}

func TestPostgresDuplicateIdempotencyKeyReplaysAcrossConnections(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	svc, _, ent := newPostgresService(t, dsn)
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	idem := runID("idem-replay-create")
	cmd := workcase.CreateCommand{Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: idem}}
	first, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := svc.Create(ctx, cmd)
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if second.ID != first.ID || second.Revision != first.Revision {
		t.Fatalf("replay diverged: first=%+v second=%+v", first, second)
	}
	var caseCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workcases WHERE enterprise_id=$1`, ent).Scan(&caseCount); err != nil {
		t.Fatal(err)
	}
	if caseCount != 1 {
		t.Fatalf("replay created a duplicate case row: %d", caseCount)
	}

	other, err := svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: runID("idem-replay-other")}})
	if err != nil {
		t.Fatalf("other Create: %v", err)
	}
	_, err = svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: other.ID, ExpectedRevision: other.Revision, IdempotencyKey: idem},
		Plan:    validPlan(),
	})
	if !errors.Is(err, workcase.ErrIdempotencyKeyReused) {
		t.Fatalf("key reused on a different case: got %v, want ErrIdempotencyKeyReused", err)
	}
}

// TestPostgresConcurrentSameIdempotencyKeyDifferentCaseIDs closes the
// idempotency TOCTOU window: two writers race the SAME brand-new key with
// different intended CaseIDs, so both can miss the replay SELECT (nothing
// committed yet) and collide at the event INSERT's
// workcase_events_idempotency_uniq constraint instead. Exactly one must
// win; the loser must get the same typed ErrIdempotencyKeyReused a
// sequential caller gets, and the loser's case must not exist (its whole
// transaction, snapshot insert included, rolled back). Looped because the
// interesting interleaving is timing-dependent.
func TestPostgresConcurrentSameIdempotencyKeyDifferentCaseIDs(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	svc, _, ent := newPostgresService(t, dsn)

	for i := 0; i < 8; i++ {
		key := confID(fmt.Sprintf("idem-samekey-%d", i))
		ids := [2]string{confID(fmt.Sprintf("case-samekey-a%d", i)), confID(fmt.Sprintf("case-samekey-b%d", i))}
		var wg sync.WaitGroup
		var errs [2]error
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				_, errs[w] = svc.Create(ctx, workcase.CreateCommand{Command: workcase.Command{
					EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: ids[w], IdempotencyKey: key,
				}})
			}(w)
		}
		wg.Wait()

		winner := -1
		reused := 0
		for w, err := range errs {
			switch {
			case err == nil:
				winner = w
			case errors.Is(err, workcase.ErrIdempotencyKeyReused):
				reused++
			default:
				t.Fatalf("iteration %d writer %d: got %v, want nil or ErrIdempotencyKeyReused", i, w, err)
			}
		}
		if winner < 0 || reused != 1 {
			t.Fatalf("iteration %d: want exactly one winner and one ErrIdempotencyKeyReused loser, got errs=%v", i, errs)
		}
		if _, err := svc.Get(ctx, ent, "org:team-a", ids[winner]); err != nil {
			t.Fatalf("iteration %d: winner's case missing: %v", i, err)
		}
		if _, err := svc.Get(ctx, ent, "org:team-a", ids[1-winner]); !errors.Is(err, workcase.ErrNotFound) {
			t.Fatalf("iteration %d: loser's case leaked (its transaction was not fully rolled back): %v", i, err)
		}
	}
}

// TestPostgresReplayReturnsRecordedResultNotCurrentState proves a replayed
// command returns the result RECORDED when the command originally applied,
// not the case's current state: create, advance the case, then replay the
// original Create key.
func TestPostgresReplayReturnsRecordedResultNotCurrentState(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	svc, _, ent := newPostgresService(t, dsn)

	createKey := runID("idem-recorded-create")
	createCmd := workcase.CreateCommand{Command: workcase.Command{
		EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", IdempotencyKey: createKey,
	}}
	first, err := svc.Create(ctx, createCmd)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.ProposePlan(ctx, workcase.ProposePlanCommand{
		Command: workcase.Command{EnterpriseID: ent, OrgScope: "org:team-a", ActorRef: "actor-1", CaseID: first.ID, ExpectedRevision: first.Revision, IdempotencyKey: runID("idem-recorded-plan")},
		Plan:    validPlan(),
	}); err != nil {
		t.Fatalf("ProposePlan: %v", err)
	}

	replay, err := svc.Create(ctx, createCmd)
	if err != nil {
		t.Fatalf("replayed Create: %v", err)
	}
	if replay.ID != first.ID || replay.Revision != 1 || replay.Status != sdkworkcase.StatusDraft || len(replay.Plans) != 0 {
		t.Fatalf("replay must be the recorded creation-time result (revision 1, draft, no plans), got %+v", replay)
	}
	current, err := svc.Get(ctx, ent, "org:team-a", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 || len(current.Plans) != 1 {
		t.Fatalf("fixture broken: current state should have advanced past the recorded result, got %+v", current)
	}
}

// ---------------------------------------------------------------------------
// Shared Store conformance suite: one behavioral table, run against
// MemoryStore unconditionally and against PostgresStore when
// ATLAS_TEST_POSTGRES_DSN is set, pinning both implementations to a single
// contract (identical typed outcomes for identical inputs).

var confSeq atomic.Uint64

// confID returns a run-unique identifier, also valid as a >=16-char
// idempotency key, safe across reruns against the shared scratch database
// and across tight loops that outrun runID's clock resolution.
func confID(prefix string) string {
	return fmt.Sprintf("%s-%04d", runID(prefix), confSeq.Add(1))
}

type conformanceEnv struct {
	store      workcase.Store
	entA, entB string
}

func conformanceBackends() []struct {
	name  string
	setup func(t *testing.T) conformanceEnv
} {
	return []struct {
		name  string
		setup func(t *testing.T) conformanceEnv
	}{
		{name: "MemoryStore", setup: func(t *testing.T) conformanceEnv {
			t.Helper()
			return conformanceEnv{store: workcase.NewMemoryStore(nil), entA: confID("ent-conf-a"), entB: confID("ent-conf-b")}
		}},
		{name: "PostgresStore", setup: func(t *testing.T) conformanceEnv {
			t.Helper()
			dsn := testDSN(t)
			ctx := context.Background()
			if err := storage.Migrate(ctx, dsn); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			pool, err := storage.NewPool(ctx, dsn, nil)
			if err != nil {
				t.Fatalf("pool: %v", err)
			}
			t.Cleanup(pool.Close)
			env := conformanceEnv{entA: confID("ent-conf-a"), entB: confID("ent-conf-b")}
			for _, ent := range []string{env.entA, env.entB} {
				if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Workcase Conformance')`, ent); err != nil {
					t.Fatalf("seed enterprise %s: %v", ent, err)
				}
			}
			store, err := workcase.NewPostgresStore(pool, nil)
			if err != nil {
				t.Fatalf("NewPostgresStore: %v", err)
			}
			env.store = store
			return env
		}},
	}
}

func newCaseMutate(id, ent, org string) workcase.Mutate {
	return func(sdkworkcase.WorkCase) (sdkworkcase.WorkCase, error) {
		return sdkworkcase.WorkCase{ID: id, EnterpriseID: ent, OrgScope: org, ActorRef: "actor-conformance", Status: sdkworkcase.StatusDraft, Revision: 1}, nil
	}
}

func bumpRevisionMutate(current sdkworkcase.WorkCase) (sdkworkcase.WorkCase, error) {
	next := current
	next.Revision++
	return next, nil
}

var errMutateMustNotRun = errors.New("conformance: mutate ran when it must not have")

func poisonMutate(sdkworkcase.WorkCase) (sdkworkcase.WorkCase, error) {
	return sdkworkcase.WorkCase{}, errMutateMustNotRun
}

func confCmd(ent, org, caseID string, rev uint64, key string) workcase.Command {
	return workcase.Command{EnterpriseID: ent, OrgScope: org, ActorRef: "actor-conformance", CaseID: caseID, ExpectedRevision: rev, IdempotencyKey: key}
}

func confCreate(t *testing.T, env conformanceEnv, ent, org, id, key string) sdkworkcase.WorkCase {
	t.Helper()
	c, err := env.store.Apply(context.Background(), confCmd(ent, org, id, 0, key), workcase.EventCaseCreated, newCaseMutate(id, ent, org))
	if err != nil {
		t.Fatalf("conformance create %s: %v", id, err)
	}
	return c
}

func TestPostgresAndMemoryStoreConformance(t *testing.T) {
	for _, backend := range conformanceBackends() {
		t.Run(backend.name, func(t *testing.T) {
			env := backend.setup(t)
			ctx := context.Background()
			const orgA, orgB = "org:conf-a", "org:conf-b"

			t.Run("IdempotencyFirstReplayReturnsRecordedResult", func(t *testing.T) {
				id, key := confID("case-replay"), confID("k-replay")
				first := confCreate(t, env, env.entA, orgA, id, key)
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgA, id, 1, confID("k-bump")), workcase.EventPlanProposed, bumpRevisionMutate); err != nil {
					t.Fatalf("advance: %v", err)
				}
				// Replaying the original create: a fresh application would
				// fail the revision check (0 vs current 2), and poisonMutate
				// would error if invoked -- the recorded result must come
				// back before either can happen.
				replay, err := env.store.Apply(ctx, confCmd(env.entA, orgA, id, 0, key), workcase.EventCaseCreated, poisonMutate)
				if err != nil {
					t.Fatalf("replay: %v", err)
				}
				if replay.ID != first.ID || replay.Revision != first.Revision || replay.Status != first.Status {
					t.Fatalf("replay diverged from recorded result: recorded=%+v replay=%+v", first, replay)
				}
			})

			t.Run("ReplayWithWrongOrgScopeIsNotFound", func(t *testing.T) {
				id, key := confID("case-orgreplay"), confID("k-orgreplay")
				confCreate(t, env, env.entA, orgA, id, key)
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgB, id, 0, key), workcase.EventCaseCreated, poisonMutate); !errors.Is(err, workcase.ErrNotFound) {
					t.Fatalf("same key + same case, wrong org: got %v, want ErrNotFound", err)
				}
				// Wrong org must mask the key's very existence: even with a
				// different CaseID (which same-org callers see as
				// ErrIdempotencyKeyReused), a wrong-org caller learns only
				// not-found.
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgB, confID("case-orgreplay-other"), 0, key), workcase.EventCaseCreated, poisonMutate); !errors.Is(err, workcase.ErrNotFound) {
					t.Fatalf("same key + different case, wrong org: got %v, want ErrNotFound", err)
				}
			})

			t.Run("SameKeyDifferentCaseIDIsReused", func(t *testing.T) {
				key := confID("k-shared")
				confCreate(t, env, env.entA, orgA, confID("case-shared-x"), key)
				otherID := confID("case-shared-y")
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgA, otherID, 0, key), workcase.EventCaseCreated, newCaseMutate(otherID, env.entA, orgA)); !errors.Is(err, workcase.ErrIdempotencyKeyReused) {
					t.Fatalf("got %v, want ErrIdempotencyKeyReused", err)
				}
			})

			t.Run("CrossEnterpriseKeyIsolation", func(t *testing.T) {
				key := confID("k-isolated")
				idA, idB := confID("case-iso-a"), confID("case-iso-b")
				a := confCreate(t, env, env.entA, orgA, idA, key)
				b := confCreate(t, env, env.entB, orgA, idB, key)
				if a.ID == b.ID {
					t.Fatalf("fixture broken: expected two distinct cases, got %q twice", a.ID)
				}
				replayA, err := env.store.Apply(ctx, confCmd(env.entA, orgA, idA, 0, key), workcase.EventCaseCreated, poisonMutate)
				if err != nil || replayA.ID != idA {
					t.Fatalf("enterprise A replay: got %+v err=%v, want case %s", replayA, err, idA)
				}
				replayB, err := env.store.Apply(ctx, confCmd(env.entB, orgA, idB, 0, key), workcase.EventCaseCreated, poisonMutate)
				if err != nil || replayB.ID != idB {
					t.Fatalf("enterprise B replay: got %+v err=%v, want case %s (no cross-enterprise bleed)", replayB, err, idB)
				}
			})

			t.Run("StaleRevisionTypedRejections", func(t *testing.T) {
				id := confID("case-stale")
				confCreate(t, env, env.entA, orgA, id, confID("k-stale-create"))
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgA, id, 0, confID("k-stale-low")), workcase.EventPlanProposed, bumpRevisionMutate); !errors.Is(err, workcase.ErrStaleRevision) {
					t.Fatalf("expected revision below current: got %v, want ErrStaleRevision", err)
				}
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgA, id, 7, confID("k-stale-high")), workcase.EventPlanProposed, bumpRevisionMutate); !errors.Is(err, workcase.ErrStaleRevision) {
					t.Fatalf("expected revision above current: got %v, want ErrStaleRevision", err)
				}
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgA, id, 0, confID("k-stale-recreate")), workcase.EventCaseCreated, newCaseMutate(id, env.entA, orgA)); !errors.Is(err, workcase.ErrStaleRevision) {
					t.Fatalf("recreate over an existing case: got %v, want ErrStaleRevision", err)
				}
			})

			t.Run("CrossTenantReadsAndWritesLookLikeNotFound", func(t *testing.T) {
				id := confID("case-tenant")
				confCreate(t, env, env.entA, orgA, id, confID("k-tenant-create"))
				if _, err := env.store.Get(ctx, env.entB, orgA, id); !errors.Is(err, workcase.ErrNotFound) {
					t.Fatalf("wrong-enterprise Get: got %v, want ErrNotFound", err)
				}
				if _, err := env.store.Get(ctx, env.entA, orgB, id); !errors.Is(err, workcase.ErrNotFound) {
					t.Fatalf("wrong-org Get: got %v, want ErrNotFound", err)
				}
				if _, err := env.store.Apply(ctx, confCmd(env.entB, orgA, id, 1, confID("k-tenant-entb")), workcase.EventPlanProposed, bumpRevisionMutate); !errors.Is(err, workcase.ErrNotFound) {
					t.Fatalf("wrong-enterprise Apply: got %v, want ErrNotFound", err)
				}
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgB, id, 1, confID("k-tenant-orgb")), workcase.EventPlanProposed, bumpRevisionMutate); !errors.Is(err, workcase.ErrNotFound) {
					t.Fatalf("wrong-org Apply: got %v, want ErrNotFound", err)
				}
			})

			t.Run("CreateCollidingCaseIDCrossOrgIsStaleRevision", func(t *testing.T) {
				id := confID("case-collide-org")
				orig := confCreate(t, env, env.entA, orgA, id, confID("k-collide-org-1"))
				if _, err := env.store.Apply(ctx, confCmd(env.entA, orgB, id, 0, confID("k-collide-org-2")), workcase.EventCaseCreated, newCaseMutate(id, env.entA, orgB)); !errors.Is(err, workcase.ErrStaleRevision) {
					t.Fatalf("cross-org id collision: got %v, want ErrStaleRevision (CaseIDs are globally unique)", err)
				}
				got, err := env.store.Get(ctx, env.entA, orgA, id)
				if err != nil || got.Revision != orig.Revision || got.OrgScope != orgA {
					t.Fatalf("original case damaged by the colliding create: got=%+v err=%v", got, err)
				}
			})

			t.Run("CreateCollidingCaseIDCrossEnterpriseIsStaleRevision", func(t *testing.T) {
				id := confID("case-collide-ent")
				orig := confCreate(t, env, env.entA, orgA, id, confID("k-collide-ent-1"))
				if _, err := env.store.Apply(ctx, confCmd(env.entB, orgA, id, 0, confID("k-collide-ent-2")), workcase.EventCaseCreated, newCaseMutate(id, env.entB, orgA)); !errors.Is(err, workcase.ErrStaleRevision) {
					t.Fatalf("cross-enterprise id collision: got %v, want ErrStaleRevision (CaseIDs are globally unique)", err)
				}
				got, err := env.store.Get(ctx, env.entA, orgA, id)
				if err != nil || got.Revision != orig.Revision || got.EnterpriseID != env.entA {
					t.Fatalf("original case damaged by the colliding create: got=%+v err=%v", got, err)
				}
			})
		})
	}
}
