// Package api embeds the OpenAPI description of the nit HTTP API.
//
// It is embedded rather than served from disk so a deployed binary always
// describes the API it actually implements: a specification file that can go
// missing, or be a different version from the process serving it, is worse than
// none — it is believed.
//
// The description is hand-written. The surface is small and stable, and the
// field comments say what a value *means* to a caller, which is the part a
// generator throws away. What keeps it honest is a test: every route the server
// registers must appear in the specification, and every path in the
// specification must be a route the server serves.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.0 description of the API.
//
//go:embed openapi.yaml
var OpenAPI []byte
