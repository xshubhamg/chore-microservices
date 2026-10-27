package postgres

import "embed"

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations exposes the embedded booking schema.
func Migrations() embed.FS { return migrations }
