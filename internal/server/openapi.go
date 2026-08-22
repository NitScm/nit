package server

import (
	"net/http"

	"github.com/NitScm/nit/api"
)

// RouteOpenAPI serves the API description.
const RouteOpenAPI = "/openapi.yaml"

// handleOpenAPI serves the embedded specification.
//
// Unauthenticated, deliberately: it describes how to authenticate, so a client
// that cannot read it yet is exactly the client that needs it. It contains no
// deployment detail — no hostnames, no policy, no identities — only the shape of
// the API, which any caller learns by using it anyway.
func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")

	// The description changes only when the binary does, so a browser or a
	// documentation tool may hold on to it for a while.
	w.Header().Set("Cache-Control", "public, max-age=300")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(api.OpenAPI)
}
