package connecttransport

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
)

// TestTimeoutInterceptorCancelsSlowHandlers pins the per-RPC bound: a
// handler that outlives the timeout must be canceled with DeadlineExceeded.
func TestTimeoutInterceptorCancelsSlowHandlers(t *testing.T) {
	interceptor := timeoutInterceptor(20 * time.Millisecond)
	slow := interceptor(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	start := time.Now()
	_, err := slow(context.Background(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow handler error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handler canceled after %s, the bound is not effective", elapsed)
	}
}

// TestTimeoutInterceptorKeepsShorterClientDeadline: context deadlines
// compose by minimum, so a tighter client deadline must survive the wrap.
func TestTimeoutInterceptorKeepsShorterClientDeadline(t *testing.T) {
	interceptor := timeoutInterceptor(time.Hour)
	inner := interceptor(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("handler context must carry a deadline")
		}
		if time.Until(deadline) > time.Minute {
			t.Errorf("deadline %s ignores the tighter client deadline", time.Until(deadline))
		}
		return nil, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := inner(ctx, nil); err != nil {
		t.Fatalf("handler: %v", err)
	}
}

// TestTimeoutInterceptorFastPathUnaffected: a prompt handler completes
// normally under the bound.
func TestTimeoutInterceptorFastPathUnaffected(t *testing.T) {
	interceptor := timeoutInterceptor(DefaultRequestTimeout)
	fast := interceptor(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if _, err := fast(context.Background(), nil); err != nil {
		t.Fatalf("fast handler: %v", err)
	}
}
