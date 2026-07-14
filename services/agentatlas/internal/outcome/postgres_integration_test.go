package outcome_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	model "github.com/astraclawteam/agentatlas/sdk/go/outcome"
	dbfs "github.com/astraclawteam/agentatlas/services/agentatlas/db"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/outcome"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

// testDSN gates the Postgres legs on ATLAS_TEST_POSTGRES_DSN, exactly like the
// workcase/operatingmap integration tests.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	return dsn
}

var confSeq atomic.Uint64

func confID(prefix string) string {
	return fmt.Sprintf("%s-%s-%04d", prefix, strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-"), confSeq.Add(1))
}

func fixedNow() func() time.Time {
	n := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return n }
}

// --- fixtures --------------------------------------------------------------

func obsRef() model.ObservationRef {
	return model.ObservationRef{
		Handle: "obs-1", ObservationHash: "sha256:1", Authority: "system_of_record",
		SignatureKeyID: "nexus-key-1", ObservedAt: time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC),
	}
}

func satisfiedOutcome(tenant, key string, rev uint64) model.Outcome {
	o := model.Outcome{
		Tenant: tenant, OutcomeKey: key, Revision: rev,
		Claim: model.OutcomeClaim{
			Goal:   model.GoalRef{Tenant: tenant, GoalKey: "mes.close_anomaly", GoalVersion: 3},
			Status: model.OutcomeSatisfied, RuleVersion: "outcome-policy-rev-7",
			Observations: []model.ObservationRef{obsRef()},
			Confidence:   []model.ConfidenceComponent{{Kind: "authority", Score: 0.9}},
		},
		WorkCaseID: "case-42", WorkCaseRevision: 5, WorkPlanRevision: 2,
		OperatingMapVersion: 4, OrgVersion: 1,
		Contributions: []model.ContributionRef{{ContributorID: "agent:planner", Kind: "method", Weight: 0.5}},
		DecidedAt:     time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
	}
	return o
}

// correctionOf returns revision rev superseding rev-1 of prior.
func correctionOf(prior model.Outcome, rev uint64, status model.OutcomeStatus) model.Outcome {
	o := prior
	o.Revision = rev
	o.Claim.Status = status
	if status != model.OutcomeSatisfied {
		o.Claim.Observations = nil
	}
	o.Supersedes = &model.OutcomeRevisionRef{
		OutcomeKey: prior.OutcomeKey, Revision: rev - 1,
		OutcomeID: model.StableID(prior.Tenant, model.NodeOutcome, prior.OutcomeKey, rev-1),
		Reason:    "correction",
	}
	return o
}

// --- shared conformance suite: Memory + Postgres pinned to one contract ----

type env struct {
	store outcome.Store
	// readNodeSummary reads the persisted summary of a lineage node by its id,
	// returning (summary, found). It is wired per backend (MemoryStore via the
	// export_test helper; PostgresStore via a direct table query) so the
	// conformance suite can prove node immutability WITHOUT adding a production
	// node getter -- a lineage-node reader/traversal is Task 0I scope.
	readNodeSummary func(id string) (string, bool)
	entA, entB      string
}

func backends() []struct {
	name  string
	setup func(t *testing.T) env
} {
	return []struct {
		name  string
		setup func(t *testing.T) env
	}{
		{name: "MemoryStore", setup: func(t *testing.T) env {
			t.Helper()
			ms := outcome.NewMemoryStore(fixedNow())
			return env{
				store:           ms,
				readNodeSummary: func(id string) (string, bool) { return ms.NodeSummaryForTest(id) },
				entA:            confID("ent-a"), entB: confID("ent-b"),
			}
		}},
		{name: "PostgresStore", setup: func(t *testing.T) env {
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
			e := env{entA: confID("ent-a"), entB: confID("ent-b")}
			for _, ent := range []string{e.entA, e.entB} {
				if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Outcome Conformance')`, ent); err != nil {
					t.Fatalf("seed enterprise: %v", err)
				}
			}
			store, err := outcome.NewPostgresStore(pool, fixedNow())
			if err != nil {
				t.Fatalf("NewPostgresStore: %v", err)
			}
			e.store = store
			e.readNodeSummary = func(id string) (string, bool) {
				var summary *string
				err := pool.QueryRow(context.Background(), `SELECT summary FROM outcome_lineage_nodes WHERE id=$1`, id).Scan(&summary)
				if err != nil {
					return "", false
				}
				if summary == nil {
					return "", true
				}
				return *summary, true
			}
			return e
		}},
	}
}

