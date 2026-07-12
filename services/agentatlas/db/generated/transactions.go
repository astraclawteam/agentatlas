package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type txOptionsBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// InTransaction runs generated queries on one PostgreSQL transaction.
func (q *Queries) InTransaction(ctx context.Context, fn func(*Queries) error) error {
	beginner, ok := q.db.(txBeginner)
	if !ok {
		return fmt.Errorf("generated query store cannot begin PostgreSQL transaction")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// InTransactionWithOptions runs generated queries on one PostgreSQL
// transaction using the requested isolation and access modes.
func (q *Queries) InTransactionWithOptions(ctx context.Context, options pgx.TxOptions, fn func(*Queries) error) error {
	beginner, ok := q.db.(txOptionsBeginner)
	if !ok {
		return fmt.Errorf("generated query store cannot begin PostgreSQL transaction with options")
	}
	tx, err := beginner.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
