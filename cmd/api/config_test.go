package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setBaseline provides the required variables so tests can focus on the
// knob under test: the DSN plus the explicit opt-in for the built-in dev
// token that the defaults carry.
func setBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("ORDER_ENGINE_DATABASE_URL", "postgres://app@db:5432/orders?sslmode=disable")
	t.Setenv("ORDER_ENGINE_ALLOW_DEV_AUTH", "true")
}

func TestLoadConfigDefaults(t *testing.T) {
	setBaseline(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := defaultConfig()
	want.DatabaseURL = "postgres://app@db:5432/orders?sslmode=disable"
	want.AllowDevAuth = true
	if cfg != want {
		t.Errorf("loadConfig = %+v, want defaults %+v", cfg, want)
	}
}

// TestLoadConfigRefusesDevTokenWithoutOptIn pins the fail-closed guardrail:
// the published development credential must never serve silently.
func TestLoadConfigRefusesDevTokenWithoutOptIn(t *testing.T) {
	t.Setenv("ORDER_ENGINE_DATABASE_URL", "postgres://app@db:5432/orders?sslmode=disable")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("default dev token without opt-in must fail validation")
	}
	if !strings.Contains(err.Error(), "ORDER_ENGINE_ALLOW_DEV_AUTH") {
		t.Errorf("error %q must point at the ORDER_ENGINE_ALLOW_DEV_AUTH opt-in", err)
	}
}

func TestLoadConfigAcceptsRealTokenWithoutOptIn(t *testing.T) {
	t.Setenv("ORDER_ENGINE_DATABASE_URL", "postgres://app@db:5432/orders?sslmode=disable")
	t.Setenv("ORDER_ENGINE_AUTH_TOKEN", "a-real-secret")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig with a real token: %v", err)
	}
	if cfg.AllowDevAuth {
		t.Error("AllowDevAuth must stay false unless explicitly set")
	}
}

func TestLoadConfigRejectsEmptyAccountID(t *testing.T) {
	setBaseline(t)
	t.Setenv("ORDER_ENGINE_AUTH_ACCOUNT_ID", "")

	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "ORDER_ENGINE_AUTH_ACCOUNT_ID") {
		t.Fatalf("empty account id must fail validation, got %v", err)
	}
}

// TestLoadConfigEnvOverridesUseDocumentedNames pins the exact variable names
// referenced by the Helm chart, compose.yaml and README.
func TestLoadConfigEnvOverridesUseDocumentedNames(t *testing.T) {
	setBaseline(t)
	t.Setenv("ORDER_ENGINE_LISTEN_ADDR", "127.0.0.1:6000")
	t.Setenv("ORDER_ENGINE_AUTH_TOKEN", "other-token")
	t.Setenv("ORDER_ENGINE_AUTH_ACCOUNT_ID", "acct-x")
	t.Setenv("ORDER_ENGINE_TRACING_ENABLED", "0") // ParseBool semantics, not =="true"
	t.Setenv("ORDER_ENGINE_REQUEST_TIMEOUT", "12s")
	t.Setenv("ORDER_ENGINE_SHUTDOWN_TIMEOUT", "45s")
	t.Setenv("ORDER_ENGINE_DB_MAX_OPEN_CONNS", "33")
	t.Setenv("ORDER_ENGINE_DB_MAX_IDLE_CONNS", "22")
	t.Setenv("ORDER_ENGINE_DB_CONN_MAX_LIFETIME", "7m")
	t.Setenv("ORDER_ENGINE_DB_CONN_MAX_IDLE_TIME", "90s")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:6000" || cfg.AuthToken != "other-token" || cfg.AuthAccountID != "acct-x" {
		t.Errorf("string overrides not applied: %+v", cfg)
	}
	if cfg.TracingEnabled {
		t.Error("TRACING_ENABLED=0 must disable tracing (strconv-style bool parsing)")
	}
	if cfg.RequestTimeout != 12*time.Second || cfg.ShutdownTimeout != 45*time.Second ||
		cfg.DBConnMaxLife != 7*time.Minute || cfg.DBConnMaxIdle != 90*time.Second {
		t.Errorf("duration overrides not applied: %+v", cfg)
	}
	if cfg.DBMaxOpenConns != 33 || cfg.DBMaxIdleConns != 22 {
		t.Errorf("int overrides not applied: %+v", cfg)
	}
}

func TestLoadConfigFileThenEnvPrecedence(t *testing.T) {
	setBaseline(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(
		"listen_addr: 127.0.0.1:7100\nshutdown_timeout: 90s\ndb_max_open_conns: 41\n"), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	t.Setenv(envConfigFile, path)
	// Env must beat the file for the same key.
	t.Setenv("ORDER_ENGINE_LISTEN_ADDR", "127.0.0.1:7200")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:7200" {
		t.Errorf("ListenAddr = %q, want the env value overriding the file", cfg.ListenAddr)
	}
	if cfg.ShutdownTimeout != 90*time.Second || cfg.DBMaxOpenConns != 41 {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if cfg.DBMaxIdleConns != defaultConfig().DBMaxIdleConns {
		t.Errorf("untouched keys must keep defaults: %+v", cfg)
	}
}

func TestLoadConfigMissingFileFails(t *testing.T) {
	setBaseline(t)
	t.Setenv(envConfigFile, filepath.Join(t.TempDir(), "absent.yaml"))
	if _, err := loadConfig(); err == nil {
		t.Fatal("an explicitly configured file that cannot be read must fail loudly")
	}
}

func TestLoadConfigReportsAllProblemsTogether(t *testing.T) {
	// DATABASE_URL missing + two invalid values: one run must surface all.
	t.Setenv("ORDER_ENGINE_DATABASE_URL", "")
	t.Setenv("ORDER_ENGINE_DB_MAX_OPEN_CONNS", "-3")
	t.Setenv("ORDER_ENGINE_SHUTDOWN_TIMEOUT", "0s")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("invalid configuration must fail")
	}
	for _, fragment := range []string{"ORDER_ENGINE_DATABASE_URL", "db_max_open_conns", "shutdown_timeout"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q must mention %s (all problems reported together)", err, fragment)
		}
	}
}

func TestLoadConfigRejectsMalformedValues(t *testing.T) {
	setBaseline(t)
	t.Setenv("ORDER_ENGINE_DB_MAX_OPEN_CONNS", "many")
	if _, err := loadConfig(); err == nil {
		t.Fatal("non-numeric int value must fail decoding")
	}

	t.Setenv("ORDER_ENGINE_DB_MAX_OPEN_CONNS", "10")
	t.Setenv("ORDER_ENGINE_SHUTDOWN_TIMEOUT", "soon")
	if _, err := loadConfig(); err == nil {
		t.Fatal("non-duration value must fail decoding")
	}
}
