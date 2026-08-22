package policy

import (
	"strings"
	"testing"
)

func testSpec() Spec {
	return Spec{
		Version: "test-1",
		Users: []User{
			{ID: "alice", Email: "alice@example.com"},
			{ID: "bob", Email: "bob@example.com"},
			{ID: "carol", Email: "carol@example.com"},
			{ID: "mallory", Email: "mallory@example.com", Disabled: true},
		},
		Groups: []Group{
			{ID: "juniors", Members: []UserID{"carol"}},
			{ID: "backend", Members: []UserID{"alice"}, Includes: []GroupID{"juniors"}},
			{ID: "frontend", Members: []UserID{"bob"}},
			{ID: "admins", Members: []UserID{"alice"}},
		},
		Repositories: []Repository{
			{ID: "backend-api", Remote: "https://github.com/acme/backend-api.git", Forge: "github", DefaultBranch: "main"},
		},
		Rules: map[RepoID][]Rule{
			"backend-api": {
				{
					ID:      "everyone-reads-src",
					Subject: RuleSubject{Type: SubjectTypeAny},
					Paths:   []Pattern{MustParsePattern("src/")},
					Actions: []Action{ActionRead},
					Effect:  EffectAllow,
				},
				{
					ID:      "backend-writes-src",
					Subject: RuleSubject{Type: SubjectTypeGroup, ID: "backend"},
					Paths:   []Pattern{MustParsePattern("src/")},
					Actions: []Action{ActionRead, ActionWrite, ActionCreate, ActionDelete},
					Effect:  EffectAllow,
				},
				{
					ID:      "admins-everything",
					Subject: RuleSubject{Type: SubjectTypeGroup, ID: "admins"},
					Paths:   []Pattern{MustParsePattern("**")},
					Actions: AllActions,
					Effect:  EffectAllow,
				},
				{
					ID:          "nobody-touches-secrets",
					Subject:     RuleSubject{Type: SubjectTypeAny},
					Paths:       []Pattern{MustParsePattern("secrets/"), MustParsePattern("**/*.env")},
					Actions:     AllActions,
					Effect:      EffectDeny,
					Description: "secrets are managed by the platform team",
				},
				{
					ID:      "no-direct-push-to-main",
					Subject: RuleSubject{Type: SubjectTypeAny},
					Paths:   []Pattern{MustParsePattern("**")},
					Refs:    []Pattern{MustParsePattern("refs/heads/main")},
					Actions: []Action{ActionWrite, ActionCreate, ActionDelete},
					Effect:  EffectDeny,
				},
			},
		},
	}
}

