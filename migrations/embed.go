// Package migrations embeds the SQL schema files.
//
// Embedding rather than reading from disk means a deployed binary carries the
// exact schema it was built against. A server that finds its migrations missing
// at start-up, or finds a version newer than itself, is a class of incident
// this removes entirely.
package migrations

import "embed"

// FS holds every migration file, named "<version>_<name>.<up|down>.sql".
//
//go:embed *.sql
var FS embed.FS
