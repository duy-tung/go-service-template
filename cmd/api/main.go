// Command api runs the order-engine server: order.v1.OrderService over
// Connect, gRPC and gRPC-Web plus gRPC health, on a single h2c listener.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/grpchealth"
	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx database/sql driver
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	orderengine "github.com/acme/order-engine"
	"github.com/acme/order-engine/internal/migrate"
	"github.com/acme/order-engine/internal/repository/postgres"
	connecttransport "github.com/acme/order-engine/internal/transport/connect"
	"github.com/acme/order-engine/internal/usecase"
	"github.com/acme/order-engine/pkg/slogotel"
)

const serviceName = "order-engine"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			// Explicit one-shot mode for the pre-install/pre-upgrade Job.
			// Serving mode below never touches migrations: N replicas
			// starting simultaneously must not race on DDL.
			if err := runMigrate(); err != nil {
				slog.Error("migration failed", "error", err)
				os.Exit(1)
			}
			return
		default:
			slog.Error("unknown command; supported: migrate", "command", os.Args[1])
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		slog.Error("order-engine exited", "error", err)
		os.Exit(1)
	}
}

func runMigrate() error {
	dsn := os.Getenv("ORDER_ENGINE_DATABASE_URL")
	if dsn == "" {
		return errors.New("ORDER_ENGINE_DATABASE_URL is required")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	return migrate.Run(ctx, logger, dsn, orderengine.MigrationsFS)
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	contextHandler, err := slogotel.NewContextHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err != nil {
		return err
	}
	logger := slog.New(contextHandler)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Database pool. The DSN is a secret and is never logged.
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(cfg.DBConnMaxLife)
	db.SetConnMaxIdleTime(cfg.DBConnMaxIdle)

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		// Log-only: readiness stays NOT_SERVING until the background loop
		// sees a healthy database, so startup does not crash-loop while a
		// dependency is briefly down.
		logger.WarnContext(ctx, "initial database ping failed; readiness stays NOT_SERVING", "error", err)
	}
	cancelPing()

	tracerProvider, err := newTracerProvider(ctx, cfg.TracingEnabled)
	if err != nil {
		return fmt.Errorf("tracer provider: %w", err)
	}
	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	health := connecttransport.NewHealthChecker()

	orders, err := postgres.NewOrderRepository(db)
	if err != nil {
		return err
	}
	balances, err := postgres.NewBalanceRepository(db)
	if err != nil {
		return err
	}
	transactor, err := postgres.NewSQLTransactor(db)
	if err != nil {
		return err
	}
	placeOrder, err := usecase.NewPlaceOrder(orders, balances, transactor)
	if err != nil {
		return err
	}

	mux, err := connecttransport.NewMux(connecttransport.MuxConfig{
		PlaceOrder: placeOrder,
		// StaticTokenValidator is a development/test credential source; a
		// real deployment injects a production TokenValidator here.
		Validator: connecttransport.StaticTokenValidator{
			Token:     cfg.AuthToken,
			AccountID: cfg.AuthAccountID,
		},
		Logger:         logger,
		Health:         health,
		RequestTimeout: cfg.RequestTimeout,
	})
	if err != nil {
		return err
	}

	// Accept HTTP/1.1 and HTTP/2 with prior knowledge on one cleartext port.
	// TLS terminates at the Gateway; pod traffic is h2c inside the cluster.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		// Design Decision: ReadTimeout/WriteTimeout stay unset. They bound
		// the whole request lifetime and would sever long-lived streaming
		// RPCs; per-message bounds come from Read/SendMaxBytes, and unary
		// handler time is capped by the request_timeout interceptor. Time
		// spent reading a slow request body is the edge proxy's to bound.
	}

	go watchReadiness(ctx, logger, db, health, cfg.ReadinessPeriod)

	serverFailed := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "listening", "addr", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverFailed <- err
		}
		close(serverFailed)
	}()

	select {
	case err, ok := <-serverFailed:
		if ok && err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return errors.New("server stopped unexpectedly")
	case <-ctx.Done():
	}

	// Shutdown order: stop advertising readiness, drain in-flight requests,
	// flush telemetry, then release the database pool.
	logger.Info("shutting down")
	health.SetStatus(connecttransport.HealthServiceReadiness, grpchealth.StatusNotServing)

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelDrain()
	if err := server.Shutdown(drainCtx); err != nil {
		logger.Error("server drain incomplete", "error", err)
	}

	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFlush()
	if err := tracerProvider.Shutdown(flushCtx); err != nil {
		logger.Error("tracer provider shutdown", "error", err)
	}

	if err := db.Close(); err != nil {
		logger.Error("database close", "error", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// newTracerProvider builds the SDK tracer provider. With tracing enabled it
// exports OTLP over gRPC (endpoint via the standard OTEL_EXPORTER_OTLP_*
// environment variables) through a batch span processor.
func newTracerProvider(ctx context.Context, tracingEnabled bool) (*sdktrace.TracerProvider, error) {
	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", serviceName)))
	if err != nil {
		return nil, err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if tracingEnabled {
		exporter, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("otlp trace exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	return sdktrace.NewTracerProvider(opts...), nil
}

// watchReadiness keeps the readiness health status in sync with database
// connectivity until ctx is canceled.
func watchReadiness(ctx context.Context, logger *slog.Logger, db *sql.DB, health *grpchealth.StaticChecker, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	healthy := false
	for {
		pingCtx, cancel := context.WithTimeout(ctx, period)
		err := db.PingContext(pingCtx)
		cancel()
		switch {
		case err == nil && !healthy:
			healthy = true
			health.SetStatus(connecttransport.HealthServiceReadiness, grpchealth.StatusServing)
			logger.InfoContext(ctx, "database reachable; readiness SERVING")
		case err != nil && healthy:
			healthy = false
			health.SetStatus(connecttransport.HealthServiceReadiness, grpchealth.StatusNotServing)
			logger.WarnContext(ctx, "database unreachable; readiness NOT_SERVING", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