// TestPostgresOutcomeMigrationUpDownUp proves migration 000016 is reversible on
// an EMPTY database. It runs on its OWN ephemeral database (withFreshOutcomeDB)
// so it is isolated from any shared-DSN pollution: Task 0I's shared outbox
// (outcome_graph_outbox, migration 000017) is append-only and its down-guard
// (correctly) refuses to drop once ANY package has written a projection event —
// so this test must not share a database with the outbox writers.
func TestPostgresOutcomeMigrationUpDownUp(t *testing.T) {
	withFreshOutcomeDB(t, testDSN(t), func(ctx context.Context, dsn string) {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := goose.SetDialect("postgres"); err != nil {
			t.Fatal(err)
		}
		goose.SetBaseFS(dbfs.Migrations)

		if err := goose.DownToContext(ctx, db, "migrations", 15); err != nil {
			t.Fatalf("down to 15: %v", err)
		}
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='outcomes')`).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatal("down migration must drop the outcomes table")
		}
		if err := storage.Migrate(ctx, dsn); err != nil {
			t.Fatalf("migrate up again: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='outcomes')`).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatal("up migration must recreate the outcomes table")
		}
	})
}

func TestOutcomeStoreConformance(t *testing.T) {
	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			e := backend.setup(t)
			ctx := context.Background()

			t.Run("AppendAndReadRevisionOne", func(t *testing.T) {
				key := confID("k")
				o := satisfiedOutcome(e.entA, key, 1)
				got, err := e.store.AppendOutcome(ctx, o)
				if err != nil {
					t.Fatalf("append: %v", err)
				}
				if got.ID != o.DerivedID() {
					t.Fatalf("store must assign the deterministic id %q, got %q", o.DerivedID(), got.ID)
				}
				read, err := e.store.GetOutcome(ctx, e.entA, key, 1)
				if err != nil || read.Claim.Status != model.OutcomeSatisfied {
					t.Fatalf("get revision 1: %+v err=%v", read, err)
				}
			})

			t.Run("SatisfiedWithoutObservationRejectedAndPersistsNothing", func(t *testing.T) {
				key := confID("k")
				o := satisfiedOutcome(e.entA, key, 1)
				o.Claim.Observations = nil
				if _, err := e.store.AppendOutcome(ctx, o); err == nil {
					t.Fatal("a satisfied outcome without an authoritative observation must be rejected")
				}
				if _, err := e.store.GetOutcome(ctx, e.entA, key, 1); !errors.Is(err, outcome.ErrNotFound) {
					t.Fatalf("rejected outcome must persist nothing, got %v", err)
				}
			})

			t.Run("ActionReceiptCannotStandInForOutcome", func(t *testing.T) {
				key := confID("k")
				o := satisfiedOutcome(e.entA, key, 1)
				o.Claim.Observations = nil
				o.ActionReceipts = []model.ReceiptRef{{Handle: "r", ReceiptHash: "sha256:d", SignatureKeyID: "nexus-key-1"}}
				if _, err := e.store.AppendOutcome(ctx, o); !errors.Is(err, model.ErrActionReceiptNotOutcomeEvidence) {
					t.Fatalf("action-receipt-only satisfied outcome: got %v, want ErrActionReceiptNotOutcomeEvidence", err)
				}
			})

			t.Run("DuplicateContributionRejectedAndPersistsNothing", func(t *testing.T) {
				key := confID("k")
				o := satisfiedOutcome(e.entA, key, 1)
				o.Contributions = []model.ContributionRef{
					{ContributorID: "agent:x", Kind: "method"}, {ContributorID: "agent:x", Kind: "method"},
				}
				if _, err := e.store.AppendOutcome(ctx, o); !errors.Is(err, model.ErrDuplicateContribution) {
					t.Fatalf("duplicate credit: got %v, want ErrDuplicateContribution", err)
				}
				if _, err := e.store.GetOutcome(ctx, e.entA, key, 1); !errors.Is(err, outcome.ErrNotFound) {
					t.Fatalf("rejected outcome must persist nothing, got %v", err)
				}
			})

			t.Run("RevisionCannotBeRewritten", func(t *testing.T) {
				key := confID("k")
				if _, err := e.store.AppendOutcome(ctx, satisfiedOutcome(e.entA, key, 1)); err != nil {
					t.Fatalf("append rev1: %v", err)
				}
				rewrite := satisfiedOutcome(e.entA, key, 1)
				rewrite.Claim.RuleVersion = "tampered"
				if _, err := e.store.AppendOutcome(ctx, rewrite); !errors.Is(err, outcome.ErrRevisionExists) {
					t.Fatalf("re-writing revision 1 (overwriting history): got %v, want ErrRevisionExists", err)
				}
				// The original revision 1 remains intact.
				read, err := e.store.GetOutcome(ctx, e.entA, key, 1)
				if err != nil || read.Claim.RuleVersion != "outcome-policy-rev-7" {
					t.Fatalf("original revision 1 must remain unchanged, got %+v err=%v", read, err)
				}
			})

			t.Run("CorrectionAppendsNewRevisionPreservingHistory", func(t *testing.T) {
				key := confID("k")
				rev1 := satisfiedOutcome(e.entA, key, 1)
				if _, err := e.store.AppendOutcome(ctx, rev1); err != nil {
					t.Fatalf("append rev1: %v", err)
				}
				rev2 := correctionOf(rev1, 2, model.OutcomeUnsatisfied)
				if _, err := e.store.AppendOutcome(ctx, rev2); err != nil {
					t.Fatalf("append correction rev2: %v", err)
				}
				// Prior revision remains readable and UNCHANGED (satisfied).
				old, err := e.store.GetOutcome(ctx, e.entA, key, 1)
				if err != nil || old.Claim.Status != model.OutcomeSatisfied {
					t.Fatalf("correction must preserve the prior revision: got %+v err=%v", old, err)
				}
				latest, err := e.store.LatestOutcome(ctx, e.entA, key)
				if err != nil || latest.Revision != 2 || latest.Claim.Status != model.OutcomeUnsatisfied {
					t.Fatalf("latest must be the correction rev2/unsatisfied: got %+v err=%v", latest, err)
				}
			})

			t.Run("CorrectionMustSupersedeHead", func(t *testing.T) {
				key := confID("k")
				if _, err := e.store.AppendOutcome(ctx, satisfiedOutcome(e.entA, key, 1)); err != nil {
					t.Fatalf("append rev1: %v", err)
				}
				// A revision 3 that skips head (rev 1, want rev 2) is a broken chain.
				skip := satisfiedOutcome(e.entA, key, 3)
				skip.Supersedes = &model.OutcomeRevisionRef{OutcomeKey: key, Revision: 2, OutcomeID: "x", Reason: "correction"}
				if _, err := e.store.AppendOutcome(ctx, skip); !errors.Is(err, outcome.ErrBrokenSupersession) {
					t.Fatalf("out-of-sequence correction: got %v, want ErrBrokenSupersession", err)
				}
			})

			t.Run("CrossTenantReadIsNotFound", func(t *testing.T) {
				key := confID("k")
				if _, err := e.store.AppendOutcome(ctx, satisfiedOutcome(e.entA, key, 1)); err != nil {
					t.Fatalf("append: %v", err)
				}
				if _, err := e.store.GetOutcome(ctx, e.entB, key, 1); !errors.Is(err, outcome.ErrNotFound) {
					t.Fatalf("cross-tenant read: got %v, want ErrNotFound", err)
				}
			})

			t.Run("CrossTenantLineageEdgeRejected", func(t *testing.T) {
				edge := model.LineageEdge{
					Tenant: e.entA, Type: model.EdgeContributesTo,
					From: model.LineageEndpoint{Tenant: e.entA, Type: model.NodeContributor, BusinessID: "agent:x", Revision: 1},
					To:   model.LineageEndpoint{Tenant: e.entB, Type: model.NodeOutcome, BusinessID: "case.goal", Revision: 1},
				}
				if err := e.store.AppendLineage(ctx, nil, []model.LineageEdge{edge}); !errors.Is(err, model.ErrCrossTenantEdge) {
					t.Fatalf("cross-tenant edge: got %v, want ErrCrossTenantEdge", err)
				}
			})

			t.Run("UnversionedLineageEndpointRejected", func(t *testing.T) {
				node := model.LineageNode{Tenant: e.entA, Type: model.NodeOutcome, BusinessID: "o", Revision: 0}
				if err := e.store.AppendLineage(ctx, []model.LineageNode{node}, nil); !errors.Is(err, model.ErrUnversionedEndpoint) {
					t.Fatalf("unversioned node: got %v, want ErrUnversionedEndpoint", err)
				}
			})

			t.Run("LineageNodesAndEdgesAppendIdempotently", func(t *testing.T) {
				node := model.LineageNode{Tenant: e.entA, Type: model.NodeOutcome, BusinessID: confID("o"), Revision: 1}
				edge := model.LineageEdge{
					Tenant: e.entA, Type: model.EdgeConcerns,
					From: model.LineageEndpoint{Tenant: e.entA, Type: model.NodeOutcome, BusinessID: node.BusinessID, Revision: 1},
					To:   model.LineageEndpoint{Tenant: e.entA, Type: model.NodeGoal, BusinessID: confID("g"), Revision: 1},
				}
				if err := e.store.AppendLineage(ctx, []model.LineageNode{node}, []model.LineageEdge{edge}); err != nil {
					t.Fatalf("append lineage: %v", err)
				}
				// Re-appending the same immutable fact is a no-op, never an error.
				if err := e.store.AppendLineage(ctx, []model.LineageNode{node}, []model.LineageEdge{edge}); err != nil {
					t.Fatalf("idempotent re-append: %v", err)
				}
			})

			t.Run("LineageNodeReAppendIsImmutableFirstWriteWins", func(t *testing.T) {
				// A node's StableID excludes its summary, so re-appending the
				// SAME id with a DIFFERENT summary must NOT mutate the stored
				// node: both stores keep the FIRST value (append-only). This
				// catches the MemoryStore last-write-wins divergence from
				// PostgresStore's ON CONFLICT DO NOTHING.
				bid := confID("o")
				first := model.LineageNode{Tenant: e.entA, Type: model.NodeOutcome, BusinessID: bid, Revision: 1, Summary: "first"}
				if err := e.store.AppendLineage(ctx, []model.LineageNode{first}, nil); err != nil {
					t.Fatalf("append first: %v", err)
				}
				mutated := first
				mutated.Summary = "second-MUTATED"
				if err := e.store.AppendLineage(ctx, []model.LineageNode{mutated}, nil); err != nil {
					t.Fatalf("re-append mutated: %v", err)
				}
				got, ok := e.readNodeSummary(first.DerivedID())
				if !ok {
					t.Fatal("node not found after append")
				}
				if got != "first" {
					t.Fatalf("re-append mutated an immutable node: summary=%q, want %q (first-write-wins)", got, "first")
				}
			})

			t.Run("ProjectionEventsAppendTombstoneNeverEdit", func(t *testing.T) {
				subj := model.StableID(e.entA, model.NodeOutcome, confID("o"), 1)
				ev1, err := e.store.AppendProjectionEvent(ctx, model.ProjectionEvent{
					Tenant: e.entA, Kind: model.ProjectionOutcomeRevision, SubjectType: model.NodeOutcome,
					SubjectID: subj, SubjectRevision: 1, PayloadHash: "sha256:1", RecordedAt: time.Now().UTC(),
				})
				if err != nil || ev1.Sequence == 0 {
					t.Fatalf("append event 1: %+v err=%v", ev1, err)
				}
				ev2, err := e.store.AppendProjectionEvent(ctx, model.ProjectionEvent{
					Tenant: e.entA, Kind: model.ProjectionTombstone, SubjectType: model.NodeOutcome,
					SubjectID: subj, SubjectRevision: 1, PayloadHash: "sha256:2", RecordedAt: time.Now().UTC(),
					SupersedesSequence: ev1.Sequence,
				})
				if err != nil {
					t.Fatalf("append tombstone: %v", err)
				}
				if ev2.Sequence <= ev1.Sequence {
					t.Fatalf("tombstone must be a NEW appended event (seq %d) after %d", ev2.Sequence, ev1.Sequence)
				}
			})

			t.Run("WatermarkAdvancesMonotonically", func(t *testing.T) {
				if _, err := e.store.AdvanceWatermark(ctx, e.entA, 5); err != nil {
					t.Fatalf("advance to 5: %v", err)
				}
				w, err := e.store.Watermark(ctx, e.entA)
				if err != nil || w.LastSequence != 5 {
					t.Fatalf("watermark: %+v err=%v", w, err)
				}
				if _, err := e.store.AdvanceWatermark(ctx, e.entA, 3); !errors.Is(err, outcome.ErrWatermarkRegression) {
					t.Fatalf("watermark regression: got %v, want ErrWatermarkRegression", err)
				}
				if _, err := e.store.AdvanceWatermark(ctx, e.entA, 9); err != nil {
					t.Fatalf("advance to 9: %v", err)
				}
			})
		})
	}
}

