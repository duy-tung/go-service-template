package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/acme/order-engine/internal/domain"
	"github.com/acme/order-engine/pkg/xsql"
)

// deductQuery is a single atomic conditional update: the balance is only
// touched when the account exists, the currency matches, and the funds
// cover the amount, so a balance can never go negative.
const deductQuery = `UPDATE "accounts" SET "balance_minor" = "balance_minor" - $1 ` +
	`WHERE "id" = $2 AND "currency" = $3 AND "balance_minor" >= $1`

const accountStateQuery = `SELECT "currency" FROM "accounts" WHERE "id" = $1 LIMIT 1`

// BalanceRepository is the PostgreSQL implementation of
// usecase.BalanceRepository.
type BalanceRepository struct {
	db *sql.DB
}

// NewBalanceRepository wires the repository.
func NewBalanceRepository(db *sql.DB) (*BalanceRepository, error) {
	if db == nil {
		return nil, errors.New("postgres: NewBalanceRepository requires a non-nil db")
	}
	return &BalanceRepository{db: db}, nil
}

// Deduct implements usecase.BalanceRepository. When the guarded update
// matches no row, a follow-up read distinguishes a missing account, a
// currency mismatch, and insufficient funds.
func (r *BalanceRepository) Deduct(ctx context.Context, accountID string, amountMinor int64, currency string) error {
	if amountMinor <= 0 {
		return fmt.Errorf("%w: deduction amount must be positive, got %d", domain.ErrInvalidArgument, amountMinor)
	}

	exec := xsql.GetExecutor(ctx, r.db)
	result, err := exec.ExecContext(ctx, deductQuery, amountMinor, accountID, currency)
	if err != nil {
		return fmt.Errorf("postgres: deduct balance: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: deduct balance: rows affected: %w", err)
	}
	if affected == 1 {
		return nil
	}

	state, err := xsql.QuerySingle[accountStateRow](ctx, exec, accountStateQuery, accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("postgres: account %q: %w", accountID, domain.ErrNotFound)
		}
		return fmt.Errorf("postgres: classify failed deduction: %w", err)
	}
	if state.Currency != currency {
		return fmt.Errorf("%w: account currency is %s, not %s", domain.ErrInvalidArgument, state.Currency, currency)
	}
	return fmt.Errorf("%w: balance cannot cover %d minor units", domain.ErrInsufficientBalance, amountMinor)
}
