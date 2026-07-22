// Concurrent-migration integration test: real PostgreSQL, an EMPTY database,
// and four separate PROCESSES calling storage.Migrate at the same instant.
//
//	docker compose -f deploy/compose/compose.yaml up -d postgres
//	ATLAS_TEST_POSTGRES_DSN=postgres://atlas:atlas@localhost:5432/agentatlas?sslmode=disable \
//	  go test ./tests/integration -run TestConcurrentMigrate -v
//
// Why processes and not goroutines: package goose keeps process-global state
// (SetBaseFS, SetDialect), so four goroutines would contend on globals that
// four containers never share, and any failure would be an artifact of the
// harness rather than of the deployment. cmd/atlas-agent, cmd/atlas-api,
// cmd/atlas-outcome-projector and cmd/atlas-worker each call storage.Migrate at
// startup and compose starts them together, so four processes is the shape.
//
// Why an empty database: an earlier attempt to observe this ran against a
// database already at the current version, where every migrator takes the
// "no migrations to run" path. Concurrent goose with nothing to apply is not
// the dangerous case; a first `docker compose up` is.
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

// concurrentMigrators is the number of AgentAtlas binaries that call
// storage.Migrate at startup. Keep it equal to that count.
const concurrentMigrators = 4

const (
	migrateChildEnv     = "ATLAS_TEST_MIGRATE_CHILD_DSN"
	migrateChildStartAt = "ATLAS_TEST_MIGRATE_CHILD_START_AT"
)

// TestConcurrentMigrateChild is the child half of TestConcurrentMigrate. It is
// a no-op unless the parent re-executes this binary with the DSN in the
// environment, so a plain `go test ./...` never runs it.
func TestConcurrentMigrateChild(t *testing.T) {
	dsn := os.Getenv(migrateChildEnv)
	if dsn == "" {
		t.Skip("child half of TestConcurrentMigrate; run that instead")
	}
	// Every child waits for the same wall-clock instant before calling
	// Migrate. Without it the migrators are staggered by process startup —
	// which is exactly how the inconclusive first attempt let one binary
	// finish all 22 migrations before the others opened a connection.
	if at := os.Getenv(migrateChildStartAt); at != "" {
		startAt, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			t.Fatalf("parse start instant: %v", err)
		}
		time.Sleep(time.Until(startAt))
	}
	if err := storage.Migrate(context.Background(), dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

func TestConcurrentMigrate(t *testing.T) {
	adminDSN := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
	if adminDSN == "" {
		t.Skip("set ATLAS_TEST_POSTGRES_DSN (production-standard postgres from deploy/compose)")
	}
	if os.Getenv(migrateChildEnv) != "" {
		t.Skip("running as a migrator child")
	}

	dsn := freshDatabase(t, adminDSN)

	// A generous head start: the children must all be past process startup and
	// blocked on the clock, otherwise they are not concurrent.
	startAt := time.Now().Add(5 * time.Second)

	type result struct {
		index  int
		err    error
		output string
	}
	results := make([]result, concurrentMigrators)
	var wg sync.WaitGroup
	for i := range concurrentMigrators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestConcurrentMigrateChild", "-test.v")
			cmd.Env = append(os.Environ(),
				migrateChildEnv+"="+dsn,
				migrateChildStartAt+"="+startAt.Format(time.RFC3339Nano),
			)
			out, err := cmd.CombinedOutput()
			results[i] = result{index: i, err: err, output: string(out)}
		}()
	}
	wg.Wait()

	var failed int
	for _, r := range results {
		if r.err != nil {
			failed++
			t.Errorf("migrator %d failed: %v\n%s", r.index, r.err, r.output)
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent migrators failed against an empty database", failed, concurrentMigrators)
	}

	// Success is not just "nobody errored": the schema must be at the single
	// expected version, applied exactly once each.
	assertMigrationsAppliedOnce(t, dsn)
}

// freshDatabase creates an empty database for one run and drops it afterwards.
// The name is derived from the test so a leftover from a crashed run is
// replaced rather than reused — a half-migrated database would make the next
// run take the no-op path and prove nothing.
func freshDatabase(t *testing.T, adminDSN string) string {
	t.Helper()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("atlas_migrate_race_%d", os.Getpid())
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name) + ` WITH (FORCE)`); err != nil {
		t.Fatalf("drop stale database: %v", err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + quoteIdent(name)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", adminDSN)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name) + ` WITH (FORCE)`)
	})
	return replaceDatabase(t, adminDSN, name)
}

// replaceDatabase swaps the database segment of a postgres URL.
func replaceDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		t.Fatalf("ATLAS_TEST_POSTGRES_DSN is not a URL: %s", dsn)
	}
	hostAndPath, query, hasQuery := strings.Cut(rest, "?")
	authority, _, ok := strings.Cut(hostAndPath, "/")
	if !ok {
		t.Fatalf("ATLAS_TEST_POSTGRES_DSN names no database: %s", dsn)
	}
	out := scheme + "://" + authority + "/" + name
	if hasQuery {
		out += "?" + query
	}
	return out
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// assertMigrationsAppliedOnce checks the goose bookkeeping the migrators share:
// one applied row per version, and no version applied twice. A race that let
// two migrators run the same migration would show up here even if neither
// process reported an error.
func assertMigrationsAppliedOnce(t *testing.T, dsn string) {
	t.Helper()
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer conn.Close()

	var duplicates int
	if err := conn.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT version_id FROM goose_db_version
			WHERE is_applied GROUP BY version_id HAVING COUNT(*) > 1
		) AS d`).Scan(&duplicates); err != nil {
		t.Fatalf("read goose bookkeeping: %v", err)
	}
	if duplicates > 0 {
		t.Fatalf("%d migration versions were applied more than once", duplicates)
	}

	var maxVersion int64
	if err := conn.QueryRow(`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&maxVersion); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if maxVersion <= 0 {
		t.Fatalf("schema version = %d; the migrators applied nothing, so this proved nothing", maxVersion)
	}
	t.Logf("%d concurrent migrators converged on schema version %d", concurrentMigrators, maxVersion)
}
