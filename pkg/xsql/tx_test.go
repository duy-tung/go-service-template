package xsql

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, mock
}

func expectationsMet(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetExecutorReturnsPoolWithoutTransaction(t *testing.T) {
	db, mock := newMockDB(t)
	if got := GetExecutor(context.Background(), db); got != SQLExecutor(db) {
		t.Fatalf("GetExecutor without transaction = %T, want the *sql.DB", got)
	}
	expectationsMet(t, mock)
}

func TestExecInTxCommitsOnSuccess(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE thing").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := ExecInTx(context.Background(), db, func(ctx context.Context) error {
		exec := GetExecutor(ctx, db)
		if _, ok := exec.(*sql.Tx); !ok {
			t.Fatalf("executor inside ExecInTx = %T, want *sql.Tx", exec)
		}
		_, err := exec.ExecContext(ctx, "UPDATE thing")
		return err
	})
	if err != nil {
		t.Fatalf("ExecInTx: %v", err)
	}
	expectationsMet(t, mock)
}

func TestExecInTxRejectsNilDBAndNilFn(t *testing.T) {
	db, _ := newMockDB(t)
	if err := ExecInTx(context.Background(), nil, func(context.Context) error { return nil }); err == nil {
		t.Error("ExecInTx with nil db: want error, got nil")
	}
	if err := ExecInTx(context.Background(), db, nil); err == nil {
		t.Error("ExecInTx with nil fn: want error, got nil")
	}
}

func TestExecInTxRollsBackOnCallbackError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	sentinel := errors.New("business failure")
	err := ExecInTx(context.Background(), db, func(context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("ExecInTx error = %v, want unwrappable to sentinel", err)
	}
	expectationsMet(t, mock)
}

func TestExecInTxReturnsBeginError(t *testing.T) {
	db, mock := newMockDB(t)
	beginErr := errors.New("begin exploded")
	mock.ExpectBegin().WillReturnError(beginErr)

	err := ExecInTx(context.Background(), db, func(context.Context) error { return nil })
	if !errors.Is(err, beginErr) {
		t.Fatalf("ExecInTx error = %v, want unwrappable to begin error", err)
	}
	expectationsMet(t, mock)
}

func TestExecInTxReturnsCommitError(t *testing.T) {
	db, mock := newMockDB(t)
	commitErr := errors.New("commit exploded")
	mock.ExpectBegin()
	mock.ExpectCommit().WillReturnError(commitErr)

	err := ExecInTx(context.Background(), db, func(context.Context) error { return nil })
	if !errors.Is(err, commitErr) {
		t.Fatalf("ExecInTx error = %v, want unwrappable to commit error", err)
	}
	expectationsMet(t, mock)
}

func TestExecInTxKeepsOriginalAndRollbackErrors(t *testing.T) {
	db, mock := newMockDB(t)
	rollbackErr := errors.New("rollback exploded")
	original := errors.New("business failure")
	mock.ExpectBegin()
	mock.ExpectRollback().WillReturnError(rollbackErr)

	err := ExecInTx(context.Background(), db, func(context.Context) error { return original })
	if !errors.Is(err, original) {
		t.Errorf("ExecInTx error = %v, want unwrappable to original error", err)
	}
	if !errors.Is(err, rollbackErr) {
		t.Errorf("ExecInTx error = %v, want unwrappable to rollback error", err)
	}
	expectationsMet(t, mock)
}

func TestExecInTxRollsBackAndRepanics(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	defer func() {
		recovered := recover()
		if recovered != "boom" {
			t.Errorf("recovered %v, want the original panic value", recovered)
		}
		expectationsMet(t, mock)
	}()
	_ = ExecInTx(context.Background(), db, func(context.Context) error { panic("boom") })
	t.Fatal("ExecInTx must re-panic")
}

