// Package storage wires PostgreSQL access: pgx pool for queries and embedded
// goose migrations so schema application is identical across dev, tests,
// Compose, and Helm deployments.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	dbfs "github.com/astraclawteam/agentatlas/services/agentatlas/db"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

// NewPool opens a pgx connection pool and verifies connectivity (fail
// loud). tlsMgr configures the PostgreSQL link's transport security
// (services/agentatlas/internal/transportsecurity), layered on top of
// whatever sslmode the dsn itself already carries; nil, or a Manager built
// with LinkConfig.Mode == ModeOff, keeps today's dsn-only behavior
// (typically sslmode=disable for local/dev, unchanged).
func NewPool(ctx context.Context, dsn string, tlsMgr *transportsecurity.Manager) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres pool config: %w", err)
	}
	if tlsMgr != nil {
		tlsCfg, err := tlsMgr.ClientTLSConfigOrNil()
		if err != nil {
			return nil, fmt.Errorf("postgres tls: %w", err)
		}
		if tlsCfg != nil {
			poolCfg.ConnConfig.TLSConfig = tlsCfg
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all embedded goose migrations, serialized against every other
// process doing the same thing.
//
// Four binaries call this at startup — cmd/atlas-agent, cmd/atlas-api,
// cmd/atlas-outcome-projector and cmd/atlas-worker — and `docker compose up`
// starts them together against one database. On a first deploy that database is
// empty, and the goose global API this used to call (goose.UpContext) let all
// four race each other into creating the version table. Reproduced against an
// empty database with four concurrent processes, three of the four died:
//
//	goose up: ERROR: relation "goose_db_version" does not exist (SQLSTATE 42P01);
//	ERROR: duplicate key value violates unique constraint "pg_class_relname_nsp_index" (SQLSTATE 23505)
//
// The provider API below closes both halves of that, and both halves are
// needed. WithSessionLocker takes a PostgreSQL advisory lock so only one process
// applies migrations at a time. That alone is not sufficient: goose's own
// pre-flight pending check deliberately does NOT take the session lock, so the
// version table can still be created concurrently — the provider answers that
// with a bounded retry around table creation, which the legacy global path does
// not have. Losing the race is now a wait, not an exit.
//
// tests/integration/migrate_concurrent_test.go is the reproduction, and it
// fails against the previous implementation.
func Migrate(ctx context.Context, dsn string) error {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer sqldb.Close()

	// The provider takes an FS rooted AT the migrations, where the global API
	// took a base FS plus a directory name.
	migrations, err := fs.Sub(dbfs.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("goose migrations fs: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("goose session locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqldb, migrations,
		goose.WithSessionLocker(locker),
		// Verbose keeps the per-migration and "successfully migrated database"
		// lines the global API printed; they are how an operator reads a
		// startup log.
		goose.WithVerbose(true),
	)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}
