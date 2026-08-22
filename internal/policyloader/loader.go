// Package policyloader holds the compiled policy bundle in memory and reloads
// it when the files on disk change.
//
// The rule that shapes it: a failed reload never changes anything. If an
// operator pushes a bundle that does not compile, the server keeps serving the
// last good one and shouts about it. The alternatives are both worse — failing
// open grants access nobody authorized, and failing closed takes an outage on
// every typo.
package policyloader

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	policyconfig "github.com/NitScm/nit/pkg/policy/config"
)

// Source provides the policy currently in force.
//
// It is an interface so that tests, and the worker, can supply a fixed bundle
// without a directory on disk.
type Source interface {
	Current() *policy.Policy
}

// Static is a Source wrapping one immutable policy.
type Static struct{ Policy *policy.Policy }

// Current implements Source.
func (s Static) Current() *policy.Policy { return s.Policy }

// Loader watches a bundle directory.
type Loader struct {
	dir string
	log *slog.Logger

	current atomic.Pointer[policy.Policy]

	// OnReload is called after a successful reload that changed the version. It
	// is where the server reconciles repositories into the database, so a new
	// repository in the bundle gets a row without anyone running a command.
	OnReload func(*policy.Policy)
}

// New loads a bundle and returns a loader holding it.
//
// A bundle that does not compile is a start-up failure: a server with no policy
// can answer no question correctly, and starting anyway would mean either
// denying everything or, far worse, allowing it.
func New(dir string, log *slog.Logger) (*Loader, error) {
	if log == nil {
		log = slog.Default()
	}

	p, err := policyconfig.Load(dir)
	if err != nil {
		return nil, err
	}

	l := &Loader{dir: dir, log: log}
	l.current.Store(p)

	log.Info("policy loaded", "version", p.Version(), "repositories", len(p.Repositories()))

	return l, nil
}

// NewStatic returns a loader serving a fixed policy, for tests.
func NewStatic(p *policy.Policy) *Loader {
	l := &Loader{log: slog.Default()}
	l.current.Store(p)
	return l
}

// Current returns the policy in force. It never returns nil after a successful
// New, and the returned value is immutable: a reload swaps the pointer, so a
// request that is halfway through an authorization pass keeps evaluating
// against one consistent bundle.
func (l *Loader) Current() *policy.Policy { return l.current.Load() }

// Reload rereads the bundle and reports whether the version changed.
//
// On error nothing is swapped and the previous bundle stays in force.
func (l *Loader) Reload() (changed bool, err error) {
	if l.dir == "" {
		return false, nil
	}

	p, err := policyconfig.Load(l.dir)
	if err != nil {
		return false, err
	}

	previous := l.current.Load()
	if previous != nil && previous.Version() == p.Version() {
		return false, nil
	}

	l.current.Store(p)

	l.log.Info("policy reloaded",
		"version", p.Version(),
		"previous", versionOf(previous),
		"repositories", len(p.Repositories()))

	if l.OnReload != nil {
		l.OnReload(p)
	}

	return true, nil
}

// Watch reloads periodically until the context is cancelled.
//
// Polling rather than filesystem notifications: a bundle is a directory of
// files that a deploy writes one at a time, and reacting to the first write
// would compile a half-written bundle. A few seconds of latency on a policy
// change is not a problem worth that risk.
func (l *Loader) Watch(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			if _, err := l.Reload(); err != nil {
				// Deliberately not fatal, and deliberately loud: the previous
				// bundle is still in force, so the system is safe but running
				// on rules that no longer match what the operator believes.
				l.log.ErrorContext(ctx, "policy reload failed, keeping previous bundle",
					"error", err, "version", versionOf(l.current.Load()))
			}
		}
	}
}

func versionOf(p *policy.Policy) string {
	if p == nil {
		return ""
	}
	return p.Version()
}

var _ Source = (*Loader)(nil)
