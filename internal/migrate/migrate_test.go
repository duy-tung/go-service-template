package migrate_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	orderengine "github.com/acme/order-engine"
	"github.com/acme/order-engine/internal/migrate"
	"github.com/acme/order-engine/internal/testutil/testpg"
)

func TestRunAppliesTracksAndIsRepeatable(t *testing.T) {
	t.Parallel()
	db, dsn := testpg.OpenEmpty(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logger := slog.Default()

	if err := migrate.Run(ctx, logger, dsn, orderengine.MigrationsFS); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	var versions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if versions != 1 {
		t.Fatalf("schema_migrations rows = %d, want 1", versions)
	}

	// The real schema must exist and carry the named idempotency constraint.
	var constraint string
	if err := db.QueryRowContext(ctx,
		`SELECT conname FROM pg_constraint WHERE conname = 'uq_orders_account_idempotency_key'`,
	).Scan(&constraint); err != nil {
		t.Fatalf("named unique constraint missing after migrate: %v", err)
	}
	testpg.CreateAccount(t, db, "acct-m", "USD", 100)

	// Second run must be a strict no-op.
	if err := migrate.Run(ctx, logger, dsn, orderengine.MigrationsFS); err != nil {
		t.Fatalf("second Run must no-op: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("re-read schema_migrations: %v", err)
	}
	if versions != 1 {
		t.Fatalf("schema_migrations rows after re-run = %d, want 1", versions)
	}
}

// TestRunSerializesConcurrentInvocations is the "5 Pods hit CREATE TABLE at
// once" drill: concurrent runners must serialize on the advisory lock and
// leave exactly one applied migration, with every runner succeeding.
func TestRunSerializesConcurrentInvocations(t *testing.T) {
	t.Parallel()
	db, dsn := testpg.OpenEmpty(t)

	const runners = 5
	errs := make([]error, runners)
	var wg sync.WaitGroup
	for i := range runners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			errs[i] = migrate.Run(ctx, slog.Default(), dsn, orderengine.MigrationsFS)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d failed: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var versions int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if versions != 1 {
		t.Fatalf("schema_migrations rows = %d, want exactly 1 despite %d concurrent runners", versions, runners)
	}
	if got := testpg.CountOrders(t, db, "no-such-account"); got != 0 {
		t.Fatalf("orders table unusable after concurrent migrate: %d", got)
	}
}

func TestRunRejectsBadDSN(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := migrate.Run(ctx, nil, "host=weird form", orderengine.MigrationsFS); err == nil {
		t.Fatal("non-URL DSN must be rejected")
	}
}
