package xsql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// txKey carries the owning pool in the key itself, so a transaction begun on
// one *sql.DB is never handed to a repository bound to a different pool: the
// lookup for pool B simply misses the entry stored for pool A.
type txKey struct{ db *sql.DB }

// GetExecutor returns the transaction bound to db carried by ctx, if any,
// and db itself otherwise.
func GetExecutor(ctx context.Context, db *sql.DB) SQLExecutor {
	if tx, ok := ctx.Value(txKey{db: db}).(*sql.Tx); ok {
		return tx
	}
	return db
}

// ExecInTx runs fn within a transaction on db using join-existing (REQUIRED)
// semantics:
//
//   - If ctx already carries a transaction for the same pool, fn joins it.
//     The inner scope must not begin, commit, or roll back; it reports its
//     outcome through fn's error, which the caller must propagate for the
//     outer scope to roll back. This is not savepoint nesting.
//   - Otherwise a new transaction is begun and made available to fn via the
//     child context. fn returning nil commits (commit errors are returned);
//     fn returning an error rolls back, keeping the original error
//     unwrappable; a panic in fn rolls back and re-panics.
//
// The transaction-carrying context must not escape fn, and a single
// transaction must not be shared by concurrent business operations.
func ExecInTx(ctx context.Context, db *sql.DB, fn func(context.Context) error) error {
	if db == nil {
		return errors.New("xsql: ExecInTx requires a non-nil db")
	}
	if fn == nil {
		return errors.New("xsql: ExecInTx requires a non-nil fn")
	}
	// Fail fast before touching the pool or running fn: a request whose
	// context is already dead should not consume a connection (or join an
	// existing transaction) just to fail on its first operation.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("xsql: context done before transaction work: %w", err)
	}
	if _, ok := ctx.Value(txKey{db: db}).(*sql.Tx); ok {
		return fn(ctx)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("xsql: begin transaction: %w", err)
	}

	completed := false
	defer func() {
		if !completed {
			// fn escaped without completing: release the transaction, then
			// let the unwinding continue unchanged. The completed-flag form
			// is deliberately stronger than recover()+panic(p): it never
			// touches the panic (value and stack stay pristine), and it also
			// rolls back on runtime.Goexit — e.g. t.Fatal inside a test
			// callback — where recover() returns nil and would leak the
			// transaction.
			_ = tx.Rollback()
		}
	}()

	if err := fn(context.WithValue(ctx, txKey{db: db}, tx)); err != nil {
		completed = true
		// ErrTxDone means database/sql already resolved the transaction —
		// typically its automatic rollback after the BeginTx context was
		// canceled (client disconnect, deadline). That is a completed
		// rollback, not a failed one; only genuine failures escalate.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			// Surface both failures; errors.Is/As keep working for each.
			return fmt.Errorf("xsql: rollback failed: %w (while handling: %w)", rbErr, err)
		}
		return err
	}
	completed = true

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("xsql: commit transaction: %w", err)
	}
	return nil
}
