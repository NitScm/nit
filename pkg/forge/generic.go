package forge

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Generic is the driver for any remote git can already talk to: HTTPS with a
// token, SSH with a key, or a local path.
//
// It is the fallback for every forge, and it is enough for the whole push and
// pull cycle: nit's git operations are plain clone, fetch and push. Provider
// APIs only become necessary for the shortcuts — reading a branch tip without
// cloning, opening a pull request — which is what the specific drivers add.
type Generic struct {
	// Key is the value repositories name in the policy bundle.
	Key string
}

// NewGeneric returns a driver registered under the given key.
func NewGeneric(key string) *Generic {
	if key == "" {
		key = "generic"
	}
	return &Generic{Key: key}
}

// Kind implements Forge.
func (g *Generic) Kind() string { return g.Key }

// AuthenticatedRemote injects credentials into an HTTPS remote and leaves every
// other form alone.
//
// The returned string is a credential. Callers must keep it out of logs and out
// of error messages: git quotes the URL it was given, so an unfiltered git error
// is a token in the log file.
func (g *Generic) AuthenticatedRemote(_ context.Context, repo RepoRef, creds Credentials) (string, error) {
	if repo.Remote == "" {
		return "", fmt.Errorf("forge: repository has no remote")
	}

	// SSH and local paths carry no in-band credentials; git resolves the key
	// from its own configuration.
	if !strings.HasPrefix(repo.Remote, "http://") && !strings.HasPrefix(repo.Remote, "https://") {
		return repo.Remote, nil
	}

	if creds.Token == "" {
		return repo.Remote, nil
	}

	parsed, err := url.Parse(repo.Remote)
	if err != nil {
		return "", fmt.Errorf("forge: remote is not a valid URL: %w", err)
	}

	username := creds.Username
	if username == "" {
		// The convention every major forge accepts for token auth over HTTPS.
		username = "x-access-token"
	}

	parsed.User = url.UserPassword(username, creds.Token)

	return parsed.String(), nil
}

// BranchHead is not implemented without a provider API. The worker resolves the
// tip from its clone instead, which costs a fetch it was going to do anyway.
func (g *Generic) BranchHead(context.Context, RepoRef, string, Credentials) (string, error) {
	return "", fmt.Errorf("forge: %s does not support reading a branch tip without cloning", g.Key)
}

// DefaultBranch is likewise unavailable; the bundle declares it.
func (g *Generic) DefaultBranch(context.Context, RepoRef, Credentials) (string, error) {
	return "", fmt.Errorf("forge: %s does not report a default branch; declare it in the policy bundle", g.Key)
}

// Registry resolves a forge key to its driver.
type Registry struct {
	drivers  map[string]Forge
	fallback Forge
}

// NewRegistry returns a registry whose fallback is the generic driver, so a
// repository naming a forge nit has no specific driver for still works.
func NewRegistry(drivers ...Forge) *Registry {
	r := &Registry{
		drivers:  make(map[string]Forge, len(drivers)),
		fallback: NewGeneric("generic"),
	}

	for _, d := range drivers {
		r.drivers[d.Kind()] = d
	}

	return r
}

// Get returns the driver for a forge key.
func (r *Registry) Get(kind string) Forge {
	if d, ok := r.drivers[kind]; ok {
		return d
	}
	return r.fallback
}

var _ Forge = (*Generic)(nil)
