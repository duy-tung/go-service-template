// Package postgres adapts the use case ports to PostgreSQL using
// database/sql, pkg/xsql and pkg/dataservicex.
package postgres

import (
	"time"

	"github.com/acme/order-engine/internal/domain"
)

// orderRow is the persistence shape of domain.Order. It lives entirely in
// this adapter: the domain never sees db tags or table names.
type orderRow struct {
	ID             string    `db:"id"`
	AccountID      string    `db:"account_id"`
	IdempotencyKey string    `db:"idempotency_key"`
	AmountMinor    int64     `db:"amount_minor"`
	Currency       string    `db:"currency"`
	CreatedAt      time.Time `db:"created_at"`
}

func (orderRow) TableName() string { return "orders" }
func (orderRow) IDColumn() string  { return "id" }

func orderRowFromDomain(o *domain.Order) *orderRow {
	return &orderRow{
		ID:             o.ID,
		AccountID:      o.AccountID,
		IdempotencyKey: o.IdempotencyKey,
		AmountMinor:    o.AmountMinor,
		Currency:       o.Currency,
		CreatedAt:      o.CreatedAt,
	}
}

func (r *orderRow) toDomain() *domain.Order {
	return &domain.Order{
		ID:             r.ID,
		AccountID:      r.AccountID,
		IdempotencyKey: r.IdempotencyKey,
		AmountMinor:    r.AmountMinor,
		Currency:       r.Currency,
		CreatedAt:      r.CreatedAt,
	}
}

// accountStateRow is the projection used to classify failed deductions.
// Only the currency is needed: the balance itself never decides the
// classification (a failed guarded UPDATE with a matching currency is
// insufficient funds by elimination).
type accountStateRow struct {
	Currency string `db:"currency"`
}
