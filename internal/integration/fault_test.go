package integration

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/acme/order-engine/gen/order/v1/orderv1connect"
	"github.com/acme/order-engine/internal/grpcclient"
	"github.com/acme/order-engine/internal/testutil/testpg"
)

// fastRetryOpt shrinks only the backoff bounds; policy, retryable codes and
// throttling stay identical to the production service config.
func fastRetryOpt() grpc.DialOption {
	return grpc.WithDefaultServiceConfig(grpcclient.ServiceConfigWithBackoff("0.01s", "0.05s"))
}

// connKiller coordinates a one-shot connection kill with the proxy: the
// outermost server interceptor decides whether the handler runs first
// (response-lost mode) or not (request-lost mode), then severs the client
// connection while the RPC is still in flight, so no response bytes — not
// even headers — ever reach the client.
type connKiller struct {
	calls      atomic.Int32
	armed      atomic.Bool
	runHandler bool
	kill       func()
}

func (k *connKiller) interceptor() connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			k.calls.Add(1)
			if !k.armed.CompareAndSwap(true, false) {
				return next(ctx, req)
			}
			if k.runHandler {
				// The order commits inside next; only then is the connection
				// severed, before the response can be written.
				resp, err := next(ctx, req)
				if err != nil {
					return nil, err
				}
				k.kill()
				return resp, nil // written into the dead connection
			}
			k.kill()
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("request lost"))
		}
	})
}

