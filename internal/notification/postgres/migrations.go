package postgres

import "embed"

//go:embed migrations/*.sql
var migrations embed.FS

func Migrations() embed.FS { return migrations }
