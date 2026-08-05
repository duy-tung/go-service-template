package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/acme/order-engine/internal/domain"
)

// idempotencyKeyPattern bounds the key to a finite length and charset before
// it ever reaches a query or an index.
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// PlaceOrder is the order-creation use case.
type PlaceOrder struct {
	orders     OrderRepository
	balances   BalanceRepository
	transactor Transactor
	currencies map[string]struct{}
	now        func() time.Time
	newID      func() (string, error)
}

// PlaceOrderOption customizes a PlaceOrder use case; used by tests to pin
// clocks and IDs.
type PlaceOrderOption func(*PlaceOrder)

// WithClock overrides the timestamp source.
func WithClock(now func() time.Time) PlaceOrderOption {
	return func(p *PlaceOrder) { p.now = now }
}

// WithIDGenerator overrides the order-ID source.
func WithIDGenerator(newID func() (string, error)) PlaceOrderOption {
	return func(p *PlaceOrder) { p.newID = newID }
}

// WithSupportedCurrencies replaces the accepted currency set.
func WithSupportedCurrencies(currencies ...string) PlaceOrderOption {
	return func(p *PlaceOrder) {
		p.currencies = make(map[string]struct{}, len(currencies))
		for _, c := range currencies {
			p.currencies[c] = struct{}{}
		}
	}
}

// NewPlaceOrder wires the use case with its dependencies.
func NewPlaceOrder(orders OrderRepository, balances BalanceRepository, transactor Transactor, opts ...PlaceOrderOption) (*PlaceOrder, error) {
	if orders == nil || balances == nil || transactor == nil {
		return nil, errors.New("usecase: NewPlaceOrder requires orders, balances and transactor")
	}
	p := &PlaceOrder{
		orders:     orders,
		balances:   balances,
		transactor: transactor,
		currencies: map[string]struct{}{"USD": {}, "EUR": {}, "VND": {}},
		now:        func() time.Time { return time.Now().UTC() },
		newID:      randomUUIDv4,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// Execute places an order for the authenticated account. accountID comes from
// the caller's verified identity, never from the request body. The operation
// is idempotent per (accountID, idempotencyKey): a retry with an identical
// payload returns the original order without deducting again, while reusing
// the key with a different payload fails with domain.ErrIdempotencyConflict.
func (p *PlaceOrder) Execute(ctx context.Context, accountID, idempotencyKey string, amountMinor int64, currency string) (*domain.Order, error) {
	if accountID == "" {
		return nil, fmt.Errorf("%w: missing authenticated account id", domain.ErrInvalidArgument)
	}
	if !idempotencyKeyPattern.MatchString(idempotencyKey) {
		return nil, fmt.Errorf("%w: idempotency key must match %s", domain.ErrInvalidArgument, idempotencyKeyPattern)
	}
	if amountMinor <= 0 {
		return nil, fmt.Errorf("%w: amount_minor must be positive, got %d", domain.ErrInvalidArgument, amountMinor)
	}
	if _, ok := p.currencies[currency]; !ok {
		return nil, fmt.Errorf("%w: unsupported currency %q", domain.ErrInvalidArgument, currency)
	}

	var placed *domain.Order
	txErr := p.transactor.WithinTransaction(ctx, func(txCtx context.Context) error {
		existing, err := p.orders.FindByIdempotencyKey(txCtx, accountID, idempotencyKey)
		switch {
		case err == nil:
			if !existing.SamePayload(accountID, amountMinor, currency) {
				return fmt.Errorf("%w: key %q was already used with a different payload",
					domain.ErrIdempotencyConflict, idempotencyKey)
			}
			placed = existing
			return nil
		case errors.Is(err, domain.ErrNotFound):
			// No previous order: create one below.
		default:
			return err
		}

		if err := p.balances.Deduct(txCtx, accountID, amountMinor, currency); err != nil {
			return err
		}

		id, err := p.newID()
		if err != nil {
			return fmt.Errorf("usecase: generate order id: %w", err)
		}
		order := &domain.Order{
			ID:             id,
			AccountID:      accountID,
			IdempotencyKey: idempotencyKey,
			AmountMinor:    amountMinor,
			Currency:       currency,
			CreatedAt:      p.now(),
		}
		if err := p.orders.Insert(txCtx, order); err != nil {
			return err
		}
		placed = order
		return nil
	})
	if txErr == nil {
		return placed, nil
	}
	if !errors.Is(txErr, domain.ErrIdempotencyConflict) {
		return nil, txErr
	}

	// A conflict either means the key was reused with a different payload, or
	// this request lost an insert race and its whole transaction — including
	// the balance deduction — was rolled back. The transaction is gone, so
	// re-read the winning order on the plain pool context and decide.
	winner, err := p.orders.FindByIdempotencyKey(ctx, accountID, idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, txErr
		}
		return nil, fmt.Errorf("usecase: reload order after idempotency conflict: %w", err)
	}
	if winner.SamePayload(accountID, amountMinor, currency) {
		return winner, nil
	}
	return nil, fmt.Errorf("%w: key %q was already used with a different payload",
		domain.ErrIdempotencyConflict, idempotencyKey)
}

// randomUUIDv4 returns an RFC 4122 version-4 UUID built on crypto/rand,
// keeping the use case free of non-approved dependencies.
func randomUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:]), nil
}
