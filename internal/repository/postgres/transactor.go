package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/acme/order-engine/pkg/xsql"
)

// SQLTransactor implements usecase.Transactor on top of xsql.ExecInTx, so
// every repository resolving its executor through xsql.GetExecutor joins the
// same transaction.
type SQLTransactor struct {
	db *sql.DB
}

// NewSQLTransactor wires the transactor.
func NewSQLTransactor(db *sql.DB) (*SQLTransactor, error) {
	if db == nil {
		return nil, errors.New("postgres: NewSQLTransactor requires a non-nil db")
	}
	return &SQLTransactor{db: db}, nil
}

// WithinTransaction implements usecase.Transactor.
func (t *SQLTransactor) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	return xsql.ExecInTx(ctx, t.db, fn)
}
