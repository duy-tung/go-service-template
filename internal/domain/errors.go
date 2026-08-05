package domain

import "errors"

// Sentinel errors returned by repositories and use cases. Callers classify
// failures exclusively through errors.Is against these values.
var (
	// ErrNotFound reports that a referenced entity does not exist.
	ErrNotFound = errors.New("domain: not found")

	// ErrInvalidArgument reports client input that fails validation.
	ErrInvalidArgument = errors.New("domain: invalid argument")

	// ErrInsufficientBalance reports that an account balance cannot cover a
	// requested deduction.
	ErrInsufficientBalance = errors.New("domain: insufficient balance")

	// ErrIdempotencyConflict reports that an idempotency key was reused with
	// a different payload.
	ErrIdempotencyConflict = errors.New("domain: idempotency conflict")
)
