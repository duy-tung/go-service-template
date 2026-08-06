package connecttransport

import (
	"context"
	"time"

	"connectrpc.com/connect"
)

// DefaultRequestTimeout bounds a unary RPC when MuxConfig.RequestTimeout is
// unset. It caps handler + use case + database time; the bytes of the
// request message are read before interceptors run, so wire-level slowness
// is still the edge proxy's to bound (see the ReadTimeout design decision
// in cmd/api).
const DefaultRequestTimeout = 30 * time.Second

// timeoutInterceptor bounds each unary handler invocation. Streaming
// procedures added later are unaffected: they flow through WrapStreaming*,
// not the unary func. Client deadlines shorter than d still win — context
// deadlines compose by taking the minimum.
func timeoutInterceptor(d time.Duration) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, d)
			defer cancel()
			return next(ctx, req)
		}
	}
}
