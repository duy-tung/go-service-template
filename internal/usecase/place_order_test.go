package usecase

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acme/order-engine/internal/domain"
)

// txMarker marks contexts produced by the fake transactor, letting tests
// verify which repository calls ran inside the transaction scope.
type txMarker struct{}

func inTx(ctx context.Context) bool {
	v, _ := ctx.Value(txMarker{}).(bool)
	return v
}

type fakeTransactor struct {
	beginErr error
	calls    int
}

func (t *fakeTransactor) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	if t.beginErr != nil {
		return t.beginErr
	}
	t.calls++
	return fn(context.WithValue(ctx, txMarker{}, true))
}

type findCall struct {
	inTx bool
}

type fakeOrderRepo struct {
	find      func(call int, ctx context.Context, accountID, key string) (*domain.Order, error)
	insert    func(ctx context.Context, o *domain.Order) error
	findCalls []findCall
	inserted  []*domain.Order
}

func (r *fakeOrderRepo) FindByIdempotencyKey(ctx context.Context, accountID, key string) (*domain.Order, error) {
	r.findCalls = append(r.findCalls, findCall{inTx: inTx(ctx)})
	return r.find(len(r.findCalls), ctx, accountID, key)
}

func (r *fakeOrderRepo) Insert(ctx context.Context, o *domain.Order) error {
	if !inTx(ctx) {
		return errors.New("Insert called outside the transaction context")
	}
	if r.insert != nil {
		if err := r.insert(ctx, o); err != nil {
			return err
		}
	}
	r.inserted = append(r.inserted, o)
	return nil
}

type deductCall struct {
	accountID   string
	amountMinor int64
	currency    string
	inTx        bool
}

type fakeBalanceRepo struct {
	deduct func(ctx context.Context, accountID string, amountMinor int64, currency string) error
	calls  []deductCall
}

func (r *fakeBalanceRepo) Deduct(ctx context.Context, accountID string, amountMinor int64, currency string) error {
	r.calls = append(r.calls, deductCall{accountID, amountMinor, currency, inTx(ctx)})
	if r.deduct != nil {
		return r.deduct(ctx, accountID, amountMinor, currency)
	}
	return nil
}

func notFoundRepo() *fakeOrderRepo {
	return &fakeOrderRepo{
		find: func(int, context.Context, string, string) (*domain.Order, error) {
			return nil, fmt.Errorf("wrapped: %w", domain.ErrNotFound)
		},
	}
}

func newUsecase(t *testing.T, orders *fakeOrderRepo, balances *fakeBalanceRepo, tx *fakeTransactor) *PlaceOrder {
	t.Helper()
	fixed := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	p, err := NewPlaceOrder(orders, balances, tx,
		WithClock(func() time.Time { return fixed }),
		WithIDGenerator(func() (string, error) { return "order-fixed-id", nil }),
	)
	if err != nil {
		t.Fatalf("NewPlaceOrder: %v", err)
	}
	return p
}

func TestExecuteCreatesOrderAndDeductsOnce(t *testing.T) {
	orders := notFoundRepo()
	balances := &fakeBalanceRepo{}
	tx := &fakeTransactor{}
	p := newUsecase(t, orders, balances, tx)

	got, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := domain.Order{
		ID: "order-fixed-id", AccountID: "acct-1", IdempotencyKey: "key-1",
		AmountMinor: 2500, Currency: "USD",
		CreatedAt: time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
	}
	if *got != want {
		t.Errorf("Execute = %+v, want %+v", *got, want)
	}
	if len(balances.calls) != 1 || balances.calls[0] != (deductCall{"acct-1", 2500, "USD", true}) {
		t.Errorf("deduct calls = %+v, want one in-tx call for acct-1/2500/USD", balances.calls)
	}
	if len(orders.inserted) != 1 {
		t.Errorf("inserted %d orders, want 1", len(orders.inserted))
	}
	if tx.calls != 1 {
		t.Errorf("transactor ran %d times, want 1", tx.calls)
	}
}

