package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acme/order-engine/internal/domain"
	"github.com/acme/order-engine/internal/testutil/testpg"
	"github.com/acme/order-engine/internal/usecase"
)

// barrierRepo holds both racing transactions at the point where each has
// observed "no existing order" inside its own transaction, guaranteeing the
// unique-constraint race actually happens instead of one goroutine taking
// the fast idempotent-hit path.
type barrierRepo struct {
	usecase.OrderRepository
	t       *testing.T
	barrier chan struct{}
	arrived atomic.Int32
}

func (b *barrierRepo) FindByIdempotencyKey(ctx context.Context, accountID, key string) (*domain.Order, error) {
	order, err := b.OrderRepository.FindByIdempotencyKey(ctx, accountID, key)
	if errors.Is(err, domain.ErrNotFound) {
		if b.arrived.Add(1) == 2 {
			close(b.barrier)
		}
		select {
		case <-b.barrier:
		case <-time.After(10 * time.Second):
			b.t.Error("barrier timed out: both goroutines must reach the in-tx lookup")
		}
	}
	return order, err
}

// TestConcurrentSameKeyDeductsOnce: two synchronized requests race on one
// idempotency key. PostgreSQL serializes them on the account row lock; the
// loser's insert hits the named unique constraint, its whole transaction
// (deduction included) rolls back, and the post-rollback re-read returns the
// winner. Both callers therefore succeed with the same order ID while the
// balance is deducted exactly once.
func TestConcurrentSameKeyDeductsOnce(t *testing.T) {
	t.Parallel()
	var barrier *barrierRepo
	st := newStack(t, func(real usecase.OrderRepository) usecase.OrderRepository {
		barrier = &barrierRepo{OrderRepository: real, t: t, barrier: make(chan struct{})}
		return barrier
	})
	testpg.CreateAccount(t, st.db, testAccount, "USD", 100_000)

	type outcome struct {
		order *domain.Order
		err   error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			order, err := st.placeOrder.Execute(ctx, testAccount, "race-key", 500, "USD")
			results <- outcome{order: order, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var ids []string
	for res := range results {
		if res.err != nil {
			t.Fatalf("concurrent Execute failed: %v", res.err)
		}
		ids = append(ids, res.order.ID)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Errorf("order IDs = %v, want both callers to receive the same order", ids)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 1 {
		t.Errorf("orders = %d, want 1", got)
	}
	if got := testpg.AccountBalance(t, st.db, testAccount); got != 100_000-500 {
		t.Errorf("balance = %d, want %d (deducted exactly once)", got, 100_000-500)
	}
}

// TestConcurrentSameKeyInsufficientForBoth: the deduct-side variant of the
// race. The balance covers the amount once but not twice, so the loser's
// guarded UPDATE re-evaluates against the winner's committed balance and
// fails with insufficient funds instead of hitting the unique constraint.
// The recovery re-read must still converge both callers on the winning
// order: same ID, one order row, one deduction.
func TestConcurrentSameKeyInsufficientForBoth(t *testing.T) {
	t.Parallel()
	var barrier *barrierRepo
	st := newStack(t, func(real usecase.OrderRepository) usecase.OrderRepository {
		barrier = &barrierRepo{OrderRepository: real, t: t, barrier: make(chan struct{})}
		return barrier
	})
	testpg.CreateAccount(t, st.db, testAccount, "USD", 600)

	type outcome struct {
		order *domain.Order
		err   error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			order, err := st.placeOrder.Execute(ctx, testAccount, "tight-balance-race", 500, "USD")
			results <- outcome{order: order, err: err}
		}()
	}
	wg.Wait()
	close(results)

	var ids []string
	for res := range results {
		if res.err != nil {
			t.Fatalf("concurrent Execute failed: %v", res.err)
		}
		ids = append(ids, res.order.ID)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Errorf("order IDs = %v, want both callers to receive the same order", ids)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 1 {
		t.Errorf("orders = %d, want 1", got)
	}
	if got := testpg.AccountBalance(t, st.db, testAccount); got != 100 {
		t.Errorf("balance = %d, want 100 (deducted exactly once)", got)
	}
}

// TestConcurrentSameKeyDifferentPayload: same race, conflicting payloads.
// Exactly one request wins; the other must surface ErrIdempotencyConflict,
// and only the winner's amount is deducted.
func TestConcurrentSameKeyDifferentPayload(t *testing.T) {
	t.Parallel()
	var barrier *barrierRepo
	st := newStack(t, func(real usecase.OrderRepository) usecase.OrderRepository {
		barrier = &barrierRepo{OrderRepository: real, t: t, barrier: make(chan struct{})}
		return barrier
	})
	testpg.CreateAccount(t, st.db, testAccount, "USD", 100_000)

	type outcome struct {
		amount int64
		order  *domain.Order
		err    error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, amount := range []int64{100, 200} {
		wg.Add(1)
		go func(amount int64) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			order, err := st.placeOrder.Execute(ctx, testAccount, "conflict-race", amount, "USD")
			results <- outcome{amount: amount, order: order, err: err}
		}(amount)
	}
	wg.Wait()
	close(results)

	var winners, conflicts int
	var winnerAmount int64
	for res := range results {
		switch {
		case res.err == nil:
			winners++
			winnerAmount = res.order.AmountMinor
		case errors.Is(res.err, domain.ErrIdempotencyConflict):
			conflicts++
		default:
			t.Fatalf("unexpected failure: %v", res.err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d, want exactly one of each", winners, conflicts)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 1 {
		t.Errorf("orders = %d, want 1", got)
	}
	if got := testpg.AccountBalance(t, st.db, testAccount); got != 100_000-winnerAmount {
		t.Errorf("balance = %d, want %d (only the winner deducts)", got, 100_000-winnerAmount)
	}
}
