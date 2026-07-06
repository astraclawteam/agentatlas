// Package dbfs embeds the goose migrations so every environment applies the
// exact same schema through internal/storage.Migrate — no drift between the
// goose CLI and embedded application startup.
package dbfs

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
