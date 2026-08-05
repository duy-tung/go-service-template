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
	"strconv"
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

	"github.com/acme/order-engine/internal/repository/postgres"
	connecttransport "github.com/acme/order-engine/internal/transport/connect"
	"github.com/acme/order-engine/internal/usecase"
	"github.com/acme/order-engine/pkg/slogotel"
)

const serviceName = "order-engine"

type config struct {
	listenAddr      string
	databaseURL     string
	authToken       string
	authAccountID   string
	tracingEnabled  bool
	shutdownTimeout time.Duration
	dbMaxOpenConns  int
	dbMaxIdleConns  int
	dbConnMaxLife   time.Duration
	dbConnMaxIdle   time.Duration
	readinessPeriod time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		listenAddr:      envString("ORDER_ENGINE_LISTEN_ADDR", "0.0.0.0:50051"),
		databaseURL:     os.Getenv("ORDER_ENGINE_DATABASE_URL"),
		authToken:       envString("ORDER_ENGINE_AUTH_TOKEN", "token-123"),
		authAccountID:   envString("ORDER_ENGINE_AUTH_ACCOUNT_ID", "acct-demo"),
		tracingEnabled:  envString("ORDER_ENGINE_TRACING_ENABLED", "true") == "true",
		shutdownTimeout: 20 * time.Second,
		dbMaxOpenConns:  10,
		dbMaxIdleConns:  5,
		dbConnMaxLife:   30 * time.Minute,
		dbConnMaxIdle:   5 * time.Minute,
		readinessPeriod: 3 * time.Second,
	}
	if cfg.databaseURL == "" {
		return config{}, errors.New("ORDER_ENGINE_DATABASE_URL is required")
	}
	if cfg.authToken == "" {
		return config{}, errors.New("ORDER_ENGINE_AUTH_TOKEN must not be empty")
	}
	var err error
	if cfg.shutdownTimeout, err = envDuration("ORDER_ENGINE_SHUTDOWN_TIMEOUT", cfg.shutdownTimeout); err != nil {
		return config{}, err
	}
	if cfg.dbMaxOpenConns, err = envInt("ORDER_ENGINE_DB_MAX_OPEN_CONNS", cfg.dbMaxOpenConns); err != nil {
		return config{}, err
	}
	if cfg.dbMaxIdleConns, err = envInt("ORDER_ENGINE_DB_MAX_IDLE_CONNS", cfg.dbMaxIdleConns); err != nil {
		return config{}, err
	}
	if cfg.dbConnMaxLife, err = envDuration("ORDER_ENGINE_DB_CONN_MAX_LIFETIME", cfg.dbConnMaxLife); err != nil {
		return config{}, err
	}
	if cfg.dbConnMaxIdle, err = envDuration("ORDER_ENGINE_DB_CONN_MAX_IDLE_TIME", cfg.dbConnMaxIdle); err != nil {
		return config{}, err
	}
	if cfg.dbMaxOpenConns <= 0 || cfg.shutdownTimeout <= 0 {
		return config{}, errors.New("connection and timeout settings must be positive")
	}
	return cfg, nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("order-engine exited", "error", err)
		os.Exit(1)
	}
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
	db, err := sql.Open("pgx", cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.dbMaxOpenConns)
	db.SetMaxIdleConns(cfg.dbMaxIdleConns)
	db.SetConnMaxLifetime(cfg.dbConnMaxLife)
	db.SetConnMaxIdleTime(cfg.dbConnMaxIdle)

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		// Log-only: readiness stays NOT_SERVING until the background loop
		// sees a healthy database, so startup does not crash-loop while a
		// dependency is briefly down.
		logger.WarnContext(ctx, "initial database ping failed; readiness stays NOT_SERVING", "error", err)
	}
	cancelPing()

	tracerProvider, err := newTracerProvider(ctx, cfg.tracingEnabled)
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
			Token:     cfg.authToken,
			AccountID: cfg.authAccountID,
		},
		Logger: logger,
		Health: health,
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
		Addr:              cfg.listenAddr,
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		// Design Decision: ReadTimeout/WriteTimeout stay unset. They bound
		// the whole request lifetime and would sever long-lived streaming
		// RPCs; per-message bounds come from Read/SendMaxBytes and
		// client deadlines instead.
	}

	go watchReadiness(ctx, logger, db, health, cfg.readinessPeriod)

	serverFailed := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "listening", "addr", cfg.listenAddr)
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

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
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

func envString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
