package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acme/order-engine/internal/domain"
	"github.com/acme/order-engine/internal/repository/postgres"
	"github.com/acme/order-engine/internal/testutil/testpg"
)

func newRepos(t *testing.T) (*sql.DB, *postgres.OrderRepository, *postgres.BalanceRepository, *postgres.SQLTransactor) {
	t.Helper()
	db := testpg.Open(t)
	orders, err := postgres.NewOrderRepository(db)
	if err != nil {
		t.Fatalf("NewOrderRepository: %v", err)
	}
	balances, err := postgres.NewBalanceRepository(db)
	if err != nil {
		t.Fatalf("NewBalanceRepository: %v", err)
	}
	transactor, err := postgres.NewSQLTransactor(db)
	if err != nil {
		t.Fatalf("NewSQLTransactor: %v", err)
	}
	return db, orders, balances, transactor
}

func sampleOrder(id, account, key string) *domain.Order {
	return &domain.Order{
		ID:             id,
		AccountID:      account,
		IdempotencyKey: key,
		AmountMinor:    250,
		Currency:       "USD",
		CreatedAt:      time.Date(2026, 8, 5, 9, 30, 0, 0, time.UTC),
	}
}

func TestDeductDistinguishesFailureModes(t *testing.T) {
	t.Parallel()
	db, _, balances, _ := newRepos(t)
	testpg.CreateAccount(t, db, "acct-1", "USD", 1000)
	ctx := context.Background()

	if err := balances.Deduct(ctx, "acct-1", 400, "USD"); err != nil {
		t.Fatalf("first deduct: %v", err)
	}
	if got := testpg.AccountBalance(t, db, "acct-1"); got != 600 {
		t.Fatalf("balance after deduct = %d, want 600", got)
	}

	if err := balances.Deduct(ctx, "acct-1", 700, "USD"); !errors.Is(err, domain.ErrInsufficientBalance) {
		t.Fatalf("over-deduct error = %v, want ErrInsufficientBalance", err)
	}
	if err := balances.Deduct(ctx, "acct-1", 100, "EUR"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("currency mismatch error = %v, want ErrInvalidArgument", err)
	}
	if err := balances.Deduct(ctx, "acct-missing", 100, "USD"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing account error = %v, want ErrNotFound", err)
	}
	if err := balances.Deduct(ctx, "acct-1", -5, "USD"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("negative amount error = %v, want ErrInvalidArgument", err)
	}
	if got := testpg.AccountBalance(t, db, "acct-1"); got != 600 {
		t.Fatalf("failed deductions must not change the balance: %d, want 600", got)
	}
}

func TestOrderRoundTripAndNotFoundMapping(t *testing.T) {
	t.Parallel()
	db, orders, _, _ := newRepos(t)
	testpg.CreateAccount(t, db, "acct-1", "USD", 1000)
	ctx := context.Background()

	if _, err := orders.FindByIdempotencyKey(ctx, "acct-1", "key-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("find on empty table = %v, want ErrNotFound", err)
	}
	if _, err := orders.FindByID(ctx, "order-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindByID on empty table = %v, want ErrNotFound", err)
	}

	want := sampleOrder("order-1", "acct-1", "key-1")
	if err := orders.Insert(ctx, want); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	byKey, err := orders.FindByIdempotencyKey(ctx, "acct-1", "key-1")
	if err != nil {
		t.Fatalf("FindByIdempotencyKey: %v", err)
	}
	if byKey.ID != want.ID || byKey.AmountMinor != want.AmountMinor ||
		byKey.Currency != want.Currency || !byKey.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("FindByIdempotencyKey = %+v, want %+v", byKey, want)
	}

	byID, err := orders.FindByID(ctx, "order-1")
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if byID.IdempotencyKey != "key-1" {
		t.Errorf("FindByID key = %q, want key-1", byID.IdempotencyKey)
	}
}

func TestInsertMapsNamedUniqueViolationToConflict(t *testing.T) {
	t.Parallel()
	db, orders, _, _ := newRepos(t)
	testpg.CreateAccount(t, db, "acct-1", "USD", 1000)
	ctx := context.Background()

	if err := orders.Insert(ctx, sampleOrder("order-1", "acct-1", "key-1")); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := orders.Insert(ctx, sampleOrder("order-2", "acct-1", "key-1"))
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("duplicate-key insert error = %v, want ErrIdempotencyConflict", err)
	}

	// A different unique violation (duplicate primary key) must NOT be
	// reported as an idempotency conflict.
	err = orders.Insert(ctx, sampleOrder("order-1", "acct-1", "key-other"))
	if err == nil || errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("duplicate-id insert error = %v, want a non-conflict failure", err)
	}
}

