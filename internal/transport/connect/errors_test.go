package connecttransport

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/acme/order-engine/internal/domain"
)

func errorInfoFrom(t *testing.T, connectErr *connect.Error) *errdetails.ErrorInfo {
	t.Helper()
	for _, detail := range connectErr.Details() {
		msg, err := detail.Value()
		if err != nil {
			t.Fatalf("decode detail: %v", err)
		}
		if info, ok := msg.(*errdetails.ErrorInfo); ok {
			return info
		}
	}
	return nil
}

func TestToConnectErrorMapsDomainSentinels(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   connect.Code
		reason string
	}{
		{"invalid argument", fmt.Errorf("ctx: %w", domain.ErrInvalidArgument), connect.CodeInvalidArgument, ReasonInvalidArgument},
		{"not found", fmt.Errorf("ctx: %w", domain.ErrNotFound), connect.CodeNotFound, ReasonNotFound},
		{"insufficient balance", fmt.Errorf("ctx: %w", domain.ErrInsufficientBalance), connect.CodeFailedPrecondition, ReasonInsufficientBalance},
		{"idempotency conflict", fmt.Errorf("ctx: %w", domain.ErrIdempotencyConflict), connect.CodeAlreadyExists, ReasonIdempotencyConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connectErr := toConnectError(tc.err)
			if connectErr.Code() != tc.code {
				t.Fatalf("code = %v, want %v", connectErr.Code(), tc.code)
			}
			info := errorInfoFrom(t, connectErr)
			if info == nil {
				t.Fatal("domain error must carry an ErrorInfo detail")
			}
			if info.GetReason() != tc.reason || info.GetDomain() != ErrorInfoDomain {
				t.Errorf("ErrorInfo = %s/%s, want %s/%s",
					info.GetDomain(), info.GetReason(), ErrorInfoDomain, tc.reason)
			}
		})
	}
}

func TestToConnectErrorMapsContextErrors(t *testing.T) {
	if got := toConnectError(fmt.Errorf("op: %w", context.Canceled)); got.Code() != connect.CodeCanceled {
		t.Errorf("canceled code = %v, want CodeCanceled", got.Code())
	}
	if got := toConnectError(fmt.Errorf("op: %w", context.DeadlineExceeded)); got.Code() != connect.CodeDeadlineExceeded {
		t.Errorf("deadline code = %v, want CodeDeadlineExceeded", got.Code())
	}
}

func TestToConnectErrorSanitizesUnknownErrors(t *testing.T) {
	leaky := errors.New(`connect to "postgres://user:secret@db:5432/orders": connection refused`)
	connectErr := toConnectError(fmt.Errorf("query: %w", leaky))
	if connectErr.Code() != connect.CodeInternal {
		t.Fatalf("code = %v, want CodeInternal", connectErr.Code())
	}
	if connectErr.Message() != "internal error" {
		t.Fatalf("message = %q: internal details must never reach clients", connectErr.Message())
	}
	if len(connectErr.Details()) != 0 {
		t.Fatalf("internal errors must carry no details, got %d", len(connectErr.Details()))
	}
}

func TestToConnectErrorPassesThroughConnectErrors(t *testing.T) {
	original := connect.NewError(connect.CodeUnavailable, errors.New("try again"))
	got := toConnectError(fmt.Errorf("wrapped: %w", original))
	if got.Code() != connect.CodeUnavailable {
		t.Fatalf("code = %v, want the original CodeUnavailable", got.Code())
	}
}

func TestNewHealthCheckerStartsReadinessNotServing(t *testing.T) {
	checker := NewHealthChecker()

	live, err := checker.Check(context.Background(), &grpchealth.CheckRequest{Service: HealthServiceLiveness})
	if err != nil || live.Status != grpchealth.StatusServing {
		t.Fatalf("liveness = %v (err %v), want SERVING at startup", live, err)
	}
	ready, err := checker.Check(context.Background(), &grpchealth.CheckRequest{Service: HealthServiceReadiness})
	if err != nil || ready.Status != grpchealth.StatusNotServing {
		t.Fatalf("readiness = %v (err %v), want NOT_SERVING before the first successful ping", ready, err)
	}
}
