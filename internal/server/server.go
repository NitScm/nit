// Package server is the nit control plane: identity, authorization,
// sync points, the task queue and the blob store, behind an HTTP API.
//
// It performs no git operation. Everything that clones, applies or pushes runs
// in a worker, so a large repository or a slow forge can never hold an API
// request open.
//
// The one piece of real logic here is the push path: verify the sync point,
// decode the patch, run the policy over it, and refuse or enqueue. Doing that
// before anything is queued means an unauthorized push costs no clone.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/NitScm/nit/internal/auditlog"
	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/blob"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/enforce"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// Config holds the tunables of a server.
type Config struct {
	Addr string

	Tenant policy.TenantID

	// MaxPatchBytes caps an uploaded payload, compressed and decompressed. A
	// few kilobytes of crafted zstd expand to gigabytes; the ceiling is what
	// makes that a rejected request rather than an outage.
	MaxPatchBytes int64

	// PullArtifactTTL is how long a generated pull patch stays fetchable.
	PullArtifactTTL time.Duration

	// EventPollInterval and EventMaxWait shape the long poll a waiting CLI
	// holds open. The server never connects back to a developer machine — it is
	// behind NAT — so this is how a client learns its task finished.
	EventPollInterval time.Duration
	EventMaxWait      time.Duration

	// ReadHeaderTimeout guards against a client opening connections and never
	// sending anything.
	ReadHeaderTimeout time.Duration

	// Guards are the structural protections applied on top of path rules.
	Guards enforce.Guards

	// AdminGroups may read the operations API.
	//
	// Named in the server configuration rather than in the policy bundle, on
	// purpose: an operator locked out by a bad rule still has to be able to look
	// at why. Putting this in the bundle would make the tool for diagnosing a
	// broken bundle depend on that bundle.
	AdminGroups []policy.GroupID

	// AllowedOrigins are the browser origins permitted to call the API.
	//
	// Empty means no cross-origin access at all, which is right for a console
	// served from the same host. A development front end runs on its own port
	// and needs its origin listed explicitly; there is deliberately no wildcard.
	AllowedOrigins []string
}

func (c *Config) applyDefaults() {
	if c.Tenant == "" {
		c.Tenant = policy.DefaultTenant
	}
	if c.MaxPatchBytes <= 0 {
		c.MaxPatchBytes = 100 << 20 // 100 MiB
	}
	if c.PullArtifactTTL <= 0 {
		c.PullArtifactTTL = 24 * time.Hour
	}
	if c.EventPollInterval <= 0 {
		c.EventPollInterval = 500 * time.Millisecond
	}
	if c.EventMaxWait <= 0 {
		c.EventMaxWait = 30 * time.Second
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = 10 * time.Second
	}
	if len(c.Guards.ProtectedPaths) == 0 && !c.Guards.SymlinksRequireAdmin && !c.Guards.SubmodulesRequireAdmin {
		c.Guards = enforce.DefaultGuards()
	}
}

// Deps are the collaborators a server needs.
type Deps struct {
	Store      store.Store
	Queue      *queue.Queue
	Blobs      blob.Store
	Policy     policyloader.Source
	Auth       *auth.Service
	SyncTokens *synctoken.Signer
	Log        *slog.Logger
	Now        func() time.Time

	// AuditSink receives every decision in addition to the database. Nil means
	// persist only, which is the default and what most deployments run.
	AuditSink audit.Sink
}

// Server serves the nit API.
type Server struct {
	cfg  Config
	deps Deps

	// audit persists every decision and forwards it, and cannot fail an
	// operation while doing so.
	audit *auditlog.Recorder

	mux *http.ServeMux

	// routes records every registered pattern, so a test can prove the OpenAPI
	// description and the code describe the same API. A specification that
	// drifts from its implementation is worse than none: it is believed.
	routes []string
}

// Routes returns the registered patterns, as "METHOD /path".
func (s *Server) Routes() []string {
	return append([]string(nil), s.routes...)
}

// New wires a server. It validates that every dependency is present: a server
// missing its sync token signer would run and fail at the first push, which is
// a much worse way to learn about a configuration mistake.
func New(cfg Config, deps Deps) (*Server, error) {
	cfg.applyDefaults()

	switch {
	case deps.Store == nil:
		return nil, errors.New("server: no store")
	case deps.Queue == nil:
		return nil, errors.New("server: no queue")
	case deps.Blobs == nil:
		return nil, errors.New("server: no blob store")
	case deps.Policy == nil:
		return nil, errors.New("server: no policy source")
	case deps.Auth == nil:
		return nil, errors.New("server: no authenticator")
	case deps.SyncTokens == nil:
		return nil, errors.New("server: no sync token signer")
	}

	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}

	s := &Server{
		cfg:   cfg,
		deps:  deps,
		audit: auditlog.New(deps.Store.Audit(), deps.AuditSink, deps.Log),
	}
	s.mux = s.buildRoutes()

	return s, nil
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.requestID(s.logging(s.cors(s.mux))))
}

func (s *Server) buildRoutes() *http.ServeMux {
	mux := http.NewServeMux()

	// register records the pattern as it wires it, so Routes() cannot fall out
	// of step with what is actually served.
	register := func(pattern string, handler http.Handler) {
		s.routes = append(s.routes, pattern)
		mux.Handle(pattern, handler)
	}

	// Unauthenticated: a load balancer has no token, and the API description is
	// what a client reads before it has one.
	register("GET "+protocol.RouteHealthz, http.HandlerFunc(s.handleHealthz))
	register("GET "+RouteOpenAPI, http.HandlerFunc(handleOpenAPI))

	// Authenticated.
	register("GET "+protocol.RouteWhoAmI, s.authenticated(s.handleWhoAmI))
	register("GET "+protocol.RouteRepos, s.authenticated(s.handleRepositories))

	register("POST "+protocol.RouteWorkspaces, s.authenticated(s.handleCreateWorkspace))
	register("GET "+protocol.RouteWorkspaces, s.authenticated(s.handleListWorkspaces))

	register("POST "+protocol.RoutePush, s.authenticated(s.handlePush))
	register("POST "+protocol.RoutePull, s.authenticated(s.handlePull))

	register("POST "+protocol.RouteBlobs, s.authenticated(s.handleUploadBlob))

	register("GET "+protocol.RouteTasks+"/{id}", s.authenticated(s.handleGetTask))
	register("GET "+protocol.RouteEvents, s.authenticated(s.handleTaskEvents))
	register("GET "+protocol.RouteTaskPatch, s.authenticated(s.handleTaskPatch))

	// Operations API: read-only, restricted to the admin groups. Everything
	// that changes authorization goes through the policy bundle, so that the
	// reviewed path stays the only path.
	register("GET /v1/admin/tasks", s.adminOnly(s.handleAdminTasks))
	register("GET /v1/admin/tasks/{id}", s.adminOnly(s.handleAdminTask))
	register("GET /v1/admin/audit", s.adminOnly(s.handleAdminAudit))
	register("GET /v1/admin/stats", s.adminOnly(s.handleAdminStats))
	register("GET /v1/admin/policy", s.adminOnly(s.handleAdminPolicy))

	return mux
}

// ListenAndServe runs until the context is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,

		// No WriteTimeout: the event endpoint holds a response open for the
		// length of a long poll by design, and a blanket write deadline would
		// cut it off.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	done := make(chan struct{})

	go func() {
		defer close(done)
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.deps.Log.Error("shutdown failed", "error", err)
		}
	}()

	s.deps.Log.Info("listening", "addr", s.cfg.Addr)

	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}

	<-done

	return err
}
