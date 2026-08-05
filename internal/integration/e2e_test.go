package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"connectrpc.com/grpchealth"

	orderv1 "github.com/acme/order-engine/gen/order/v1"
	"github.com/acme/order-engine/gen/order/v1/orderv1connect"
	"github.com/acme/order-engine/internal/grpcclient"
	"github.com/acme/order-engine/internal/testutil/testpg"
	connecttransport "github.com/acme/order-engine/internal/transport/connect"
)

const callTimeout = 15 * time.Second

func connectCreate(t *testing.T, client orderv1connect.OrderServiceClient, token, key string, amount int64, currency string) (*orderv1.Order, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	req := connect.NewRequest(&orderv1.CreateOrderRequest{
		IdempotencyKey: key,
		AmountMinor:    amount,
		Currency:       currency,
	})
	if token != "" {
		req.Header().Set("Authorization", bearer(token))
	}
	resp, err := client.CreateOrder(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetOrder(), nil
}

func grpcConn(t *testing.T, server *testServer, extraOpts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	conn, err := grpcclient.New(server.addr, extraOpts...)
	if err != nil {
		t.Fatalf("grpcclient.New: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func grpcCreate(t *testing.T, conn *grpc.ClientConn, token, key string, amount int64, currency string) (*orderv1.Order, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer(token))
	}
	resp, err := orderv1.NewOrderServiceClient(conn).CreateOrder(ctx, &orderv1.CreateOrderRequest{
		IdempotencyKey: key,
		AmountMinor:    amount,
		Currency:       currency,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetOrder(), nil
}

// TestCreateOrderAcrossWireProtocols proves one h2c server serves the
// Connect protocol (HTTP/1.1), gRPC-Web, and native gRPC — and that
// idempotency holds across protocols.
func TestCreateOrderAcrossWireProtocols(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	server := startServer(t, st)
	testpg.CreateAccount(t, st.db, testAccount, "USD", 100_000)

	connectClient := orderv1connect.NewOrderServiceClient(http.DefaultClient, server.url)
	viaConnect, err := connectCreate(t, connectClient, testToken, "proto-shared", 1000, "USD")
	if err != nil {
		t.Fatalf("Connect create: %v", err)
	}

	grpcWebClient := orderv1connect.NewOrderServiceClient(http.DefaultClient, server.url, connect.WithGRPCWeb())
	viaGRPCWeb, err := connectCreate(t, grpcWebClient, testToken, "proto-web", 500, "USD")
	if err != nil {
		t.Fatalf("gRPC-Web create: %v", err)
	}
	if viaGRPCWeb.GetAmountMinor() != 500 {
		t.Errorf("gRPC-Web order = %+v, want amount 500", viaGRPCWeb)
	}

	conn := grpcConn(t, server)
	viaGRPC, err := grpcCreate(t, conn, testToken, "proto-shared", 1000, "USD")
	if err != nil {
		t.Fatalf("native gRPC create: %v", err)
	}
	if viaGRPC.GetId() != viaConnect.GetId() {
		t.Errorf("cross-protocol retry created a second order: connect=%s grpc=%s",
			viaConnect.GetId(), viaGRPC.GetId())
	}

	if got := testpg.AccountBalance(t, st.db, testAccount); got != 100_000-1000-500 {
		t.Errorf("balance = %d, want %d (each logical order deducted once)", got, 100_000-1000-500)
	}
	if got := testpg.CountOrders(t, st.db, testAccount); got != 2 {
		t.Errorf("orders = %d, want 2", got)
	}
}

func TestAuthAcrossProtocols(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	server := startServer(t, st)
	testpg.CreateAccount(t, st.db, testAccount, "USD", 1000)

	connectClient := orderv1connect.NewOrderServiceClient(http.DefaultClient, server.url)

	if _, err := connectCreate(t, connectClient, "", "auth-1", 100, "USD"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Connect without token: %v, want CodeUnauthenticated", err)
	}
	if _, err := connectCreate(t, connectClient, "token-999", "auth-2", 100, "USD"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Connect wrong token: %v, want CodeUnauthenticated", err)
	}

	conn := grpcConn(t, server)
	if _, err := grpcCreate(t, conn, "", "auth-3", 100, "USD"); status.Code(err) != codes.Unauthenticated {
		t.Errorf("gRPC without token: %v, want Unauthenticated", err)
	}

	if _, err := connectCreate(t, connectClient, testToken, "auth-ok", 100, "USD"); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
}

func TestErrorInfoDecodesOverBothClients(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	server := startServer(t, st)
	testpg.CreateAccount(t, st.db, testAccount, "USD", 1000)

	connectClient := orderv1connect.NewOrderServiceClient(http.DefaultClient, server.url)
	if _, err := connectCreate(t, connectClient, testToken, "conflict-key", 100, "USD"); err != nil {
		t.Fatalf("setup create: %v", err)
	}

	// Connect client decoding.
	_, err := connectCreate(t, connectClient, testToken, "conflict-key", 200, "USD")
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeAlreadyExists {
		t.Fatalf("Connect conflict = %v, want CodeAlreadyExists", err)
	}
	foundConnect := false
	for _, detail := range connectErr.Details() {
		msg, valueErr := detail.Value()
		if valueErr != nil {
			continue
		}
		if info, ok := msg.(*errdetails.ErrorInfo); ok {
			foundConnect = true
			if info.GetReason() != connecttransport.ReasonIdempotencyConflict || info.GetDomain() != connecttransport.ErrorInfoDomain {
				t.Errorf("Connect ErrorInfo = %s/%s, want %s/%s",
					info.GetDomain(), info.GetReason(),
					connecttransport.ErrorInfoDomain, connecttransport.ReasonIdempotencyConflict)
			}
		}
	}
	if !foundConnect {
		t.Error("Connect error carried no decodable ErrorInfo detail")
	}

	// grpc-go client decoding of the same failure.
	conn := grpcConn(t, server)
	_, err = grpcCreate(t, conn, testToken, "conflict-key", 200, "USD")
	st2 := status.Convert(err)
	if st2.Code() != codes.AlreadyExists {
		t.Fatalf("gRPC conflict = %v, want AlreadyExists", err)
	}
	foundGRPC := false
	for _, detail := range st2.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			foundGRPC = true
			if info.GetReason() != connecttransport.ReasonIdempotencyConflict {
				t.Errorf("gRPC ErrorInfo reason = %s, want %s", info.GetReason(), connecttransport.ReasonIdempotencyConflict)
			}
		}
	}
	if !foundGRPC {
		t.Error("gRPC status carried no decodable ErrorInfo detail")
	}

	// Insufficient balance and validation classifications.
	if _, err := grpcCreate(t, conn, testToken, "too-big", 999_999, "USD"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("insufficient balance = %v, want FailedPrecondition", err)
	}
	if _, err := connectCreate(t, connectClient, testToken, "bad amount!", -1, "USD"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid payload = %v, want CodeInvalidArgument", err)
	}
}

// TestHealthEndpoints exercises grpc.health.v1 over the native gRPC wire
// protocol: liveness stays SERVING, readiness reflects the checker state,
// and no Authorization header is ever required.
func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	st := newStack(t, nil)
	server := startServer(t, st)

	conn := grpcConn(t, server)
	healthClient := grpc_health_v1.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	live, err := healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: connecttransport.HealthServiceLiveness})
	if err != nil || live.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("liveness = %v (err %v), want SERVING", live.GetStatus(), err)
	}

	ready, err := healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: connecttransport.HealthServiceReadiness})
	if err != nil || ready.GetStatus() != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("fresh readiness = %v (err %v), want NOT_SERVING", ready.GetStatus(), err)
	}

	server.health.SetStatus(connecttransport.HealthServiceReadiness, grpchealth.StatusServing)
	ready, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: connecttransport.HealthServiceReadiness})
	if err != nil || ready.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("readiness after SetStatus = %v (err %v), want SERVING", ready.GetStatus(), err)
	}

	server.health.SetStatus(connecttransport.HealthServiceReadiness, grpchealth.StatusNotServing)
	ready, err = healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: connecttransport.HealthServiceReadiness})
	if err != nil || ready.GetStatus() != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("readiness after drain flip = %v (err %v), want NOT_SERVING", ready.GetStatus(), err)
	}

	if _, err := healthClient.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown health service = %v, want NotFound", err)
	}
}