// TestGRPCRetryAfterResponseLost is the response-lost drill: attempt one
// commits the order but its response never reaches the client (connection
// severed first). The grpc-go client retries the UNAVAILABLE failure within
// its deadline, and idempotency turns the retry into a read of the committed
// order — one order, one deduction.
func TestGRPCRetryAfterResponseLost(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	killer := &connKiller{runHandler: true}
	server := startServer(t, st, killer.interceptor())
	proxy := startKillerProxy(t, server.addr)
	killer.kill = proxy.KillConns
	killer.armed.Store(true)
	testpg.CreateAccount(t, st.db, testAccount, "USD", 10_000)

	conn, err := grpcclient.New(proxy.addr(), fastRetryOpt())
	if err != nil {
		t.Fatalf("grpcclient.New: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	order, err := grpcCreate(t, conn, testToken, "lost-response", 750, "USD")
	if err != nil {
		t.Fatalf("CreateOrder with retry after lost response: %v", err)
	}

	if got := killer.calls.Load(); got != 2 {
		t.Errorf("server saw %d attempts, want 2 (original + one retry)", got)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 1 {
		t.Errorf("orders = %d, want exactly 1 despite the retry", got)
	}
	if got := testpg.AccountBalance(t, st.db, testAccount); got != 10_000-750 {
		t.Errorf("balance = %d, want %d (deducted exactly once)", got, 10_000-750)
	}

	var storedID string
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT "id" FROM "orders" WHERE "account_id" = $1`, testAccount).Scan(&storedID); err != nil {
		t.Fatalf("read stored order: %v", err)
	}
	if order.GetId() != storedID {
		t.Errorf("returned order %s, want the committed row %s", order.GetId(), storedID)
	}
}

// TestGRPCRetryAfterRequestLost covers the sibling fault: the connection
// dies before the handler runs, so nothing was committed; the retry creates
// the order exactly once.
func TestGRPCRetryAfterRequestLost(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	killer := &connKiller{runHandler: false}
	server := startServer(t, st, killer.interceptor())
	proxy := startKillerProxy(t, server.addr)
	killer.kill = proxy.KillConns
	killer.armed.Store(true)
	testpg.CreateAccount(t, st.db, testAccount, "USD", 10_000)

	conn, err := grpcclient.New(proxy.addr(), fastRetryOpt())
	if err != nil {
		t.Fatalf("grpcclient.New: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := grpcCreate(t, conn, testToken, "lost-request", 300, "USD"); err != nil {
		t.Fatalf("CreateOrder with retry after lost request: %v", err)
	}
	if got := killer.calls.Load(); got != 2 {
		t.Errorf("server saw %d attempts, want 2", got)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 1 {
		t.Errorf("orders = %d, want 1", got)
	}
	if got := testpg.AccountBalance(t, st.db, testAccount); got != 10_000-300 {
		t.Errorf("balance = %d, want %d", got, 10_000-300)
	}
}

// statusFault returns UNAVAILABLE as an ordinary gRPC status (headers plus
// HTTP trailers — net/http offers no frame-level control, so connect-go
// cannot emit a trailers-only error frame).
type statusFault struct {
	calls atomic.Int32
	armed atomic.Bool
}

func (f *statusFault) interceptor() connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			f.calls.Add(1)
			if f.armed.CompareAndSwap(true, false) {
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("injected status fault"))
			}
			return next(ctx, req)
		}
	})
}

// TestGRPCStatusUnavailableIsNotAutoRetried pins a real interop constraint:
// gRFC A6 lets grpc-go apply its retryPolicy only to trailers-only failures
// (grpc-go stream.go: `if !a.transportStream.TrailersOnly() { return false,
// err }`), and a net/http-based connect server can never produce one. A
// server-*returned* UNAVAILABLE therefore surfaces to the caller unretried —
// which is exactly why the fault drills above sever connections instead, and
// why callers must still retry with a stable idempotency key.
func TestGRPCStatusUnavailableIsNotAutoRetried(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	fault := &statusFault{}
	fault.armed.Store(true)
	server := startServer(t, st, fault.interceptor())
	testpg.CreateAccount(t, st.db, testAccount, "USD", 10_000)

	conn := grpcConn(t, server, fastRetryOpt())
	_, err := grpcCreate(t, conn, testToken, "status-fault", 100, "USD")
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("CreateOrder = %v, want the injected Unavailable to surface", err)
	}
	if got := fault.calls.Load(); got != 1 {
		t.Errorf("server saw %d attempts, want 1 (no auto-retry for non-trailers-only status)", got)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 0 {
		t.Errorf("orders = %d, want 0", got)
	}

	// The caller-side remedy is the same as for any UNAVAILABLE: retry with
	// the same idempotency key.
	if _, err := grpcCreate(t, conn, testToken, "status-fault", 100, "USD"); err != nil {
		t.Fatalf("manual retry: %v", err)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 1 {
		t.Errorf("orders after manual retry = %d, want 1", got)
	}
}

// TestConnectClientManualRetryAfterResponseLost mirrors the drill for the
// Connect protocol: connect-go clients do not retry automatically, so the
// caller retries with the same idempotency key and must get the original
// order back without a second charge.
func TestConnectClientManualRetryAfterResponseLost(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	killer := &connKiller{runHandler: true}
	server := startServer(t, st, killer.interceptor())
	proxy := startKillerProxy(t, server.addr)
	killer.kill = proxy.KillConns
	killer.armed.Store(true)
	testpg.CreateAccount(t, st.db, testAccount, "USD", 10_000)

	client := orderv1connect.NewOrderServiceClient(&http.Client{}, "http://"+proxy.addr())

	if _, err := connectCreate(t, client, testToken, "manual-retry", 600, "USD"); err == nil {
		t.Fatal("first attempt must fail: its response was lost")
	}

	order, err := connectCreate(t, client, testToken, "manual-retry", 600, "USD")
	if err != nil {
		t.Fatalf("manual retry: %v", err)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 1 {
		t.Errorf("orders = %d, want 1", got)
	}
	if got := testpg.AccountBalance(t, st.db, testAccount); got != 10_000-600 {
		t.Errorf("balance = %d, want %d (deducted once)", got, 10_000-600)
	}
	if order.GetIdempotencyKey() != "manual-retry" {
		t.Errorf("order = %+v, want the original manual-retry order", order)
	}
}
