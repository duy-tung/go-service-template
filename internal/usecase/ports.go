// Package usecase implements the application's business operations against
// interfaces it owns. It depends on the domain only: no database/sql, no
// driver, no transport.
package usecase

import (
	"context"

	"github.com/acme/order-engine/internal/domain"
)

// OrderRepository persists and loads orders.
type OrderRepository interface {
	// FindByIdempotencyKey returns the order created for the given
	// (account, idempotency key) pair, or an error wrapping
	// domain.ErrNotFound when none exists.
	FindByIdempotencyKey(ctx context.Context, accountID, idempotencyKey string) (*domain.Order, error)

	// Insert persists a new order. Reusing an (account, idempotency key)
	// pair returns an error wrapping domain.ErrIdempotencyConflict.
	Insert(ctx context.Context, order *domain.Order) error
}

// BalanceRepository mutates account balances.
type BalanceRepository interface {
	// Deduct atomically subtracts amountMinor from the account's balance.
	// It fails with domain.ErrNotFound when the account does not exist,
	// domain.ErrInvalidArgument on a currency mismatch, and
	// domain.ErrInsufficientBalance when the balance cannot cover the
	// amount; a balance never goes negative.
	Deduct(ctx context.Context, accountID string, amountMinor int64, currency string) error
}

// Transactor runs a function within a database transaction. The context
// passed to fn carries the transaction and must not escape it.
type Transactor interface {
	WithinTransaction(ctx context.Context, fn func(context.Context) error) error
}
