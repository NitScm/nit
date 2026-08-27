package policy_test

import (
	"testing"

	"github.com/NitScm/nit/pkg/policy"
)

// AllowsAll's success path.
//
// It had none. A mutation turning its final `return true` into `return false`
// survived the entire suite, which means nothing anywhere asked it about a set
// of paths that are all allowed. It is exported, so somebody outside this module
// may be relying on an answer no test had ever checked.
func TestAllowsAllAnswersYesWhenEverythingIsAllowed(t *testing.T) {
	p, err := policy.Compile(policy.Spec{
		Version:      "v1",
		Users:        []policy.User{{ID: "dev"}},
		Groups:       []policy.Group{{ID: "devs", Members: []policy.UserID{"dev"}}},
		Repositories: []policy.Repository{{ID: "repo"}},
		Rules: map[policy.RepoID][]policy.Rule{
			"repo": {{
				ID:      "devs-own-src",
				Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "devs"},
				Paths:   []policy.Pattern{policy.MustParsePattern("src/")},
				Actions: []policy.Action{policy.ActionRead, policy.ActionWrite},
				Effect:  policy.EffectAllow,
			}},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	subject, err := p.Subject("dev")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	base := policy.Request{Repo: "repo", Subject: subject}

	ok, d := policy.AllowsAll(p, base,
		[]string{"src/a.go", "src/b.go"}, policy.ActionRead, policy.ActionWrite)

	if !ok {
		t.Fatalf("AllowsAll said no for paths that are all allowed: %+v", d)
	}

	// The refusing decision comes back, not merely the boolean, or a caller
	// cannot tell somebody which path stopped them — which is the sentence the
	// whole product is built around.
	ok, d = policy.AllowsAll(p, base,
		[]string{"src/a.go", "secrets/prod.env"}, policy.ActionRead)

	if ok {
		t.Fatal("AllowsAll said yes with a path outside every rule")
	}

	if d.Allowed {
		t.Fatal("the returned decision says allowed")
	}

	if d.Reason == "" {
		t.Error("the returned decision carries no reason; a caller cannot say why")
	}
}

// An empty set is allowed. It reads like a triviality and it is the kind of
// edge a caller hits with a patch that touches nothing — a merge commit, an
// empty push — where refusing would be a refusal nobody could explain.
func TestAllowsAllOnNothingIsAllowed(t *testing.T) {
	p, err := policy.Compile(policy.Spec{
		Version:      "v1",
		Users:        []policy.User{{ID: "dev"}},
		Repositories: []policy.Repository{{ID: "repo"}},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	subject, err := p.Subject("dev")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	if ok, d := policy.AllowsAll(p, policy.Request{Repo: "repo", Subject: subject},
		nil, policy.ActionRead); !ok {
		t.Fatalf("an empty set of paths was refused: %+v", d)
	}
}