// --- Postgres-only: immutability triggers, DB defense-in-depth, races -------

func TestPostgresOutcomeImmutabilityAndDownGuard(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ent := confID("ent-immut")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Immut')`, ent); err != nil {
		t.Fatal(err)
	}
	store, err := outcome.NewPostgresStore(pool, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	key := confID("k")
	if _, err := store.AppendOutcome(ctx, satisfiedOutcome(ent, key, 1)); err != nil {
		t.Fatalf("append terminal satisfied outcome: %v", err)
	}

	// mutate-terminal rejected: a raw UPDATE/DELETE/TRUNCATE of a terminal
	// outcome row is refused by the append-only trigger.
	if _, err := pool.Exec(ctx, `UPDATE outcomes SET status='unsatisfied' WHERE tenant=$1 AND outcome_key=$2`, ent, key); err == nil {
		t.Fatal("UPDATE of a terminal outcome must be rejected by the immutability trigger")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM outcomes WHERE tenant=$1 AND outcome_key=$2`, ent, key); err == nil {
		t.Fatal("DELETE of an outcome must be rejected by the immutability trigger")
	}
	if _, err := pool.Exec(ctx, `TRUNCATE outcomes`); err == nil {
		t.Fatal("TRUNCATE of outcomes must be rejected by the statement-level trigger")
	}

	// (The satisfied/desync/unsigned CHECK constraints are proven by the
	// dedicated TestPostgresRejectsDesyncAndUnsignedSatisfied.)

	// Migration down guard: refuse to drop while governed history exists.
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.DownToContext(ctx, sqldb, "migrations", 15); err == nil {
		t.Fatal("down migration must refuse to drop outcomes while governed history exists")
	}
}

// TestPostgresTwoWriterRevisionRace has two goroutines race the SAME correction
// revision (rev 2) against one committed rev 1: exactly one wins, the other is
// rejected, and the chain ends with a single rev 2.
//
// NOTE: the per-(tenant, outcome_key) advisory lock in AppendOutcome is an
// optimization that lets the loser fail fast with ErrRevisionExists on the head
// re-read; the REAL serialization backstop is the `UNIQUE (tenant, outcome_key,
// revision)` constraint (both writers derive the SAME id and target the same
// (tenant,key,revision), so at most one INSERT can commit). This test therefore
// still passes even if the advisory lock were removed — the loser would then
// surface the unique violation mapped to ErrRevisionExists instead.
func TestPostgresTwoWriterRevisionRace(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ent := confID("ent-race")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Race')`, ent); err != nil {
		t.Fatal(err)
	}
	store, err := outcome.NewPostgresStore(pool, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	key := confID("k")
	rev1 := satisfiedOutcome(ent, key, 1)
	if _, err := store.AppendOutcome(ctx, rev1); err != nil {
		t.Fatalf("seed rev1: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.AppendOutcome(ctx, correctionOf(rev1, 2, model.OutcomeUnsatisfied))
		}(i)
	}
	wg.Wait()

	wins, rejects := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, outcome.ErrRevisionExists) || errors.Is(err, outcome.ErrBrokenSupersession):
			rejects++
		default:
			t.Fatalf("unexpected error from racing writer: %v", err)
		}
	}
	if wins != 1 || rejects != 1 {
		t.Fatalf("want exactly one winner and one reject, got wins=%d rejects=%d (%v)", wins, rejects, errs)
	}
	var rev2count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outcomes WHERE tenant=$1 AND outcome_key=$2 AND revision=2`, ent, key).Scan(&rev2count); err != nil {
		t.Fatal(err)
	}
	if rev2count != 1 {
		t.Fatalf("chain corrupted: want exactly one rev2, got %d", rev2count)
	}
}

// withFreshOutcomeDB creates a brand-new ephemeral database, migrates it to
// head, runs fn against it, and drops it — so a test can assert behavior on a
// GUARANTEED-empty 0G schema regardless of what other tests wrote to the shared
// scratch DB (append-only tables can never be cleaned in place). Mirrors the
// reviewer's manual reproduction.
func withFreshOutcomeDB(t *testing.T, baseDSN string, fn func(ctx context.Context, dsn string)) {
	t.Helper()
	ctx := context.Background()
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := fmt.Sprintf("atlas_test_task0g_eph_%s", strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", ""))

	admin := *u
	admin.Path = "/postgres"
	adminDB, err := sql.Open("pgx", admin.String())
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer adminDB.Close()
	if _, err := adminDB.ExecContext(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create ephemeral db: %v", err)
	}
	defer func() {
		if _, err := adminDB.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Logf("drop ephemeral db %s: %v", name, err)
		}
	}()

	ephemeral := *u
	ephemeral.Path = "/" + name
	dsn := ephemeral.String()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate ephemeral: %v", err)
	}
	fn(ctx, dsn)
}

// TestPostgresDownGuardCoversAllGovernedTables proves the 000016 down guard
// refuses to drop when history exists in ANY of the four append-only governed
// tables — not only `outcomes`. Uses an ephemeral DB so `outcomes` is genuinely
// empty while lineage/projection history is present (BLOCKER 1). Against the
// unfixed outcomes-only guard, each leg's `down` succeeds and destroys the
// history — so this test FAILS (RED) until the guard covers every table.
func TestPostgresDownGuardCoversAllGovernedTables(t *testing.T) {
	dsn := testDSN(t)

	downRefused := func(ctx context.Context, dsn string) bool {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()
		if err := goose.SetDialect("postgres"); err != nil {
			t.Fatal(err)
		}
		goose.SetBaseFS(dbfs.Migrations)
		return goose.DownToContext(ctx, db, "migrations", 15) != nil
	}

	t.Run("LineageNodeOnly", func(t *testing.T) {
		withFreshOutcomeDB(t, dsn, func(ctx context.Context, dsn string) {
			pool, err := storage.NewPool(ctx, dsn, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ent := confID("ent-dg-node")
			if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'DG')`, ent); err != nil {
				t.Fatal(err)
			}
			store, err := outcome.NewPostgresStore(pool, fixedNow())
			if err != nil {
				t.Fatal(err)
			}
			node := model.LineageNode{Tenant: ent, Type: model.NodeOutcome, BusinessID: confID("o"), Revision: 1}
			if err := store.AppendLineage(ctx, []model.LineageNode{node}, nil); err != nil {
				t.Fatalf("append node: %v", err)
			}
			var outcomeCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM outcomes`).Scan(&outcomeCount); err != nil {
				t.Fatal(err)
			}
			if outcomeCount != 0 {
				t.Fatalf("fixture broken: expected zero outcomes, got %d", outcomeCount)
			}
			if !downRefused(ctx, dsn) {
				t.Fatal("down must be REFUSED when lineage-node history exists (zero outcomes)")
			}
			var nodeCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM outcome_lineage_nodes`).Scan(&nodeCount); err != nil {
				t.Fatalf("lineage node table must survive the refused down: %v", err)
			}
			if nodeCount != 1 {
				t.Fatalf("lineage node history destroyed by down: count=%d", nodeCount)
			}
		})
	})

	t.Run("ProjectionEventOnly", func(t *testing.T) {
		withFreshOutcomeDB(t, dsn, func(ctx context.Context, dsn string) {
			pool, err := storage.NewPool(ctx, dsn, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ent := confID("ent-dg-proj")
			if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'DG')`, ent); err != nil {
				t.Fatal(err)
			}
			store, err := outcome.NewPostgresStore(pool, fixedNow())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AppendProjectionEvent(ctx, model.ProjectionEvent{
				Tenant: ent, Kind: model.ProjectionOutcomeRevision, SubjectType: model.NodeOutcome,
				SubjectID: model.StableID(ent, model.NodeOutcome, confID("o"), 1), SubjectRevision: 1,
				PayloadHash: "sha256:1", RecordedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("append projection event: %v", err)
			}
			var outcomeCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM outcomes`).Scan(&outcomeCount); err != nil {
				t.Fatal(err)
			}
			if outcomeCount != 0 {
				t.Fatalf("fixture broken: expected zero outcomes, got %d", outcomeCount)
			}
			if !downRefused(ctx, dsn) {
				t.Fatal("down must be REFUSED when projection-event history exists (zero outcomes)")
			}
			// Task 0I re-points AppendProjectionEvent onto the ONE shared
			// Outcome-Graph outbox (outcome_graph_outbox, migration 000017),
			// unifying the 0G outcome-only projection path. The projection-event
			// history — and the append-only down-guard protecting it — now live
			// there (the legacy outcome_projection_events table is retired).
			var evCount int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM outcome_graph_outbox`).Scan(&evCount); err != nil {
				t.Fatalf("shared projection outbox must survive the refused down: %v", err)
			}
			if evCount != 1 {
				t.Fatalf("projection event history destroyed by down: count=%d", evCount)
			}
			// The retired 0G table receives no writes.
			var legacy int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM outcome_projection_events`).Scan(&legacy); err != nil {
				t.Fatalf("legacy table read: %v", err)
			}
			if legacy != 0 {
				t.Fatalf("retired outcome_projection_events must not be written, got %d rows", legacy)
			}
		})
	})
}