func TestExecuteValidatesBeforeOpeningTransaction(t *testing.T) {
	cases := []struct {
		name           string
		accountID, key string
		amount         int64
		currency       string
	}{
		{"missing account", "", "key-1", 100, "USD"},
		{"empty key", "acct-1", "", 100, "USD"},
		{"key too long", "acct-1", strings.Repeat("k", 65), 100, "USD"},
		{"key bad charset", "acct-1", "key with spaces!", 100, "USD"},
		{"zero amount", "acct-1", "key-1", 0, "USD"},
		{"negative amount", "acct-1", "key-1", -5, "USD"},
		{"unsupported currency", "acct-1", "key-1", 100, "XXX"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &fakeTransactor{}
			p := newUsecase(t, notFoundRepo(), &fakeBalanceRepo{}, tx)
			_, err := p.Execute(context.Background(), tc.accountID, tc.key, tc.amount, tc.currency)
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("Execute error = %v, want ErrInvalidArgument", err)
			}
			if tx.calls != 0 {
				t.Errorf("validation failures must not open a transaction (ran %d)", tx.calls)
			}
		})
	}
}

func TestExecuteReturnsExistingOrderWithoutDeducting(t *testing.T) {
	existing := &domain.Order{
		ID: "order-old", AccountID: "acct-1", IdempotencyKey: "key-1",
		AmountMinor: 2500, Currency: "USD", CreatedAt: time.Now().UTC(),
	}
	orders := &fakeOrderRepo{
		find: func(int, context.Context, string, string) (*domain.Order, error) { return existing, nil },
	}
	balances := &fakeBalanceRepo{}
	p := newUsecase(t, orders, balances, &fakeTransactor{})

	got, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ID != "order-old" {
		t.Errorf("Execute returned %q, want the existing order", got.ID)
	}
	if len(balances.calls) != 0 {
		t.Errorf("idempotent replay must not deduct again: %+v", balances.calls)
	}
	if len(orders.inserted) != 0 {
		t.Errorf("idempotent replay must not insert: %+v", orders.inserted)
	}
}

func TestExecuteRejectsKeyReuseWithDifferentPayload(t *testing.T) {
	existing := &domain.Order{
		ID: "order-old", AccountID: "acct-1", IdempotencyKey: "key-1",
		AmountMinor: 2500, Currency: "USD",
	}
	orders := &fakeOrderRepo{
		find: func(int, context.Context, string, string) (*domain.Order, error) { return existing, nil },
	}
	balances := &fakeBalanceRepo{}
	p := newUsecase(t, orders, balances, &fakeTransactor{})

	_, err := p.Execute(context.Background(), "acct-1", "key-1", 9999, "USD")
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Execute error = %v, want ErrIdempotencyConflict", err)
	}
	if len(balances.calls) != 0 {
		t.Errorf("conflict must not deduct: %+v", balances.calls)
	}
}

func TestExecuteRecoversWinnerAfterLostInsertRace(t *testing.T) {
	winner := &domain.Order{
		ID: "order-winner", AccountID: "acct-1", IdempotencyKey: "key-1",
		AmountMinor: 2500, Currency: "USD",
	}
	orders := &fakeOrderRepo{
		find: func(call int, ctx context.Context, _, _ string) (*domain.Order, error) {
			if call == 1 {
				return nil, domain.ErrNotFound
			}
			return winner, nil
		},
		insert: func(context.Context, *domain.Order) error {
			return fmt.Errorf("repo: %w", domain.ErrIdempotencyConflict)
		},
	}
	balances := &fakeBalanceRepo{}
	p := newUsecase(t, orders, balances, &fakeTransactor{})

	got, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ID != "order-winner" {
		t.Errorf("Execute returned %q, want the winning order", got.ID)
	}
	if len(orders.findCalls) != 2 {
		t.Fatalf("find called %d times, want 2 (in-tx lookup + post-rollback re-read)", len(orders.findCalls))
	}
	if !orders.findCalls[0].inTx {
		t.Error("first lookup must run inside the transaction")
	}
	if orders.findCalls[1].inTx {
		t.Error("post-rollback re-read must run on the pool context, not the dead transaction")
	}
	if len(balances.calls) != 1 {
		t.Errorf("deduct calls = %d, want 1 (rolled back with the losing transaction)", len(balances.calls))
	}
}