func compileTestPolicy(t *testing.T) *Policy {
	t.Helper()

	p, err := Compile(testSpec())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

func subjectOf(t *testing.T, p *Policy, id UserID) Subject {
	t.Helper()

	s, err := p.Subject(id)
	if err != nil {
		t.Fatalf("Subject(%q): %v", id, err)
	}
	return s
}

func TestEvaluate(t *testing.T) {
	p := compileTestPolicy(t)

	cases := []struct {
		name    string
		user    UserID
		ref     string
		path    string
		action  Action
		allowed bool
		reason  Reason
	}{
		{"backend writes own subtree", "alice", "refs/heads/feature/x", "src/app.go", ActionWrite, true, ReasonAllowedByRule},
		{"frontend reads src", "bob", "refs/heads/feature/x", "src/app.go", ActionRead, true, ReasonAllowedByRule},
		{"frontend cannot write src", "bob", "refs/heads/feature/x", "src/app.go", ActionWrite, false, ReasonNoMatchingRule},
		{"nothing matches outside src", "bob", "refs/heads/feature/x", "docs/readme.md", ActionRead, false, ReasonNoMatchingRule},

		// Deny wins even for an admin whose allow rule matches "**".
		{"admin denied on secrets", "alice", "refs/heads/feature/x", "secrets/prod.key", ActionRead, false, ReasonDeniedByRule},
		{"scattered env files denied", "alice", "refs/heads/feature/x", "src/config/local.env", ActionRead, false, ReasonDeniedByRule},

		// Ref scoping.
		{"write to main denied", "alice", "refs/heads/main", "src/app.go", ActionWrite, false, ReasonDeniedByRule},
		{"read on main still allowed", "alice", "refs/heads/main", "src/app.go", ActionRead, true, ReasonAllowedByRule},

		// Transitive membership: carol is in juniors, juniors is included by
		// backend, therefore carol has the backend grants.
		{"transitive group member writes", "carol", "refs/heads/feature/x", "src/app.go", ActionWrite, true, ReasonAllowedByRule},

		{"disabled user denied", "mallory", "refs/heads/feature/x", "src/app.go", ActionRead, false, ReasonUserDisabled},

		// The subtree pattern covers the directory entry itself, not just what
		// is under it: a symlink named "secrets" would otherwise slip through.
		{"subtree covers the directory entry", "alice", "refs/heads/feature/x", "secrets", ActionCreate, false, ReasonDeniedByRule},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := p.Evaluate(Request{
				Repo:    "backend-api",
				Ref:     tc.ref,
				Subject: subjectOf(t, p, tc.user),
				Path:    tc.path,
				Action:  tc.action,
			})

			if d.Allowed != tc.allowed {
				t.Errorf("Allowed = %v, want %v (%s)", d.Allowed, tc.allowed, d)
			}
			if d.Reason != tc.reason {
				t.Errorf("Reason = %q, want %q", d.Reason, tc.reason)
			}
			if d.PolicyVersion != "test-1" {
				t.Errorf("PolicyVersion = %q, want %q", d.PolicyVersion, "test-1")
			}
		})
	}
}

func TestEvaluateUnknownRepository(t *testing.T) {
	p := compileTestPolicy(t)

	d := p.Evaluate(Request{
		Repo:    "does-not-exist",
		Subject: subjectOf(t, p, "alice"),
		Path:    "src/app.go",
		Action:  ActionRead,
	})

	if d.Allowed || d.Reason != ReasonUnknownRepository {
		t.Errorf("got %s, want denial for unknown repository", d)
	}
}

// The outcome must not depend on the order rules appear in the bundle.
func TestEvaluateIsOrderIndependent(t *testing.T) {
	spec := testSpec()
	rules := spec.Rules["backend-api"]

	reversed := make([]Rule, len(rules))
	for i, r := range rules {
		reversed[len(rules)-1-i] = r
	}

	spec.Rules = map[RepoID][]Rule{"backend-api": reversed}

	p, err := Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	d := p.Evaluate(Request{
		Repo:    "backend-api",
		Ref:     "refs/heads/feature/x",
		Subject: subjectOf(t, p, "alice"),
		Path:    "secrets/prod.key",
		Action:  ActionRead,
	})

	if d.Allowed {
		t.Errorf("reversing rule order flipped the decision: %s", d)
	}
}

func TestDecisionCarriesRuleAttribution(t *testing.T) {
	p := compileTestPolicy(t)

	d := p.Evaluate(Request{
		Repo:    "backend-api",
		Ref:     "refs/heads/feature/x",
		Subject: subjectOf(t, p, "alice"),
		Path:    "secrets/prod.key",
		Action:  ActionWrite,
	})

	if d.RuleID != "nobody-touches-secrets" {
		t.Errorf("RuleID = %q, want %q", d.RuleID, "nobody-touches-secrets")
	}
	if d.Pattern != "secrets/" {
		t.Errorf("Pattern = %q, want %q", d.Pattern, "secrets/")
	}
	if d.Description == "" {
		t.Error("Description is empty; a denial must tell the developer why")
	}
}

func TestSubjectResolvesTransitiveGroups(t *testing.T) {
	p := compileTestPolicy(t)

	s := subjectOf(t, p, "carol")

	if !s.InGroup("juniors") {
		t.Error("carol should be in juniors")
	}
	if !s.InGroup("backend") {
		t.Error("carol should be in backend through juniors")
	}
	if s.InGroup("frontend") {
		t.Error("carol should not be in frontend")
	}
}

