// Package sql embeds NetMeta's ClickHouse schema so cmd/migrate ships as a single binary.
package sql

import "embed"

// Migrations holds versioned goose migrations (applied once, in order).
//
//go:embed migrations/*.sql
var Migrations embed.FS

// Functions holds UDF definitions (CREATE OR REPLACE), re-applied on every start.
//
//go:embed functions/*.sql
var Functions embed.FS