func TestExecuteLostRaceWithDifferentPayloadConflicts(t *testing.T) {
	winner := &domain.Order{
		ID: "order-winner", AccountID: "acct-1", IdempotencyKey: "key-1",
		AmountMinor: 1, Currency: "USD",
	}
	orders := &fakeOrderRepo{
		find: func(call int, ctx context.Context, _, _ string) (*domain.Order, error) {
			if call == 1 {
				return nil, domain.ErrNotFound
			}
			return winner, nil
		},
		insert: func(context.Context, *domain.Order) error {
			return fmt.Errorf("repo: %w", domain.ErrIdempotencyConflict)
		},
	}
	p := newUsecase(t, orders, &fakeBalanceRepo{}, &fakeTransactor{})

	_, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Execute error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestExecuteReturnsConflictWhenWinnerVanishes(t *testing.T) {
	orders := &fakeOrderRepo{
		find: func(int, context.Context, string, string) (*domain.Order, error) {
			return nil, domain.ErrNotFound
		},
		insert: func(context.Context, *domain.Order) error {
			return fmt.Errorf("repo: %w", domain.ErrIdempotencyConflict)
		},
	}
	p := newUsecase(t, orders, &fakeBalanceRepo{}, &fakeTransactor{})

	_, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Execute error = %v, want the original conflict, got %v", domain.ErrIdempotencyConflict, err)
	}
}

// TestExecuteRecoversWinnerAfterLostDeductRace covers the sibling of the
// insert race: the loser's guarded deduction re-evaluates against the
// winner's committed balance, fails with ErrInsufficientBalance, and the
// post-rollback re-read must return the winning order instead of surfacing
// a business failure for an order that was actually placed.
func TestExecuteRecoversWinnerAfterLostDeductRace(t *testing.T) {
	winner := &domain.Order{
		ID: "order-winner", AccountID: "acct-1", IdempotencyKey: "key-1",
		AmountMinor: 2500, Currency: "USD",
	}
	orders := &fakeOrderRepo{
		find: func(call int, ctx context.Context, _, _ string) (*domain.Order, error) {
			if call == 1 {
				return nil, domain.ErrNotFound
			}
			return winner, nil
		},
	}
	balances := &fakeBalanceRepo{
		deduct: func(context.Context, string, int64, string) error {
			return fmt.Errorf("repo: %w", domain.ErrInsufficientBalance)
		},
	}
	p := newUsecase(t, orders, balances, &fakeTransactor{})

	got, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ID != "order-winner" {
		t.Errorf("Execute returned %q, want the winning order", got.ID)
	}
	if len(orders.findCalls) != 2 {
		t.Fatalf("find called %d times, want 2 (in-tx lookup + post-rollback re-read)", len(orders.findCalls))
	}
	if orders.findCalls[1].inTx {
		t.Error("post-rollback re-read must run on the pool context, not the dead transaction")
	}
	if len(orders.inserted) != 0 {
		t.Errorf("loser must not insert: %+v", orders.inserted)
	}
}

