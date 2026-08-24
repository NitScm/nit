package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// maxRequestBody caps a JSON request body. Patches never travel this way — they
// are uploaded as blobs — so a metadata document that large is a mistake or an
// attack either way.
const maxRequestBody = 1 << 20

// apiError is an HTTP status together with the body to render.
type apiError struct {
	status int
	body   *protocol.Error
}

func (e *apiError) Error() string { return e.body.Error() }

func fail(status int, code, format string, args ...any) *apiError {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}

	return &apiError{
		status: status,
		body:   &protocol.Error{Code: code, Message: message},
	}
}

// handlerFunc is a handler that may fail with a rendered error.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (s *Server) authenticated(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := auth.BearerToken(r.Header.Get("Authorization"))
		if err != nil {
			s.writeError(w, r, unauthorized(err))
			return
		}

		principal, err := s.deps.Auth.Authenticate(r.Context(), token)
		if err != nil {
			s.writeError(w, r, unauthorized(err))
			return
		}

		// Both, and in this order. The principal is what the handlers read;
		// the tenant is what the *store* reads, because a backend enforcing
		// row-level security stamps it on the connection before every query.
		//
		// Stamping it here rather than in each handler is the point: a handler
		// that forgot would not read the wrong tenant's rows, it would read
		// none — but only if something put it in the context to begin with,
		// and this is the one place that knows.
		ctx := auth.WithPrincipal(r.Context(), principal)
		ctx = store.WithTenant(ctx, principal.Tenant)

		if err := h(w, r.WithContext(ctx)); err != nil {
			s.writeError(w, r, err)
		}
	})
}

// unauthorized maps an authentication failure to a response.
//
// The message differs per cause on purpose. "Unauthorized" for every case is
// unhelpful to the point of being an operational problem: an expired token, a
// disabled account and a user missing from the policy bundle need three
// different actions, and only one of them is "log in again".
func unauthorized(err error) *apiError {
	switch {
	case errors.Is(err, auth.ErrNoCredentials):
		return fail(http.StatusUnauthorized, "no_credentials", "no credentials; run: nit login")
	case errors.Is(err, auth.ErrMalformed):
		return fail(http.StatusUnauthorized, "malformed_credentials", "malformed credentials")
	case errors.Is(err, auth.ErrExpired):
		return fail(http.StatusUnauthorized, "token_expired", "token expired; run: nit login")
	case errors.Is(err, auth.ErrRevoked):
		return fail(http.StatusUnauthorized, "token_revoked", "token revoked; run: nit login")
	case errors.Is(err, auth.ErrUserDisabled):
		return fail(http.StatusForbidden, "user_disabled", "account disabled; contact an administrator")
	case errors.Is(err, auth.ErrUnknownSubject):
		return fail(http.StatusForbidden, "user_not_in_policy",
			"your account is not in the policy bundle; contact an administrator")
	case errors.Is(err, auth.ErrUnknownToken):
		return fail(http.StatusUnauthorized, "invalid_token", "invalid token; run: nit login")
	default:
		return fail(http.StatusInternalServerError, "internal", "authentication failed")
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		// An unclassified error is a bug. Log it in full, tell the client
		// nothing: internal messages leak table names, paths and sometimes
		// secrets.
		s.log(r).ErrorContext(r.Context(), "unhandled error", "error", err)

		apiErr = fail(http.StatusInternalServerError, "internal", "internal error")
	}

	if apiErr.status >= 500 {
		s.log(r).ErrorContext(r.Context(), "request failed",
			"code", apiErr.body.Code, "message", apiErr.body.Message)
	}

	writeJSON(w, apiErr.status, apiErr.body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// decodeJSON reads a request body into out, rejecting unknown fields.
//
// Strict decoding turns a client typo into an error instead of a silently
// ignored field — which, on a request carrying a push mode or a sync token,
// would mean acting on something other than what the client asked for.
func decodeJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(out); err != nil {
		return fail(http.StatusBadRequest, "bad_request", "malformed request body: %v", err)
	}

	return nil
}

// checkProtocolVersion refuses clients this server cannot serve, with a message
// that says what to do rather than a decoding error three layers down.
func checkProtocolVersion(version string) error {
	if version == "" || version == protocol.Version {
		return nil
	}

	return fail(http.StatusBadRequest, protocol.CodeUnsupportedVersion,
		"client speaks protocol %s, this server speaks %s; upgrade the nit CLI",
		version, protocol.Version)
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

type requestIDKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			var buf [8]byte
			if _, err := rand.Read(buf[:]); err == nil {
				id = hex.EncodeToString(buf[:])
			}
		}

		w.Header().Set("X-Request-Id", id)

		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.deps.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.log(r).InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", s.deps.Now().Sub(start))
	})
}

// recoverer keeps one panicking handler from taking down the process, and every
// other in-flight request with it.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log(r).ErrorContext(r.Context(), "panic in handler",
					"panic", rec, "path", r.URL.Path)

				writeJSON(w, http.StatusInternalServerError,
					&protocol.Error{Code: "internal", Message: "internal error"})
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func (s *Server) log(r *http.Request) *slog.Logger {
	logger := s.deps.Log

	if id := requestIDFrom(r.Context()); id != "" {
		logger = logger.With("request", id)
	}
	if p := auth.PrincipalFrom(r.Context()); p != nil {
		logger = logger.With("user", p.User.PolicyUserID)
	}

	return logger
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying writer so the long-poll handler can stream.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
