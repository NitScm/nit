// Package auth issues and verifies the tokens the CLI authenticates with.
//
// Identity in nit comes from here and nowhere else. In particular it never
// comes from the patch: the author field of a git commit is free text, and a
// system that trusted it would let anyone attribute a change to a colleague —
// including a colleague with wider access.
//
// Tokens are opaque random strings. Only their hash is stored, so a database
// dump yields nothing usable, and nit never needs to display a token again
// after issuing it once.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// TokenPrefix marks a nit token.
//
// It exists so that secret scanners — GitHub's, an internal one, a grep in a
// support ticket — can recognize a leaked credential on sight. A token that
// looks like random base64 gets pasted into issues forever.
const TokenPrefix = "nit_"

// tokenBytes is the entropy behind a token. 32 bytes is far beyond brute force
// and keeps the string short enough to paste.
const tokenBytes = 32

// Authentication failures. They are distinguished because the right advice
// differs: an expired token means "log in again", an unknown one means
// something is wrong, and a disabled account means talk to an administrator.
var (
	ErrNoCredentials = errors.New("auth: no credentials")
	ErrMalformed     = errors.New("auth: malformed token")
	ErrUnknownToken  = errors.New("auth: unknown token")
	ErrExpired       = errors.New("auth: token expired")
	ErrRevoked       = errors.New("auth: token revoked")
	ErrUserDisabled  = errors.New("auth: user disabled")

	// ErrUnknownSubject: the account exists in the database but not in the
	// policy bundle. Authentication succeeds and authorization cannot proceed,
	// which is a configuration error worth naming precisely.
	ErrUnknownSubject = errors.New("auth: user is not in the policy bundle")
)

// Principal is an authenticated caller.
type Principal struct {
	User    *store.User
	Session *store.Session

	// Tenant is whose data this request may touch, resolved from the token
	// rather than from the process's configuration.
	//
	// It is the answer to the question a multi-tenant control plane asks on
	// every request, and it is deliberately *here* rather than in a service
	// field: a tenant fixed at construction is a process that serves one
	// customer, which is right for a self-hosted deployment and wrong for a
	// hosted one.
	Tenant policy.TenantID

	// Subject is the resolved authorization principal, groups already expanded
	// from the policy bundle in force at authentication time.
	Subject policy.Subject

	// PolicyVersion is the bundle that resolved the subject. Recording it means
	// a decision can be replayed against exactly the rules that produced it.
	PolicyVersion string
}

// GenerateToken returns a new token and its hash.
func GenerateToken() (token string, hash []byte, err error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: entropy unavailable: %w", err)
	}

	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	return token, HashToken(token), nil
}

// HashToken returns the stored form of a token.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// BearerToken extracts a token from an Authorization header value.
func BearerToken(header string) (string, error) {
	if header == "" {
		return "", ErrNoCredentials
	}

	const scheme = "Bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", ErrMalformed
	}

	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", ErrMalformed
	}

	return token, nil
}

// Clock is the time source, injectable so expiry is testable without waiting.
type Clock func() time.Time

// Service issues and verifies tokens.
type Service struct {
	sessions store.SessionStore
	users    store.UserStore
	policy   policyloader.Source
	tenant   policy.TenantID
	now      Clock
}

// NewService wires an authentication service.
func NewService(s store.Store, source policyloader.Source, tenant policy.TenantID, now Clock) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if tenant == "" {
		tenant = policy.DefaultTenant
	}

	return &Service{
		sessions: s.Sessions(),
		users:    s.Users(),
		policy:   source,
		tenant:   tenant,
		now:      now,
	}
}

// Issue creates a token for a user.
//
// The plaintext is returned once and never stored. A caller that loses it
// issues another; there is no recovery path, by design.
func (s *Service) Issue(ctx context.Context, userID store.ID, label string, ttl time.Duration) (string, *store.Session, error) {
	token, hash, err := GenerateToken()
	if err != nil {
		return "", nil, err
	}

	session := &store.Session{
		TenantID:  s.tenant,
		UserID:    userID,
		TokenHash: hash,
		Label:     label,
		CreatedAt: s.now(),
	}

	if ttl > 0 {
		expiry := s.now().Add(ttl)
		session.ExpiresAt = &expiry
	}

	created, err := s.sessions.Create(ctx, session)
	if err != nil {
		return "", nil, err
	}

	return token, created, nil
}

// Revoke invalidates a session.
func (s *Service) Revoke(ctx context.Context, id store.ID) error {
	return s.sessions.Revoke(ctx, id, s.now())
}

// Authenticate verifies a token and resolves the caller.
//
// Note the order: the session is checked, then the account, then the policy
// bundle. Each step can fail for its own reason, and the caller is told which —
// a system that answers "unauthorized" to every one of them generates support
// tickets nobody can close.
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, ErrNoCredentials
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		return nil, ErrMalformed
	}

	session, err := s.sessions.ByTokenHash(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnknownToken
	}
	if err != nil {
		return nil, err
	}

	// The lookup already matched on the hash, so this comparison is redundant
	// against a correct store. It is here so that a backend doing a fuzzy or
	// prefix match — a future cache, an index bug — cannot turn into an
	// authentication bypass, and it is constant-time so it adds no signal.
	if subtle.ConstantTimeCompare(session.TokenHash, HashToken(token)) != 1 {
		return nil, ErrUnknownToken
	}

	now := s.now()

	if session.RevokedAt != nil {
		return nil, ErrRevoked
	}
	if session.ExpiresAt != nil && !now.Before(*session.ExpiresAt) {
		return nil, ErrExpired
	}

	user, err := s.users.ByID(ctx, session.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnknownToken
	}
	if err != nil {
		return nil, err
	}

	if user.Disabled {
		return nil, ErrUserDisabled
	}

	current := s.policy.Current()

	subject, err := current.Subject(user.PolicyUserID)
	if errors.Is(err, policy.ErrUnknownUser) {
		return nil, fmt.Errorf("%w: %s", ErrUnknownSubject, user.PolicyUserID)
	}
	if err != nil {
		return nil, err
	}

	// Best-effort: knowing an installation is alive is useful, but failing a
	// request because the bookkeeping write failed is not.
	_ = s.sessions.Touch(ctx, session.ID, now)

	return &Principal{
		User:          user,
		Session:       session,
		Tenant:        session.TenantID,
		Subject:       subject,
		PolicyVersion: current.Version(),
	}, nil
}

// contextKey is unexported so no other package can collide with it.
type contextKey struct{}

// WithPrincipal returns a context carrying the authenticated caller.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// PrincipalFrom returns the authenticated caller, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(contextKey{}).(*Principal)
	return p
}