// TestPostgresRejectsDesyncAndUnsignedSatisfied proves the authoritative store's
// CHECK constraints (not just Go Validate) reject: (a) a promoted `status`
// column that disagrees with `content.claim.status`, (b) a satisfied outcome
// whose observation is unsigned / has no authority, and (c) a satisfied outcome
// with zero observations in content. All bypass the Go layer via raw INSERT.
// (a) and (b) FAIL against the pre-fix schema (RED); (c) is already enforced by
// the presence CHECK and stays green as a regression guard. (BLOCKER 2)
func TestPostgresRejectsDesyncAndUnsignedSatisfied(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	if err := storage.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := storage.NewPool(ctx, dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ent := confID("ent-desync")
	if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Desync')`, ent); err != nil {
		t.Fatal(err)
	}

	// rawInsert marshals o to content and inserts it with the given promoted
	// status (which may deliberately disagree with content), bypassing Go
	// Validate entirely. Returns the DB error (nil == accepted).
	rawInsert := func(o model.Outcome, promotedStatus string) error {
		content, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		_, execErr := pool.Exec(ctx, `INSERT INTO outcomes
			(id,tenant,outcome_key,revision,status,goal_tenant,goal_key,goal_version,rule_version,
			 work_case_id,work_case_revision,work_plan_revision,operating_map_version,org_version,decided_at,content)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			o.DerivedID()+"-"+promotedStatus, o.Tenant, o.OutcomeKey, int64(o.Revision), promotedStatus,
			o.Claim.Goal.Tenant, o.Claim.Goal.GoalKey, o.Claim.Goal.GoalVersion, o.Claim.RuleVersion,
			o.WorkCaseID, int64(o.WorkCaseRevision), int64(o.WorkPlanRevision), o.OperatingMapVersion, o.OrgVersion,
			o.DecidedAt.UTC(), content)
		return execErr
	}

	// (a) promoted/content status desync: content says satisfied (with a valid
	// signed observation), promoted column says unsatisfied.
	desync := satisfiedOutcome(ent, confID("k"), 1)
	if err := rawInsert(desync, "unsatisfied"); err == nil {
		t.Fatal("DB must reject a promoted status that disagrees with content.claim.status")
	}

	// (b) satisfied with an unsigned / no-authority observation.
	unsigned := satisfiedOutcome(ent, confID("k"), 1)
	unsigned.Claim.Observations = []model.ObservationRef{{
		Handle: "obs-x", ObservationHash: "sha256:1", Authority: "", SignatureKeyID: "",
		ObservedAt: time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC),
	}}
	if err := rawInsert(unsigned, "satisfied"); err == nil {
		t.Fatal("DB must reject a satisfied outcome whose observation lacks authority/signature_key_id")
	}

	// (c) satisfied with zero observations in content (regression guard).
	zeroObs := satisfiedOutcome(ent, confID("k"), 1)
	zeroObs.Claim.Observations = nil
	if err := rawInsert(zeroObs, "satisfied"); err == nil {
		t.Fatal("DB must reject a satisfied outcome with zero observations in content")
	}

	// A fully valid satisfied outcome (content and promoted status agree, one
	// signed authoritative observation) is still accepted.
	if err := rawInsert(satisfiedOutcome(ent, confID("k"), 1), "satisfied"); err != nil {
		t.Fatalf("a valid satisfied outcome must still be accepted: %v", err)
	}
}