func TestCompileRejectsGroupCycle(t *testing.T) {
	spec := testSpec()
	spec.Groups = []Group{
		{ID: "a", Includes: []GroupID{"b"}},
		{ID: "b", Includes: []GroupID{"c"}},
		{ID: "c", Includes: []GroupID{"a"}},
	}
	spec.Rules = nil

	_, err := Compile(spec)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("got %v, want a group inclusion cycle error", err)
	}
}

func TestCompileRejectsWriteWithoutRead(t *testing.T) {
	spec := testSpec()
	spec.Rules = map[RepoID][]Rule{
		"backend-api": {{
			ID:      "blind-write",
			Subject: RuleSubject{Type: SubjectTypeUser, ID: "bob"},
			Paths:   []Pattern{MustParsePattern("src/")},
			Actions: []Action{ActionWrite},
			Effect:  EffectAllow,
		}},
	}

	_, err := Compile(spec)
	if err == nil || !strings.Contains(err.Error(), "implies read") {
		t.Errorf("got %v, want a write-without-read error", err)
	}
}

// A deny rule may of course name write alone: denying write while leaving read
// intact is exactly how "read-only for this team" is expressed.
func TestCompileAllowsDenyWriteWithoutRead(t *testing.T) {
	spec := testSpec()
	spec.Rules = map[RepoID][]Rule{
		"backend-api": {{
			ID:      "read-only-team",
			Subject: RuleSubject{Type: SubjectTypeGroup, ID: "frontend"},
			Paths:   []Pattern{MustParsePattern("src/")},
			Actions: []Action{ActionWrite, ActionCreate, ActionDelete},
			Effect:  EffectDeny,
		}},
	}

	if _, err := Compile(spec); err != nil {
		t.Errorf("Compile: %v", err)
	}
}

func TestCompileRejectsUnknownReferences(t *testing.T) {
	cases := map[string]func(*Spec){
		"unknown user in group": func(s *Spec) {
			s.Groups = append(s.Groups, Group{ID: "ghosts", Members: []UserID{"nobody"}})
		},
		"unknown group in rule": func(s *Spec) {
			s.Rules["backend-api"] = []Rule{{
				Subject: RuleSubject{Type: SubjectTypeGroup, ID: "nope"},
				Paths:   []Pattern{MustParsePattern("**")},
				Actions: []Action{ActionRead},
				Effect:  EffectAllow,
			}}
		},
		"rules for unknown repository": func(s *Spec) {
			s.Rules["nope"] = []Rule{{
				Subject: RuleSubject{Type: SubjectTypeAny},
				Paths:   []Pattern{MustParsePattern("**")},
				Actions: []Action{ActionRead},
				Effect:  EffectAllow,
			}}
		},
		"duplicate user": func(s *Spec) {
			s.Users = append(s.Users, User{ID: "alice"})
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			mutate(&spec)

			if _, err := Compile(spec); err == nil {
				t.Error("expected a compile error")
			}
		})
	}
}

func TestCachingEngine(t *testing.T) {
	p := compileTestPolicy(t)

	base := Request{
		Repo:    "backend-api",
		Ref:     "refs/heads/feature/x",
		Subject: subjectOf(t, p, "alice"),
	}

	counted := &countingEngine{inner: p}
	cached := NewCachingEngine(counted, base)

	for range 10 {
		if d := cached.Evaluate("src/app.go", ActionWrite); !d.Allowed {
			t.Fatalf("unexpected denial: %s", d)
		}
	}

	if counted.calls != 1 {
		t.Errorf("underlying engine called %d times, want 1", counted.calls)
	}
}

type countingEngine struct {
	inner Engine
	calls int
}

func (c *countingEngine) Evaluate(req Request) Decision {
	c.calls++
	return c.inner.Evaluate(req)
}