// TestExecuteLostDeductRaceDifferentPayloadConflicts: same deduct-side race,
// but the committed order carries a different payload — the request reused
// the key, so the conflict classification wins over insufficient balance.
func TestExecuteLostDeductRaceDifferentPayloadConflicts(t *testing.T) {
	winner := &domain.Order{
		ID: "order-winner", AccountID: "acct-1", IdempotencyKey: "key-1",
		AmountMinor: 1, Currency: "USD",
	}
	orders := &fakeOrderRepo{
		find: func(call int, ctx context.Context, _, _ string) (*domain.Order, error) {
			if call == 1 {
				return nil, domain.ErrNotFound
			}
			return winner, nil
		},
	}
	balances := &fakeBalanceRepo{
		deduct: func(context.Context, string, int64, string) error {
			return fmt.Errorf("repo: %w", domain.ErrInsufficientBalance)
		},
	}
	p := newUsecase(t, orders, balances, &fakeTransactor{})

	_, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("Execute error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestExecutePropagatesDeductFailures(t *testing.T) {
	for _, sentinel := range []error{domain.ErrInsufficientBalance, domain.ErrNotFound, domain.ErrInvalidArgument} {
		orders := notFoundRepo()
		balances := &fakeBalanceRepo{
			deduct: func(context.Context, string, int64, string) error {
				return fmt.Errorf("repo: %w", sentinel)
			},
		}
		p := newUsecase(t, orders, balances, &fakeTransactor{})

		_, err := p.Execute(context.Background(), "acct-1", "key-1", 2500, "USD")
		if !errors.Is(err, sentinel) {
			t.Errorf("Execute error = %v, want %v", err, sentinel)
		}
		if len(orders.inserted) != 0 {
			t.Errorf("failed deduct must not insert an order")
		}
	}
}

func TestExecutePropagatesInfrastructureErrors(t *testing.T) {
	infraErr := errors.New("connection reset")

	t.Run("transactor", func(t *testing.T) {
		p := newUsecase(t, notFoundRepo(), &fakeBalanceRepo{}, &fakeTransactor{beginErr: infraErr})
		if _, err := p.Execute(context.Background(), "acct-1", "key-1", 1, "USD"); !errors.Is(err, infraErr) {
			t.Errorf("Execute error = %v, want transactor error", err)
		}
	})

	t.Run("lookup", func(t *testing.T) {
		orders := &fakeOrderRepo{
			find: func(int, context.Context, string, string) (*domain.Order, error) { return nil, infraErr },
		}
		p := newUsecase(t, orders, &fakeBalanceRepo{}, &fakeTransactor{})
		if _, err := p.Execute(context.Background(), "acct-1", "key-1", 1, "USD"); !errors.Is(err, infraErr) {
			t.Errorf("Execute error = %v, want lookup error", err)
		}
	})

	t.Run("id generation", func(t *testing.T) {
		idErr := errors.New("entropy exhausted")
		p, err := NewPlaceOrder(notFoundRepo(), &fakeBalanceRepo{}, &fakeTransactor{},
			WithIDGenerator(func() (string, error) { return "", idErr }))
		if err != nil {
			t.Fatalf("NewPlaceOrder: %v", err)
		}
		if _, err := p.Execute(context.Background(), "acct-1", "key-1", 1, "USD"); !errors.Is(err, idErr) {
			t.Errorf("Execute error = %v, want id generator error", err)
		}
	})
}

func TestNewPlaceOrderRejectsNilDependencies(t *testing.T) {
	if _, err := NewPlaceOrder(nil, &fakeBalanceRepo{}, &fakeTransactor{}); err == nil {
		t.Error("nil orders: want error")
	}
	if _, err := NewPlaceOrder(notFoundRepo(), nil, &fakeTransactor{}); err == nil {
		t.Error("nil balances: want error")
	}
	if _, err := NewPlaceOrder(notFoundRepo(), &fakeBalanceRepo{}, nil); err == nil {
		t.Error("nil transactor: want error")
	}
}

func TestWithSupportedCurrenciesReplacesSet(t *testing.T) {
	p, err := NewPlaceOrder(notFoundRepo(), &fakeBalanceRepo{}, &fakeTransactor{},
		WithSupportedCurrencies("GBP"))
	if err != nil {
		t.Fatalf("NewPlaceOrder: %v", err)
	}
	if _, err := p.Execute(context.Background(), "acct-1", "key-1", 1, "USD"); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("USD should be unsupported after override, got %v", err)
	}
	if _, err := p.Execute(context.Background(), "acct-1", "key-1", 1, "GBP"); err != nil {
		t.Errorf("GBP should be supported after override, got %v", err)
	}
}

func TestRandomUUIDv4Shape(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := make(map[string]struct{})
	for range 64 {
		id, err := randomUUIDv4()
		if err != nil {
			t.Fatalf("randomUUIDv4: %v", err)
		}
		if !pattern.MatchString(id) {
			t.Fatalf("randomUUIDv4 = %q, not a v4 UUID", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("randomUUIDv4 repeated %q", id)
		}
		seen[id] = struct{}{}
	}
}
