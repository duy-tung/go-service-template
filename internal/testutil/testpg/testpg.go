// Package testpg provisions throwaway PostgreSQL databases for integration
// tests. Each call to Open creates a dedicated database on the configured
// server, applies the repository's migrations with psql (the same mechanism
// the Makefile uses), and drops the database again on cleanup.
package testpg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx database/sql driver
)

const (
	// EnvDatabaseURL points at an admin database (URL form, e.g.
	// postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable) whose
	// role may create and drop databases.
	EnvDatabaseURL = "ORDER_ENGINE_TEST_DATABASE_URL"

	// EnvRequireDB turns the "no database configured" skip into a failure,
	// so CI and the local gate suite cannot silently skip integration tests.
	EnvRequireDB = "ORDER_ENGINE_TEST_REQUIRE_DB"
)

const opTimeout = 30 * time.Second

// Open returns a pool bound to a fresh, fully migrated database.
func Open(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := open(t, true)
	return db
}

// OpenEmpty returns a pool bound to a fresh database with NO migrations
// applied, plus its DSN — for tests that exercise the migration runner
// itself.
func OpenEmpty(t *testing.T) (*sql.DB, string) {
	t.Helper()
	return open(t, false)
}

func open(t *testing.T, migrated bool) (*sql.DB, string) {
	t.Helper()

	adminDSN := os.Getenv(EnvDatabaseURL)
	if adminDSN == "" {
		if os.Getenv(EnvRequireDB) != "" {
			t.Fatalf("%s is set but %s is empty: integration tests are required here", EnvRequireDB, EnvDatabaseURL)
		}
		t.Skipf("set %s to run integration tests against a real PostgreSQL", EnvDatabaseURL)
	}

	name := freshDatabaseName(t)

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	// The name is generated from a fixed safe alphabet, so quoting suffices.
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		admin.Close()
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		defer admin.Close()
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if _, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE %q WITH (FORCE)`, name)); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	dsn, err := swapDatabase(adminDSN, name)
	if err != nil {
		t.Fatalf("derive test DSN: %v", err)
	}
	if migrated {
		applyMigrations(t, dsn)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}
	return db, dsn
}

// CreateAccount inserts an account row for tests.
func CreateAccount(t *testing.T, db *sql.DB, id, currency string, balanceMinor int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	_, err := db.ExecContext(ctx,
		`INSERT INTO "accounts" ("id", "currency", "balance_minor") VALUES ($1, $2, $3)`,
		id, currency, balanceMinor)
	if err != nil {
		t.Fatalf("seed account %s: %v", id, err)
	}
}

// AccountBalance reads an account's current balance.
func AccountBalance(t *testing.T, db *sql.DB, id string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var balance int64
	if err := db.QueryRowContext(ctx, `SELECT "balance_minor" FROM "accounts" WHERE "id" = $1`, id).Scan(&balance); err != nil {
		t.Fatalf("read balance of %s: %v", id, err)
	}
	return balance
}

// CountOrders counts the orders stored for an account.
func CountOrders(t *testing.T, db *sql.DB, accountID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM "orders" WHERE "account_id" = $1`, accountID).Scan(&n); err != nil {
		t.Fatalf("count orders of %s: %v", accountID, err)
	}
	return n
}

func freshDatabaseName(t *testing.T) string {
	t.Helper()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate database name: %v", err)
	}
	return fmt.Sprintf("oe_test_%d_%s", os.Getpid(), hex.EncodeToString(suffix[:]))
}

func swapDatabase(dsn, database string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse %s (URL form required): %w", EnvDatabaseURL, err)
	}
	u.Path = "/" + database
	return u.String(), nil
}

// applyMigrations runs every up migration through psql, mirroring the
// documented production workflow so test and deployed schemas cannot drift.
func applyMigrations(t *testing.T, dsn string) {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate repository root: runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	pattern := filepath.Join(root, "migrations", "*.up.sql")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations (%s): files=%v err=%v", pattern, files, err)
	}
	sort.Strings(files)

	for _, file := range files {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		// -1 wraps the whole file in one transaction; the files carry no
		// BEGIN/COMMIT of their own.
		cmd := exec.CommandContext(ctx, "psql", dsn, "-q", "-1", "-v", "ON_ERROR_STOP=1", "-f", file)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("apply migration %s: %v\n%s", filepath.Base(file), err, out)
		}
	}
}
