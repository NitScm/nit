package memory

import (
	"bytes"
	"context"
	"sort"
	"time"

	"github.com/NitScm/nit/pkg/store"
)

type sessionStore Store

func (s *sessionStore) Create(_ context.Context, sess *store.Session) (*store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	created := *sess
	if created.ID == "" {
		created.ID = (*Store)(s).nextID("session")
	}
	if created.CreatedAt.IsZero() {
		created.CreatedAt = time.Now().UTC()
	}

	s.sessions[created.ID] = &created

	return cloneSession(&created), nil
}

// The lookup carries no tenant: the token is what resolves one.
func (s *sessionStore) ByTokenHash(_ context.Context, hash []byte) (*store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sess := range s.sessions {
		// bytes.Equal rather than a constant-time compare: the value being
		// matched is already a hash of the secret, so a timing signal here
		// leaks nothing an attacker could use to recover the token.
		if bytes.Equal(sess.TokenHash, hash) {
			return cloneSession(sess), nil
		}
	}

	return nil, store.ErrNotFound
}

func (s *sessionStore) ByID(_ context.Context, id store.ID) (*store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil, store.ErrNotFound
	}

	return cloneSession(sess), nil
}

func (s *sessionStore) ListByUser(_ context.Context, userID store.ID) ([]*store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.Session
	for _, sess := range s.sessions {
		if sess.UserID == userID {
			out = append(out, cloneSession(sess))
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *sessionStore) Touch(_ context.Context, id store.ID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return store.ErrNotFound
	}

	sess.LastUsedAt = &at
	return nil
}

func (s *sessionStore) Revoke(_ context.Context, id store.ID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return store.ErrNotFound
	}

	// Revocation is not undone by a second call: keep the first instant, which
	// is the one that matters for an incident timeline.
	if sess.RevokedAt == nil {
		sess.RevokedAt = &at
	}

	return nil
}

func cloneSession(s *store.Session) *store.Session {
	clone := *s

	clone.TokenHash = append([]byte(nil), s.TokenHash...)

	if s.ExpiresAt != nil {
		t := *s.ExpiresAt
		clone.ExpiresAt = &t
	}
	if s.LastUsedAt != nil {
		t := *s.LastUsedAt
		clone.LastUsedAt = &t
	}
	if s.RevokedAt != nil {
		t := *s.RevokedAt
		clone.RevokedAt = &t
	}

	return &clone
}

var _ store.SessionStore = (*sessionStore)(nil)
