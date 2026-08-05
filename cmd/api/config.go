package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	envprovider "github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

const (
	envPrefix     = "ORDER_ENGINE_"
	envConfigFile = "ORDER_ENGINE_CONFIG_FILE"
)

// config is the full runtime configuration. Sources merge with precedence
// defaults < YAML file (ORDER_ENGINE_CONFIG_FILE) < environment. The koanf
// tags double as the YAML keys and as the environment names minus the
// ORDER_ENGINE_ prefix, so the documented variable names stay authoritative.
type config struct {
	ListenAddr      string        `koanf:"listen_addr"`
	DatabaseURL     string        `koanf:"database_url"`
	AuthToken       string        `koanf:"auth_token"`
	AuthAccountID   string        `koanf:"auth_account_id"`
	TracingEnabled  bool          `koanf:"tracing_enabled"`
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
	DBMaxOpenConns  int           `koanf:"db_max_open_conns"`
	DBMaxIdleConns  int           `koanf:"db_max_idle_conns"`
	DBConnMaxLife   time.Duration `koanf:"db_conn_max_lifetime"`
	DBConnMaxIdle   time.Duration `koanf:"db_conn_max_idle_time"`
	ReadinessPeriod time.Duration `koanf:"readiness_period"`
}

func defaultConfig() config {
	return config{
		ListenAddr: "0.0.0.0:50051",
		// Dev/test static validator credentials; see the README trust model.
		AuthToken:       "token-123",
		AuthAccountID:   "acct-demo",
		TracingEnabled:  true,
		ShutdownTimeout: 20 * time.Second,
		DBMaxOpenConns:  10,
		// Idle equals open so steady traffic reuses warm connections instead
		// of churning through fresh ones.
		DBMaxIdleConns:  10,
		DBConnMaxLife:   30 * time.Minute,
		DBConnMaxIdle:   5 * time.Minute,
		ReadinessPeriod: 3 * time.Second,
	}
}

// loadConfig merges defaults, the optional YAML file, and the environment,
// then validates the result. Type conversions ("20s" → duration, "10" → int,
// "true"/"1" → bool) come from koanf's decoder, and validation failures are
// reported together via errors.Join — no per-variable error plumbing.
func loadConfig() (config, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(defaultConfig(), "koanf"), nil); err != nil {
		return config{}, fmt.Errorf("config defaults: %w", err)
	}
	if path := os.Getenv(envConfigFile); path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return config{}, fmt.Errorf("config file %s: %w", path, err)
		}
	}
	if err := k.Load(envprovider.Provider(".", envprovider.Opt{
		Prefix: envPrefix,
		TransformFunc: func(key, value string) (string, any) {
			// ORDER_ENGINE_DB_MAX_OPEN_CONNS → db_max_open_conns: flat keys,
			// byte-identical to the documented names. Variables that map to
			// no struct field (e.g. ORDER_ENGINE_CONFIG_FILE itself) are
			// simply ignored by Unmarshal.
			return strings.ToLower(strings.TrimPrefix(key, envPrefix)), value
		},
	}), nil); err != nil {
		return config{}, fmt.Errorf("config environment: %w", err)
	}

	var cfg config
	if err := k.Unmarshal("", &cfg); err != nil {
		return config{}, fmt.Errorf("config decode: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (c config) validate() error {
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("ORDER_ENGINE_DATABASE_URL is required"))
	}
	if c.AuthToken == "" {
		errs = append(errs, errors.New("ORDER_ENGINE_AUTH_TOKEN must not be empty"))
	}
	if c.ListenAddr == "" {
		errs = append(errs, errors.New("listen_addr must not be empty"))
	}
	if c.DBMaxOpenConns <= 0 {
		errs = append(errs, fmt.Errorf("db_max_open_conns must be positive, got %d", c.DBMaxOpenConns))
	}
	if c.DBMaxIdleConns < 0 {
		errs = append(errs, fmt.Errorf("db_max_idle_conns must not be negative, got %d", c.DBMaxIdleConns))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("shutdown_timeout must be positive, got %s", c.ShutdownTimeout))
	}
	if c.ReadinessPeriod <= 0 {
		errs = append(errs, fmt.Errorf("readiness_period must be positive, got %s", c.ReadinessPeriod))
	}
	return errors.Join(errs...)
}
