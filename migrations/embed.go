// Package migrations embeds the SQL schema files.
//
// Embedding rather than reading from disk means a deployed binary carries the
// exact schema it was built against. A server that finds its migrations missing
// at start-up, or finds a version newer than itself, is a class of incident
// this removes entirely.
//
// One directory per dialect, with the same version numbers meaning the same
// schema. They are not generated from a common source and are not meant to be:
// the differences between them are the interesting part — a partial index with
// no equivalent, a TRUNCATE that cannot be intercepted — and a generator would
// hide exactly those. `pkg/store/storetest` is what proves the two backends
// behave alike; the SQL is allowed to differ as much as it must.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed postgres/*.sql
var postgresFS embed.FS

//go:embed mysql/*.sql
var mysqlFS embed.FS

// Postgres holds the PostgreSQL migrations, named "<version>_<name>.<up|down>.sql".
var Postgres = sub(postgresFS, "postgres")

// MySQL holds the MySQL and MariaDB migrations. The two share a dialect here:
// everything nit needs is supported by MySQL 8.0.16+ and MariaDB 10.6+ alike,
// and where they differ it is called out in the file.
var MySQL = sub(mysqlFS, "mysql")

// sub panics rather than returning an error: the directory is embedded at build
// time, so a failure here is a broken binary, not a runtime condition.
func sub(fsys embed.FS, dir string) fs.FS {
	out, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("migrations: " + err.Error())
	}
	return out
}
