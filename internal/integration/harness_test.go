// Package integration hosts end-to-end tests: a real server (h2c, auth,
// tracing, health) backed by real PostgreSQL, exercised through Connect,
// gRPC-Web and native gRPC clients.
package integration

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/acme/order-engine/internal/repository/postgres"
	"github.com/acme/order-engine/internal/testutil/testpg"
	connecttransport "github.com/acme/order-engine/internal/transport/connect"
	"github.com/acme/order-engine/internal/usecase"

	"connectrpc.com/grpchealth"
)

const (
	testToken   = "token-123"
	testAccount = "acct-demo"
)

// repoDecorator lets tests wrap the real order repository (e.g. with a
// synchronization barrier) while everything else stays production wiring.
type repoDecorator func(usecase.OrderRepository) usecase.OrderRepository

// stack is the production dependency graph built on a throwaway database.
type stack struct {
	db         *sql.DB
	placeOrder *usecase.PlaceOrder
}

func newStack(t *testing.T, decorate repoDecorator) *stack {
	t.Helper()
	db := testpg.Open(t)

	orders, err := postgres.NewOrderRepository(db)
	if err != nil {
		t.Fatalf("NewOrderRepository: %v", err)
	}
	var orderRepo usecase.OrderRepository = orders
	if decorate != nil {
		orderRepo = decorate(orders)
	}
	balances, err := postgres.NewBalanceRepository(db)
	if err != nil {
		t.Fatalf("NewBalanceRepository: %v", err)
	}
	transactor, err := postgres.NewSQLTransactor(db)
	if err != nil {
		t.Fatalf("NewSQLTransactor: %v", err)
	}
	placeOrder, err := usecase.NewPlaceOrder(orderRepo, balances, transactor)
	if err != nil {
		t.Fatalf("NewPlaceOrder: %v", err)
	}
	return &stack{db: db, placeOrder: placeOrder}
}

// testServer is a full order-engine server on an ephemeral port.
type testServer struct {
	addr   string // host:port
	url    string // http://host:port
	health *grpchealth.StaticChecker
	db     *sql.DB
}

// startServer boots the production mux (h2c: HTTP/1.1 + HTTP/2 prior
// knowledge) around the given stack. extra interceptors are mounted
// outermost — used by fault-injection and traffic-accounting tests.
func startServer(t *testing.T, st *stack, extra ...connect.Interceptor) *testServer {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	health := connecttransport.NewHealthChecker()
	mux, err := connecttransport.NewMux(connecttransport.MuxConfig{
		PlaceOrder:        st.placeOrder,
		Validator:         connecttransport.StaticTokenValidator{Token: testToken, AccountID: testAccount},
		Logger:            logger,
		Health:            health,
		ExtraInterceptors: extra,
	})
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Handler: mux, Protocols: protocols}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Errorf("serve: %v", serveErr)
		}
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	addr := listener.Addr().String()
	return &testServer{addr: addr, url: "http://" + addr, health: health, db: st.db}
}

func bearer(token string) string { return "Bearer " + token }

// killerProxy is a dumb TCP relay between the gRPC client and the real
// server. Tests use it to sever the client connection at an exact moment —
// e.g. after the handler committed but before the response bytes flow — which
// is the only faithful way to simulate a lost response: a server that
// *returns* an UNAVAILABLE status over net/http cannot produce the
// trailers-only frame grpc-go requires before applying its retryPolicy.
type killerProxy struct {
	t        *testing.T
	listener net.Listener
	backend  string

	mu    sync.Mutex
	conns []net.Conn
}

func startKillerProxy(t *testing.T, backendAddr string) *killerProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	proxy := &killerProxy{t: t, listener: listener, backend: backendAddr}
	go proxy.acceptLoop()
	t.Cleanup(func() {
		listener.Close()
		proxy.KillConns()
	})
	return proxy
}

func (p *killerProxy) addr() string { return p.listener.Addr().String() }

func (p *killerProxy) acceptLoop() {
	for {
		clientConn, err := p.listener.Accept()
		if err != nil {
			return
		}
		serverConn, err := net.Dial("tcp", p.backend)
		if err != nil {
			clientConn.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, clientConn, serverConn)
		p.mu.Unlock()
		relay := func(dst, src net.Conn) {
			_, _ = io.Copy(dst, src)
			dst.Close()
			src.Close()
		}
		go relay(serverConn, clientConn)
		go relay(clientConn, serverConn)
	}
}

// KillConns synchronously severs every active relayed connection. New
// connections are still accepted afterwards, so a retrying client can
// reconnect.
func (p *killerProxy) KillConns() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		conn.Close()
	}
	p.conns = nil
}
