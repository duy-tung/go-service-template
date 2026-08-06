package connecttransport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/otelconnect"
	"google.golang.org/protobuf/types/known/timestamppb"

	orderv1 "github.com/acme/order-engine/gen/order/v1"
	"github.com/acme/order-engine/gen/order/v1/orderv1connect"
	"github.com/acme/order-engine/internal/domain"
	"github.com/acme/order-engine/internal/usecase"
)

// Health service names probed by the kubelet. They are plain strings from
// grpc.health.v1's perspective, deliberately not protobuf service names.
const (
	HealthServiceLiveness  = "liveness"
	HealthServiceReadiness = "readiness"
)

// Default message size bounds; requests and responses beyond these are
// rejected instead of exhausting memory.
const (
	DefaultReadMaxBytes = 4 << 20
	DefaultSendMaxBytes = 4 << 20
)

// OrderServiceHandler serves order.v1.OrderService.
type OrderServiceHandler struct {
	placeOrder *usecase.PlaceOrder
	logger     *slog.Logger
}

// NewOrderServiceHandler wires the RPC handler.
func NewOrderServiceHandler(placeOrder *usecase.PlaceOrder, logger *slog.Logger) (*OrderServiceHandler, error) {
	if placeOrder == nil || logger == nil {
		return nil, errors.New("connecttransport: NewOrderServiceHandler requires placeOrder and logger")
	}
	return &OrderServiceHandler{placeOrder: placeOrder, logger: logger}, nil
}

// CreateOrder implements orderv1connect.OrderServiceHandler.
func (h *OrderServiceHandler) CreateOrder(
	ctx context.Context,
	req *connect.Request[orderv1.CreateOrderRequest],
) (*connect.Response[orderv1.CreateOrderResponse], error) {
	identity, ok := IdentityFromContext(ctx)
	if !ok {
		// The auth interceptor always runs first in production wiring; this
		// guards against a future mis-assembled server ever trusting an
		// unauthenticated request.
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no authenticated identity"))
	}

	msg := req.Msg
	order, err := h.placeOrder.Execute(ctx, identity.AccountID, msg.GetIdempotencyKey(), msg.GetAmountMinor(), msg.GetCurrency())
	if err != nil {
		connectErr := toConnectError(err)
		if connectErr.Code() == connect.CodeInternal {
			// Full cause stays in server logs; the client sees a sanitized
			// message only.
			h.logger.ErrorContext(ctx, "create order failed", "error", err)
		} else {
			h.logger.InfoContext(ctx, "create order rejected",
				"code", connectErr.Code().String(), "account_id", identity.AccountID)
		}
		return nil, connectErr
	}

	h.logger.InfoContext(ctx, "order created",
		"order_id", order.ID, "account_id", order.AccountID,
		"amount_minor", order.AmountMinor, "currency", order.Currency)
	return connect.NewResponse(&orderv1.CreateOrderResponse{Order: toProtoOrder(order)}), nil
}

func toProtoOrder(o *domain.Order) *orderv1.Order {
	return &orderv1.Order{
		Id:             o.ID,
		AccountId:      o.AccountID,
		IdempotencyKey: o.IdempotencyKey,
		AmountMinor:    o.AmountMinor,
		Currency:       o.Currency,
		CreatedAt:      timestamppb.New(o.CreatedAt),
	}
}

// MuxConfig assembles the public HTTP surface of the service.
type MuxConfig struct {
	PlaceOrder *usecase.PlaceOrder
	Validator  TokenValidator
	Logger     *slog.Logger
	Health     *grpchealth.StaticChecker

	// ReadMaxBytes/SendMaxBytes default to DefaultReadMaxBytes/DefaultSendMaxBytes.
	ReadMaxBytes int
	SendMaxBytes int

	// RequestTimeout bounds each unary RPC (handler + database time);
	// defaults to DefaultRequestTimeout.
	RequestTimeout time.Duration

	// ExtraInterceptors are placed outermost, ahead of tracing and auth.
	// Production wiring leaves this empty; tests use it for fault injection.
	ExtraInterceptors []connect.Interceptor
}

// NewMux returns the ServeMux serving the order service (Connect, gRPC and
// gRPC-Web on one path) plus the gRPC health service. Health endpoints are
// mounted without the auth interceptor: the kubelet sends no Authorization
// header. The tracing interceptor wraps auth so failed authentication is
// traced too.
func NewMux(cfg MuxConfig) (*http.ServeMux, error) {
	if cfg.Health == nil {
		return nil, errors.New("connecttransport: NewMux requires a health checker")
	}
	handler, err := NewOrderServiceHandler(cfg.PlaceOrder, cfg.Logger)
	if err != nil {
		return nil, err
	}
	authInterceptor, err := NewAuthInterceptor(cfg.Validator)
	if err != nil {
		return nil, err
	}
	// The global tracer provider and propagator are used on purpose: this is
	// a server behind the trust boundary, and otelconnect only trusts
	// incoming trace context as far as the propagator does.
	tracingInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		return nil, fmt.Errorf("connecttransport: tracing interceptor: %w", err)
	}

	readMax := cfg.ReadMaxBytes
	if readMax <= 0 {
		readMax = DefaultReadMaxBytes
	}
	sendMax := cfg.SendMaxBytes
	if sendMax <= 0 {
		sendMax = DefaultSendMaxBytes
	}
	requestTimeout := cfg.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = DefaultRequestTimeout
	}

	// The timeout interceptor sits innermost so the bound covers the handler
	// and everything below it, while failed auth is never charged against it.
	interceptors := make([]connect.Interceptor, 0, len(cfg.ExtraInterceptors)+3)
	interceptors = append(interceptors, cfg.ExtraInterceptors...)
	interceptors = append(interceptors, tracingInterceptor, authInterceptor,
		timeoutInterceptor(requestTimeout))

	mux := http.NewServeMux()
	path, orderHandler := orderv1connect.NewOrderServiceHandler(handler,
		connect.WithInterceptors(interceptors...),
		connect.WithReadMaxBytes(readMax),
		connect.WithSendMaxBytes(sendMax),
	)
	mux.Handle(path, orderHandler)
	mux.Handle(grpchealth.NewHandler(cfg.Health))
	return mux, nil
}

// NewHealthChecker returns the health checker with liveness serving and
// readiness explicitly NOT_SERVING: readiness only flips to serving after
// the first successful database ping.
func NewHealthChecker() *grpchealth.StaticChecker {
	checker := grpchealth.NewStaticChecker(HealthServiceLiveness, HealthServiceReadiness)
	// NewStaticChecker defaults every listed service to SERVING, which would
	// let traffic in before the first successful dependency check.
	checker.SetStatus(HealthServiceReadiness, grpchealth.StatusNotServing)
	return checker
}
