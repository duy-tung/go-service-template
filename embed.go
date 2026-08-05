// Package orderengine exposes repository-level embedded assets.
package orderengine

import "embed"

// MigrationsFS embeds the SQL migrations so the single deployable binary can
// apply them (see internal/migrate and the Helm pre-install/pre-upgrade
// Job). The migrations/ directory stays the only source of truth.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS
