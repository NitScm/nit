// Package forge abstracts the hosting provider a repository lives on.
//
// nit is not a GitHub product: the upstream can be GitHub Cloud, GitHub
// Enterprise, GitLab, Gitea or a plain SSH remote. Everything provider-specific
// is confined to this interface, so adding a provider never touches the policy
// engine, the queue or the workers.
//
// The interface is deliberately small. nit does not manage issues, reviews or
// releases; it needs to authenticate a clone, read a branch tip cheaply, and
// know when a push was rejected because the branch moved.
package forge

import "context"

// RepoRef identifies a repository on a forge.
type RepoRef struct {
	// Remote is the URL as configured in the policy bundle.
	Remote string

	// Owner and Name are parsed from Remote by the driver; they are surfaced
	// for API calls and for log lines.
	Owner string
	Name  string
}

// Credentials carry whatever the driver needs to authenticate. nit holds a
// single machine identity per repository — it is the only writer of the
// upstream — so this is a service token, not a user token.
type Credentials struct {
	// Token is a personal access token, app installation token or equivalent.
	Token string

	// Username is required by providers that expect basic auth over HTTPS.
	Username string
}

// There is deliberately no SSH key here. A driver's job is to return a URL, and
// a key cannot be expressed in one; SSH remotes are authenticated by git's own
// configuration, which the worker selects with git.ssh_command. Carrying a key
// path on this struct would also have the wrong cardinality — it is one per
// worker process, while a deploy key is valid for exactly one repository.

// Forge is a hosting provider driver.
type Forge interface {
	// Kind returns the driver key used in the policy bundle ("github",
	// "gitlab", ...).
	Kind() string

	// AuthenticatedRemote returns a URL a git process can clone and push
	// without prompting.
	//
	// Implementations must keep the token out of anything they log: an
	// authenticated URL is a credential, and git error messages quote the URL
	// they were given.
	AuthenticatedRemote(ctx context.Context, repo RepoRef, creds Credentials) (string, error)

	// BranchHead returns the commit a branch points to, without cloning.
	//
	// Workers call this before deciding to do any work at all, and the queue
	// uses it to detect a branch that moved outside nit.
	BranchHead(ctx context.Context, repo RepoRef, branch string, creds Credentials) (string, error)

	// DefaultBranch returns the repository's default branch.
	DefaultBranch(ctx context.Context, repo RepoRef, creds Credentials) (string, error)
}
