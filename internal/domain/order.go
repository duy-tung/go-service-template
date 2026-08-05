// Package domain holds the persistence-agnostic business model of the order
// engine. It has no knowledge of SQL, transports, or frameworks.
package domain

import "time"

// Order is an accepted order. Monetary amounts are integral minor units
// (e.g. cents) paired with an ISO 4217 currency code; floating point is
// never used for money.
type Order struct {
	ID             string
	AccountID      string
	IdempotencyKey string
	AmountMinor    int64
	Currency       string
	CreatedAt      time.Time
}

// SamePayload reports whether an incoming request carrying the same
// idempotency key is a retry of this order (identical client-controlled
// payload) rather than a conflicting reuse of the key. Every idempotency
// comparison must go through this single definition.
func (o *Order) SamePayload(accountID string, amountMinor int64, currency string) bool {
	return o.AccountID == accountID &&
		o.AmountMinor == amountMinor &&
		o.Currency == currency
}
