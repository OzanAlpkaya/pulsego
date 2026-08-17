// Package db exposes the SQL migrations as an embedded filesystem so that the
// migrate binary carries its own schema and needs no files mounted at
// runtime.
package db

import "embed"

// Migrations holds the goose migration files under db/migrations.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// MigrationsDir is the path of the migrations inside Migrations.
const MigrationsDir = "migrations"
