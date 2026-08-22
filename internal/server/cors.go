package server

import (
	"net/http"
	"strings"
)

// cors permits browser access from the configured origins.
//
// There is deliberately no wildcard. The API is read-and-write and bearer
// authenticated; "*" would let any page a developer happens to visit call it
// with their credentials if a token ever reached browser-accessible storage.
// A console served from the same host needs no CORS at all, and a development
// front end on its own port lists its origin explicitly.
func (s *Server) cors(next http.Handler) http.Handler {
	if len(s.cfg.AllowedOrigins) == 0 {
		return next
	}

	allowed := make(map[string]struct{}, len(s.cfg.AllowedOrigins))
	for _, origin := range s.cfg.AllowedOrigins {
		allowed[strings.TrimRight(strings.TrimSpace(origin), "/")] = struct{}{}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")

		if _, ok := allowed[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Nit-Digest, X-Nit-Encoding, X-Request-Id")
			w.Header().Set("Access-Control-Expose-Headers", "X-Nit-Digest, X-Request-Id")
			w.Header().Set("Access-Control-Max-Age", "600")

			// Origin is echoed rather than fixed, so a cache shared between
			// origins cannot serve one origin's headers to another.
			w.Header().Add("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			// A preflight is answered whether or not the origin is allowed:
			// without the headers above, the browser refuses the real request
			// anyway, and answering uniformly keeps the handler simple.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
