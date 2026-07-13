// Package storage wires PostgreSQL access: pgx pool for queries and embedded
// goose migrations so schema application is identical across dev, tests,
// Compose, and Helm deployments.
package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

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

// Migrate applies all embedded goose migrations.
func Migrate(ctx context.Context, dsn string) error {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer sqldb.Close()

	goose.SetBaseFS(dbfs.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, sqldb, "migrations"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}
