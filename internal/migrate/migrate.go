// Package migrate applies the embedded SQL migrations to PostgreSQL. It is
// deliberately not a framework: ordered *.up.sql files, one cluster-wide
// advisory lock, and a version-tracking table.
//
// Deployment runs it as a single pre-install/pre-upgrade Job — never from
// serving Pods — so N replicas starting at once cannot race on DDL. The
// advisory lock makes even accidental concurrent invocations serialize.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"sort"
)

// advisoryLockKey serializes migration runs across the whole database.
// Arbitrary, but it must never change.
const advisoryLockKey = 727857214

// versionPattern bounds recorded version identifiers (migration file names)
// to a charset that is safe to inline into the migration script.
var versionPattern = regexp.MustCompile(`^[0-9A-Za-z._-]+$`)

// Run applies every embedded migrations/*.up.sql that is not yet recorded in
// schema_migrations, in filename order. dsn must be a URL-form PostgreSQL
// DSN. Each migration and its version record are committed in one
// transaction, so a crash can never leave a half-tracked migration.
func Run(ctx context.Context, logger *slog.Logger, dsn string, migrations fs.FS) error {
	if logger == nil {
		logger = slog.Default()
	}

	simpleDSN, err := withSimpleProtocol(dsn)
	if err != nil {
		return err
	}
	// The simple query protocol lets one Exec run a multi-statement script.
	db, err := sql.Open("pgx", simpleDSN)
	if err != nil {
		return fmt.Errorf("migrate: open database: %w", err)
	}
	defer db.Close()

	// Pin one session for the whole run: pg_advisory_lock is session-scoped,
	// and *sql.DB would transparently retry a dead connection on a NEW
	// session that no longer holds the lock. With *sql.Conn a lost session
	// fails loudly instead. Closing conn (or db) ends the session, which
	// releases the lock on every exit path.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SELECT pg_advisory_lock(%d)", advisoryLockKey)); err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	logger.InfoContext(ctx, "migration lock acquired")

	if _, err := conn.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now(),
			CONSTRAINT pk_schema_migrations PRIMARY KEY (version)
		)`); err != nil {
		return fmt.Errorf("migrate: ensure schema_migrations: %w", err)
	}

	files, err := fs.Glob(migrations, "migrations/*.up.sql")
	if err != nil {
		return fmt.Errorf("migrate: list migrations: %w", err)
	}
	if len(files) == 0 {
		return errors.New("migrate: no embedded *.up.sql migrations found")
	}
	sort.Strings(files)

	for _, file := range files {
		version := path.Base(file)
		if !versionPattern.MatchString(version) {
			return fmt.Errorf("migrate: invalid migration file name %q", version)
		}

		var applied bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version,
		).Scan(&applied); err != nil {
			return fmt.Errorf("migrate: check %s: %w", version, err)
		}
		if applied {
			logger.InfoContext(ctx, "migration already applied", "version", version)
			continue
		}

		content, err := fs.ReadFile(migrations, file)
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", version, err)
		}
		// Migration files carry no BEGIN/COMMIT of their own: the runner
		// owns the transaction so the DDL and its version record commit
		// atomically. versionPattern makes the inlined literal safe.
		script := fmt.Sprintf("BEGIN;\n%s\nINSERT INTO schema_migrations (version) VALUES ('%s');\nCOMMIT;",
			content, version)
		if _, err := conn.ExecContext(ctx, script); err != nil {
			return fmt.Errorf("migrate: apply %s: %w", version, err)
		}
		logger.InfoContext(ctx, "migration applied", "version", version)
	}
	return nil
}

func withSimpleProtocol(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("migrate: parse DSN (URL form required): %w", err)
	}
	if u.Scheme == "" {
		return "", errors.New("migrate: DSN must be URL form (postgres://...)")
	}
	query := u.Query()
	query.Set("default_query_exec_mode", "simple_protocol")
	// pgx only sanitizes simple-protocol parameters under UTF8.
	query.Set("client_encoding", "UTF8")
	u.RawQuery = query.Encode()
	return u.String(), nil
}
