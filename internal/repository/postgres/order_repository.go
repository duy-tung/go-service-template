package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/acme/order-engine/internal/domain"
	"github.com/acme/order-engine/pkg/dataservicex"
	"github.com/acme/order-engine/pkg/xsql"
)

// UniqueIdempotencyConstraint is the migration-defined constraint enforcing
// one order per (account_id, idempotency_key). Insert matches this exact
// name when translating unique violations, so migration and code cannot
// drift silently: the integration suite triggers a real violation.
const UniqueIdempotencyConstraint = "uq_orders_account_idempotency_key"

const uniqueViolationCode = "23505"

const findByIdempotencyKeyQuery = `SELECT "id", "account_id", "idempotency_key", "amount_minor", "currency", "created_at" ` +
	`FROM "orders" WHERE "account_id" = $1 AND "idempotency_key" = $2 LIMIT 1`

// OrderRepository is the PostgreSQL implementation of usecase.OrderRepository.
// It composes the generic DataService for single-entity plumbing and adds the
// order-specific queries and error translation.
type OrderRepository struct {
	db     *sql.DB
	orders *dataservicex.DataService[orderRow]
}

// NewOrderRepository validates the entity metadata and wires the repository.
func NewOrderRepository(db *sql.DB) (*OrderRepository, error) {
	if db == nil {
		return nil, errors.New("postgres: NewOrderRepository requires a non-nil db")
	}
	orders, err := dataservicex.NewDataService[orderRow](db)
	if err != nil {
		return nil, fmt.Errorf("postgres: order data service: %w", err)
	}
	return &OrderRepository{db: db, orders: orders}, nil
}

// FindByIdempotencyKey implements usecase.OrderRepository.
func (r *OrderRepository) FindByIdempotencyKey(ctx context.Context, accountID, idempotencyKey string) (*domain.Order, error) {
	exec := xsql.GetExecutor(ctx, r.db)
	row, err := xsql.QuerySingle[orderRow](ctx, exec, findByIdempotencyKeyQuery, accountID, idempotencyKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("postgres: order for idempotency key: %w", domain.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: find order by idempotency key: %w", err)
	}
	return row.toDomain(), nil
}

// FindByID loads one order by primary key, translating the generic
// not-found error into the domain sentinel.
func (r *OrderRepository) FindByID(ctx context.Context, id string) (*domain.Order, error) {
	row, err := r.orders.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, dataservicex.ErrEntityNotFound) {
			return nil, fmt.Errorf("postgres: order %q: %w", id, domain.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: find order by id: %w", err)
	}
	return row.toDomain(), nil
}

// Insert implements usecase.OrderRepository. A violation of the named
// idempotency constraint becomes domain.ErrIdempotencyConflict.
func (r *OrderRepository) Insert(ctx context.Context, order *domain.Order) error {
	if order == nil {
		return errors.New("postgres: Insert requires a non-nil order")
	}
	if err := r.orders.Insert(ctx, orderRowFromDomain(order)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == uniqueViolationCode &&
			pgErr.ConstraintName == UniqueIdempotencyConstraint {
			return fmt.Errorf("postgres: insert order: %w", domain.ErrIdempotencyConflict)
		}
		return fmt.Errorf("postgres: insert order: %w", err)
	}
	return nil
}
