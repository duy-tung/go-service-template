// Package xsql provides small database/sql building blocks used by the
// repository layer: an executor abstraction shared by *sql.DB and *sql.Tx,
// context-based transaction propagation with pool identity, and a generic
// single-row query helper driven by `db` struct tags.
package xsql

import (
	"context"
	"database/sql"
)

// SQLExecutor is the subset of database/sql operations shared by *sql.DB and
// *sql.Tx. Code written against it runs unchanged inside and outside a
// transaction.
type SQLExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ SQLExecutor = (*sql.DB)(nil)
	_ SQLExecutor = (*sql.Tx)(nil)
)
