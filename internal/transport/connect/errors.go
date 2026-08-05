package connecttransport

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/acme/order-engine/internal/domain"
)

// ErrorInfoDomain is the stable domain identifier carried in ErrorInfo
// details for machine-readable classification.
const ErrorInfoDomain = "order-engine.acme.example"

// Reasons attached to domain failures, UPPER_SNAKE_CASE per AIP-193.
const (
	ReasonInvalidArgument     = "INVALID_ARGUMENT"
	ReasonNotFound            = "NOT_FOUND"
	ReasonInsufficientBalance = "INSUFFICIENT_BALANCE"
	ReasonIdempotencyConflict = "IDEMPOTENCY_KEY_CONFLICT"
)

// toConnectError classifies err with errors.Is/As and translates it into a
// *connect.Error. Domain failures keep their (safe, self-authored) message
// and gain an errdetails.ErrorInfo; anything unrecognized becomes a
// sanitized CodeInternal that leaks no SQL, DSNs, or driver details.
func toConnectError(err error) *connect.Error {
	var code connect.Code
	var reason string
	switch {
	case errors.Is(err, domain.ErrInvalidArgument):
		code, reason = connect.CodeInvalidArgument, ReasonInvalidArgument
	case errors.Is(err, domain.ErrNotFound):
		code, reason = connect.CodeNotFound, ReasonNotFound
	case errors.Is(err, domain.ErrInsufficientBalance):
		code, reason = connect.CodeFailedPrecondition, ReasonInsufficientBalance
	case errors.Is(err, domain.ErrIdempotencyConflict):
		code, reason = connect.CodeAlreadyExists, ReasonIdempotencyConflict
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("request canceled"))
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("request deadline exceeded"))
	default:
		var already *connect.Error
		if errors.As(err, &already) {
			return already
		}
		return connect.NewError(connect.CodeInternal, errors.New("internal error"))
	}

	connectErr := connect.NewError(code, err)
	detail, detailErr := connect.NewErrorDetail(&errdetails.ErrorInfo{
		Reason: reason,
		Domain: ErrorInfoDomain,
	})
	if detailErr != nil {
		// The classified error is still correct without its detail payload;
		// never drop it over a marshalling failure.
		return connectErr
	}
	connectErr.AddDetail(detail)
	return connectErr
}