// TestWithinTransactionAtomicity is the definitive executor-propagation
// proof: deduct and insert issued through repositories inside one
// WithinTransaction either both commit or both roll back.
func TestWithinTransactionAtomicity(t *testing.T) {
	t.Parallel()
	db, orders, balances, transactor := newRepos(t)
	testpg.CreateAccount(t, db, "acct-1", "USD", 1000)
	ctx := context.Background()

	err := transactor.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := balances.Deduct(txCtx, "acct-1", 250, "USD"); err != nil {
			return err
		}
		if err := orders.Insert(txCtx, sampleOrder("order-commit", "acct-1", "key-commit")); err != nil {
			return err
		}
		// Reads through the same context observe uncommitted work...
		inTx, err := orders.FindByIdempotencyKey(txCtx, "acct-1", "key-commit")
		if err != nil {
			return err
		}
		if inTx.ID != "order-commit" {
			t.Errorf("in-tx read = %+v, want the uncommitted order", inTx)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("committing transaction: %v", err)
	}
	if got := testpg.AccountBalance(t, db, "acct-1"); got != 750 {
		t.Fatalf("balance after commit = %d, want 750", got)
	}
	if got := testpg.CountOrders(t, db, "acct-1"); got != 1 {
		t.Fatalf("orders after commit = %d, want 1", got)
	}

	boom := errors.New("forced failure after both writes")
	err = transactor.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := balances.Deduct(txCtx, "acct-1", 500, "USD"); err != nil {
			return err
		}
		if err := orders.Insert(txCtx, sampleOrder("order-rollback", "acct-1", "key-rollback")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("rolling-back transaction error = %v, want the forced failure", err)
	}
	if got := testpg.AccountBalance(t, db, "acct-1"); got != 750 {
		t.Fatalf("rollback must restore the deduction: balance = %d, want 750", got)
	}
	if got := testpg.CountOrders(t, db, "acct-1"); got != 1 {
		t.Fatalf("rollback must discard the insert: orders = %d, want 1", got)
	}
	if _, err := orders.FindByIdempotencyKey(ctx, "acct-1", "key-rollback"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rolled-back order lookup = %v, want ErrNotFound", err)
	}
}

// TestWithinTransactionCanceledMidFlight reproduces a client disconnect: the
// request context dies while the transaction is open, database/sql rolls the
// transaction back automatically, and the surfaced error must classify as
// context.Canceled — not as a rollback failure — so cancellation alerting
// stays meaningful.
func TestWithinTransactionCanceledMidFlight(t *testing.T) {
	t.Parallel()
	db, orders, _, transactor := newRepos(t)
	testpg.CreateAccount(t, db, "acct-1", "USD", 1000)

	ctx, cancel := context.WithCancel(context.Background())
	err := transactor.WithinTransaction(ctx, func(txCtx context.Context) error {
		if err := orders.Insert(txCtx, sampleOrder("order-cancel", "acct-1", "key-cancel")); err != nil {
			return err
		}
		cancel() // client disconnects mid-transaction
		time.Sleep(50 * time.Millisecond) // let database/sql's auto-rollback run
		if _, err := orders.FindByIdempotencyKey(txCtx, "acct-1", "key-cancel"); err != nil {
			return err
		}
		return errors.New("unreachable: statements on a canceled context must fail")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithinTransaction error = %v, want unwrappable to context.Canceled", err)
	}
	if strings.Contains(err.Error(), "rollback failed") {
		t.Errorf("error %q must not report the benign post-cancel ErrTxDone as a rollback failure", err)
	}
	if _, err := orders.FindByIdempotencyKey(context.Background(), "acct-1", "key-cancel"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("canceled transaction must be rolled back, lookup = %v, want ErrNotFound", err)
	}
}

// TestSchemaChecks pins the database-level guarantees the application
// relies on: positive amounts and non-negative balances.
func TestSchemaChecks(t *testing.T) {
	t.Parallel()
	db, orders, _, _ := newRepos(t)
	testpg.CreateAccount(t, db, "acct-1", "USD", 1000)
	ctx := context.Background()

	bad := sampleOrder("order-neg", "acct-1", "key-neg")
	bad.AmountMinor = 0
	if err := orders.Insert(ctx, bad); err == nil {
		t.Fatal("zero-amount insert must violate ck_orders_amount_positive")
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE "accounts" SET "balance_minor" = -1 WHERE "id" = $1`, "acct-1"); err == nil {
		t.Fatal("negative balance must violate ck_accounts_balance_non_negative")
	}
}
