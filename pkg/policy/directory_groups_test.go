package policy_test

import (
	"testing"

	"github.com/NitScm/nit/pkg/policy"
)

// A bundle that names directory groups has to compile without a directory,
// because that is what CI has: the files and nothing else.
func bundleReferringToADirectory() policy.Spec {
	return policy.Spec{
		Tenant:  "acme",
		Version: "v1",
		Users:   []policy.User{{ID: "alice"}},
		Groups: []policy.Group{
			{ID: "platform", Members: []policy.UserID{"alice"}, Includes: []policy.GroupID{"idp:platform"}},
			{ID: "payments", Includes: []policy.GroupID{"idp:payments"}},
		},
		Repositories: []policy.Repository{{ID: "backend-api"}},
		Rules: map[policy.RepoID][]policy.Rule{
			"backend-api": {
				{
					ID:      "payments-owns-billing",
					Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "payments"},
					Paths:   []policy.Pattern{policy.MustParsePattern("src/billing/")},
					Actions: []policy.Action{policy.ActionRead, policy.ActionWrite},
					Effect:  policy.EffectAllow,
				},
			},
		},
	}
}

func TestABundleCompilesWithoutTheDirectoryItNames(t *testing.T) {
	p, err := policy.Compile(bundleReferringToADirectory())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	var found bool

	for _, g := range p.Groups() {
		if g.ID == "idp:platform" {
			found = true

			if len(g.Members) != 0 {
				t.Fatalf("the directory group has members nobody supplied: %v", g.Members)
			}
		}
	}

	if !found {
		t.Fatal("the directory group was not declared at all")
	}
}

// The point of compiling without the directory is that nobody gains anything by
// it. If an empty directory group granted access, CI would be validating a
// bundle that is more permissive than the one that runs.
func TestNobodyReachesAnythingThroughADirectoryGroupThatIsEmpty(t *testing.T) {
	p, err := policy.Compile(bundleReferringToADirectory())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	subject, err := p.Subject("alice")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	for _, g := range subject.Groups {
		if g == "payments" || g == "idp:payments" {
			t.Fatalf("alice is in %q, which nothing put her in", g)
		}
	}

	d := p.Evaluate(policy.Request{
		Repo: "backend-api", Subject: subject,
		Path: "src/billing/invoice.go", Action: policy.ActionWrite,
	})

	if d.Allowed {
		t.Fatal("an empty directory group granted a write")
	}
}

// A typo in an ordinary group name is still a typo. Only the reserved prefix is
// forgiven, and only because something else is expected to fill it in.
func TestAnUnknownGroupWithoutThePrefixIsStillRefused(t *testing.T) {
	spec := bundleReferringToADirectory()
	spec.Groups[0].Includes = []policy.GroupID{"platfrom"}

	if _, err := policy.Compile(spec); err == nil {
		t.Fatal("a misspelled include compiled")
	}
}

// A rule may name a directory group directly, without a bundle group wrapping
// it. Adoption has to see that too, or the bundle stops compiling the moment
// somebody writes the shorter form.
func TestARuleMayNameADirectoryGroupDirectly(t *testing.T) {
	spec := bundleReferringToADirectory()
	spec.Rules["backend-api"] = append(spec.Rules["backend-api"], policy.Rule{
		ID:      "release-is-platform-only",
		Subject: policy.RuleSubject{Type: policy.SubjectTypeAny},
		Except:  []policy.RuleSubject{{Type: policy.SubjectTypeGroup, ID: "idp:release-managers"}},
		Paths:   []policy.Pattern{policy.MustParsePattern("**")},
		Actions: []policy.Action{policy.ActionWrite},
		Effect:  policy.EffectDeny,
	})

	if _, err := policy.Compile(spec); err != nil {
		t.Fatalf("Compile: %v", err)
	}
}
