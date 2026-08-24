package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

var now = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

type harness struct {
	store   *memory.Store
	service *auth.Service
	user    *store.User
	clock   time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx := context.Background()
	s := memory.New()

	compiled, err := policy.Compile(policy.Spec{
		Version: "test-1",
		Users: []policy.User{
			{ID: "alice"},
			{ID: "ghost"},
		},
		Groups: []policy.Group{
			{ID: "devs", Members: []policy.UserID{"alice"}},
		},
		Repositories: []policy.Repository{{ID: "backend-api"}},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	user, err := s.Users().Upsert(ctx, &store.User{
		TenantID:     policy.DefaultTenant,
		PolicyUserID: "alice",
		Email:        "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	h := &harness{store: s, user: user, clock: now}
	h.service = auth.NewService(s, policy.OneSource{Source: policyloader.NewStatic(compiled)}, policy.DefaultTenant,
		func() time.Time { return h.clock })

	return h
}

func TestIssueAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	token, session, err := h.service.Issue(ctx, h.user.ID, "laptop", 24*time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if !strings.HasPrefix(token, auth.TokenPrefix) {
		t.Errorf("token %q lacks the %q prefix that lets scanners recognize it", token, auth.TokenPrefix)
	}
	if len(token) < 40 {
		t.Errorf("token is only %d characters; that is not enough entropy", len(token))
	}

	// The plaintext must be nowhere in the stored record.
	if strings.Contains(string(session.TokenHash), token) {
		t.Error("the stored session contains the plaintext token")
	}

	principal, err := h.service.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if principal.User.ID != h.user.ID {
		t.Errorf("got user %s, want %s", principal.User.ID, h.user.ID)
	}
	if !principal.Subject.InGroup("devs") {
		t.Error("groups were not expanded from the policy bundle")
	}
	if principal.PolicyVersion != "test-1" {
		t.Errorf("PolicyVersion = %q, want the bundle in force", principal.PolicyVersion)
	}
}

func TestAuthenticateRejections(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		setup func(t *testing.T, h *harness) string
		want  error
	}{
		{
			name:  "empty",
			setup: func(*testing.T, *harness) string { return "" },
			want:  auth.ErrNoCredentials,
		},
		{
			name:  "wrong prefix",
			setup: func(*testing.T, *harness) string { return "github_pat_whatever" },
			want:  auth.ErrMalformed,
		},
		{
			name: "unknown token",
			setup: func(t *testing.T, h *harness) string {
				token, _, err := auth.GenerateToken()
				if err != nil {
					t.Fatalf("GenerateToken: %v", err)
				}
				return token
			},
			want: auth.ErrUnknownToken,
		},
		{
			name: "expired",
			setup: func(t *testing.T, h *harness) string {
				token, _, err := h.service.Issue(ctx, h.user.ID, "laptop", time.Hour)
				if err != nil {
					t.Fatalf("Issue: %v", err)
				}
				h.clock = now.Add(2 * time.Hour)
				return token
			},
			want: auth.ErrExpired,
		},
		{
			name: "revoked",
			setup: func(t *testing.T, h *harness) string {
				token, session, err := h.service.Issue(ctx, h.user.ID, "laptop", time.Hour)
				if err != nil {
					t.Fatalf("Issue: %v", err)
				}
				if err := h.service.Revoke(ctx, session.ID); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
				return token
			},
			want: auth.ErrRevoked,
		},
		{
			name: "disabled account",
			setup: func(t *testing.T, h *harness) string {
				token, _, err := h.service.Issue(ctx, h.user.ID, "laptop", time.Hour)
				if err != nil {
					t.Fatalf("Issue: %v", err)
				}

				disabled := *h.user
				disabled.Disabled = true
				if _, err := h.store.Users().Upsert(ctx, &disabled); err != nil {
					t.Fatalf("Upsert: %v", err)
				}
				return token
			},
			want: auth.ErrUserDisabled,
		},
		{
			name: "account missing from the policy bundle",
			setup: func(t *testing.T, h *harness) string {
				stranger, err := h.store.Users().Upsert(ctx, &store.User{
					TenantID:     policy.DefaultTenant,
					PolicyUserID: "not-in-bundle",
				})
				if err != nil {
					t.Fatalf("Upsert: %v", err)
				}

				token, _, err := h.service.Issue(ctx, stranger.ID, "laptop", time.Hour)
				if err != nil {
					t.Fatalf("Issue: %v", err)
				}
				return token
			},
			want: auth.ErrUnknownSubject,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			token := tc.setup(t, h)

			_, err := h.service.Authenticate(ctx, token)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// Each rejection has its own error because the right advice differs: "log in
// again" for an expired token, "talk to an administrator" for a disabled
// account, "something is wrong" for an unknown one.
func TestRejectionsAreDistinguishable(t *testing.T) {
	distinct := []error{
		auth.ErrNoCredentials,
		auth.ErrMalformed,
		auth.ErrUnknownToken,
		auth.ErrExpired,
		auth.ErrRevoked,
		auth.ErrUserDisabled,
		auth.ErrUnknownSubject,
	}

	for i, a := range distinct {
		for j, b := range distinct {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v and %v are not distinguishable", a, b)
			}
		}
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := make(map[string]bool)

	for range 1000 {
		token, hash, err := auth.GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		if seen[token] {
			t.Fatal("GenerateToken produced a duplicate")
		}
		if len(hash) != 32 {
			t.Fatalf("hash is %d bytes, want 32", len(hash))
		}

		seen[token] = true
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
		err    error
	}{
		"valid":            {"Bearer nit_abc", "nit_abc", nil},
		"lowercase scheme": {"bearer nit_abc", "nit_abc", nil},
		"empty":            {"", "", auth.ErrNoCredentials},
		"no scheme":        {"nit_abc", "", auth.ErrMalformed},
		"wrong scheme":     {"Basic dXNlcjpwYXNz", "", auth.ErrMalformed},
		"scheme only":      {"Bearer ", "", auth.ErrMalformed},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := auth.BearerToken(tc.header)

			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPrincipalContext(t *testing.T) {
	ctx := context.Background()

	if auth.PrincipalFrom(ctx) != nil {
		t.Error("an empty context must carry no principal")
	}

	p := &auth.Principal{PolicyVersion: "test-1"}

	if got := auth.PrincipalFrom(auth.WithPrincipal(ctx, p)); got != p {
		t.Error("principal did not survive the context round trip")
	}
}
