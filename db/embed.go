// Package db embeds SQL migrations so cmd/migration and tests can run them without external files.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