// TestExecInTxRollsBackOnGoexit pins the property that makes the
// completed-flag defer stronger than a recover()-based one: when fn exits
// via runtime.Goexit (t.Fatal in a test callback, for example), recover()
// returns nil, but the transaction must still be released.
func TestExecInTxRollsBackOnGoexit(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ExecInTx(context.Background(), db, func(context.Context) error {
			runtime.Goexit()
			return nil
		})
		t.Error("unreachable: Goexit must terminate the goroutine")
	}()
	<-done
	expectationsMet(t, mock)
}

func TestExecInTxJoinsExistingSamePoolTransaction(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE outer_op").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE inner_op").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := ExecInTx(context.Background(), db, func(outerCtx context.Context) error {
		if _, err := GetExecutor(outerCtx, db).ExecContext(outerCtx, "UPDATE outer_op"); err != nil {
			return err
		}
		return ExecInTx(outerCtx, db, func(innerCtx context.Context) error {
			_, err := GetExecutor(innerCtx, db).ExecContext(innerCtx, "UPDATE inner_op")
			return err
		})
	})
	if err != nil {
		t.Fatalf("ExecInTx: %v", err)
	}
	expectationsMet(t, mock)
}

func TestExecInTxInnerErrorRollsBackJoinedTransaction(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	sentinel := errors.New("inner failure")
	err := ExecInTx(context.Background(), db, func(outerCtx context.Context) error {
		// Propagating the inner error is what makes the outer scope roll back.
		return ExecInTx(outerCtx, db, func(context.Context) error { return sentinel })
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ExecInTx error = %v, want unwrappable to inner sentinel", err)
	}
	expectationsMet(t, mock)
}

func TestExecInTxDifferentPoolGetsOwnTransaction(t *testing.T) {
	dbA, mockA := newMockDB(t)
	dbB, mockB := newMockDB(t)
	mockA.ExpectBegin()
	mockA.ExpectCommit()
	mockB.ExpectBegin()
	mockB.ExpectCommit()

	err := ExecInTx(context.Background(), dbA, func(ctxA context.Context) error {
		// Pool B must not see pool A's transaction.
		if got := GetExecutor(ctxA, dbB); got != SQLExecutor(dbB) {
			t.Errorf("GetExecutor for pool B inside pool A tx = %T, want the pool B *sql.DB", got)
		}
		return ExecInTx(ctxA, dbB, func(ctxB context.Context) error {
			if _, ok := GetExecutor(ctxB, dbB).(*sql.Tx); !ok {
				t.Error("pool B callback did not receive its own *sql.Tx")
			}
			if _, ok := GetExecutor(ctxB, dbA).(*sql.Tx); !ok {
				t.Error("pool A transaction lost from nested context")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("ExecInTx: %v", err)
	}
	expectationsMet(t, mockA)
	expectationsMet(t, mockB)
}

func TestExecInTxCanceledContextFailsBegin(t *testing.T) {
	db, _ := newMockDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ExecInTx(ctx, db, func(context.Context) error {
		t.Error("fn must not run when the context is already dead")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecInTx error = %v, want unwrappable to context.Canceled", err)
	}
}

// TestExecInTxCanceledContextSkipsJoinedWork covers the join-existing path:
// even with a live transaction in the context, a dead request context must
// fail fast instead of running fn.
func TestExecInTxCanceledContextSkipsJoinedWork(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	outerErr := ExecInTx(context.Background(), db, func(txCtx context.Context) error {
		canceled, cancel := context.WithCancel(txCtx)
		cancel()
		if err := ExecInTx(canceled, db, func(context.Context) error {
			t.Error("joined fn must not run when the context is already dead")
			return nil
		}); !errors.Is(err, context.Canceled) {
			t.Errorf("joined ExecInTx error = %v, want unwrappable to context.Canceled", err)
		}
		return errors.New("roll the outer transaction back")
	})
	if outerErr == nil {
		t.Fatal("outer ExecInTx must propagate the callback error")
	}
	expectationsMet(t, mock)
}
