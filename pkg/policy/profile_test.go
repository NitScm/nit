package policy

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// The contract, tested the only way it can honestly be tested: exhaustively,
// over every subject pair and a corpus of paths chosen to exercise every rule
// in the fixture.
//
// A profile collision between two subjects with different rights is not a
// performance bug. It is one developer receiving another's files, delivered by
// the component whose job is to prevent exactly that. So the assertion runs the
// implication in the direction that matters: equal profiles must imply equal
// decisions, for every path, at every ref.
func TestEqualProfilesDecideAlike(t *testing.T) {
	p := compileTestPolicy(t)

	users := []UserID{"alice", "bob", "carol", "mallory"}
	refs := []string{"refs/heads/main", "refs/heads/feature/x"}
	paths := []string{
		"src/app.go",
		"src/config/local.env",
		"secrets/prod.key",
		"docs/readme.md",
		"README.md",
		"src/deep/nested/file.txt",
		".github/workflows/ci.yml",
	}

	for _, action := range AllActions {
		for _, ref := range refs {
			for _, a := range users {
				for _, b := range users {
					subjectA := subjectOf(t, p, a)
					subjectB := subjectOf(t, p, b)

					if p.Profile("backend-api", ref, action, subjectA) !=
						p.Profile("backend-api", ref, action, subjectB) {
						continue
					}

					for _, path := range paths {
						da := p.Evaluate(Request{
							Repo: "backend-api", Ref: ref, Subject: subjectA,
							Path: path, Action: action,
						})
						db := p.Evaluate(Request{
							Repo: "backend-api", Ref: ref, Subject: subjectB,
							Path: path, Action: action,
						})

						if da.Allowed != db.Allowed {
							t.Errorf("%s and %s share a profile for %s on %s but %s: %v vs %v",
								a, b, action, ref, path, da.Allowed, db.Allowed)
						}
					}
				}
			}
		}
	}
}

// The same contract against policies nobody wrote by hand. A fixture tests the
// rules someone thought of; this tests the ones they did not.
func TestEqualProfilesDecideAlikeOnGeneratedPolicies(t *testing.T) {
	paths := []string{
		"a.go", "src/a.go", "src/b/c.go", "secrets/k", "docs/d.md",
		"x.env", "src/x.env", "deep/deep/deep/file", "", "/",
	}

	for seed := range 200 {
		p := generatePolicy(t, uint64(seed))

		var subjects []Subject
		for i := range 6 {
			id := UserID(fmt.Sprintf("u%d", i))

			s, err := p.Subject(id)
			if err != nil {
				t.Fatalf("Subject(%q): %v", id, err)
			}

			subjects = append(subjects, s)
		}

		for _, ref := range []string{"refs/heads/main", "refs/heads/topic"} {
			for _, action := range AllActions {
				byProfile := map[string]Subject{}

				for _, s := range subjects {
					profile := p.Profile("repo", ref, action, s)

					other, seen := byProfile[profile]
					if !seen {
						byProfile[profile] = s
						continue
					}

					for _, path := range paths {
						da := p.Evaluate(Request{Repo: "repo", Ref: ref, Subject: s, Path: path, Action: action})
						db := p.Evaluate(Request{Repo: "repo", Ref: ref, Subject: other, Path: path, Action: action})

						if da.Allowed != db.Allowed {
							t.Fatalf("seed %d: %s and %s share a profile for %s on %s "+
								"but disagree on %q: %v vs %v",
								seed, s.UserID, other.UserID, action, ref, path, da.Allowed, db.Allowed)
						}
					}
				}
			}
		}
	}
}

// Sharing has to actually happen, or the profile is sound and useless: a
// fingerprint that gave every subject a distinct value would pass the test
// above and cache nothing.
func TestProfilesAreSharedBetweenEquivalentSubjects(t *testing.T) {
	p, err := Compile(Spec{
		Version: "shared-1",
		Users: []User{
			{ID: "alice", Email: "a@example.com"},
			{ID: "bob", Email: "b@example.com"},
			{ID: "carol", Email: "c@example.com"},
		},
		Groups: []Group{
			{ID: "readers", Members: []UserID{"alice", "bob"}},
			{ID: "writers", Members: []UserID{"carol"}},
		},
		Repositories: []Repository{{ID: "repo", Remote: "https://example.com/r.git", Forge: "github", DefaultBranch: "main"}},
		Rules: map[RepoID][]Rule{
			"repo": {{
				ID:      "readers-read",
				Subject: RuleSubject{Type: SubjectTypeGroup, ID: "readers"},
				Paths:   []Pattern{MustParsePattern("**")},
				Actions: []Action{ActionRead},
				Effect:  EffectAllow,
			}},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	alice := subjectOf(t, p, "alice")
	bob := subjectOf(t, p, "bob")
	carol := subjectOf(t, p, "carol")

	ref := "refs/heads/main"

	if p.Profile("repo", ref, ActionRead, alice) != p.Profile("repo", ref, ActionRead, bob) {
		t.Error("two members of the same group have different profiles; nothing would ever be shared")
	}
	if p.Profile("repo", ref, ActionRead, alice) == p.Profile("repo", ref, ActionRead, carol) {
		t.Error("a reader and a non-reader share a profile")
	}
}

// A profile is only meaningful inside one compiled bundle. Rule positions are
// what it hashes, and the same position means a different rule after an edit.
func TestProfileChangesWithThePolicyVersion(t *testing.T) {
	spec := testSpec()

	first, err := Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	spec.Version = "test-2"

	second, err := Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	alice := subjectOf(t, first, "alice")

	if first.Profile("backend-api", "refs/heads/main", ActionRead, alice) ==
		second.Profile("backend-api", "refs/heads/main", ActionRead, alice) {
		t.Error("the profile survived a version change; a cache keyed on it would " +
			"serve a projection computed under rules that no longer apply")
	}
}

// The ref is part of the key because rules carry ref patterns. Sharing a
// profile across refs would deliver a branch's projection for another branch.
func TestProfileDistinguishesRefsAndActions(t *testing.T) {
	p := compileTestPolicy(t)
	alice := subjectOf(t, p, "alice")

	main := p.Profile("backend-api", "refs/heads/main", ActionWrite, alice)
	topic := p.Profile("backend-api", "refs/heads/feature/x", ActionWrite, alice)

	if main == topic {
		t.Error("refs/heads/main and a feature branch share a write profile, " +
			"though no-direct-push-to-main applies to only one of them")
	}

	if p.Profile("backend-api", "refs/heads/main", ActionRead, alice) == main {
		t.Error("read and write share a profile")
	}
}

// Every disabled user is denied everything, so they may share one profile — but
// they must not share it with anybody who is not disabled.
func TestDisabledUsersShareAProfileWithNobodyElse(t *testing.T) {
	p := compileTestPolicy(t)

	mallory := subjectOf(t, p, "mallory")
	alice := subjectOf(t, p, "alice")

	ref := "refs/heads/main"

	if p.Profile("backend-api", ref, ActionRead, mallory) == p.Profile("backend-api", ref, ActionRead, alice) {
		t.Error("a disabled user shares a profile with an enabled one")
	}
}

// generatePolicy builds a policy with overlapping groups, exemptions and ref
// restrictions — the shapes that make two subjects' rule sets differ in ways a
// hand-written fixture rarely covers.
func generatePolicy(t *testing.T, seed uint64) *Policy {
	t.Helper()

	rng := rand.New(rand.NewPCG(seed, 0x6e6974))

	spec := Spec{
		Version:      fmt.Sprintf("generated-%d", seed),
		Repositories: []Repository{{ID: "repo", Remote: "https://example.com/r.git", Forge: "github", DefaultBranch: "main"}},
	}

	const users, groups = 6, 3

	for i := range users {
		spec.Users = append(spec.Users, User{
			ID:       UserID(fmt.Sprintf("u%d", i)),
			Email:    fmt.Sprintf("u%d@example.com", i),
			Disabled: rng.IntN(6) == 0,
		})
	}

	for g := range groups {
		group := Group{ID: GroupID(fmt.Sprintf("g%d", g))}

		for i := range users {
			if rng.IntN(2) == 0 {
				group.Members = append(group.Members, UserID(fmt.Sprintf("u%d", i)))
			}
		}

		spec.Groups = append(spec.Groups, group)
	}

	patterns := []string{"**", "src/", "secrets/", "**/*.env", "docs/", "deep/deep/"}

	// Write access implies read access, and the compiler enforces it. Drawing
	// from valid combinations keeps the generator producing policies a real
	// bundle could contain.
	actionSets := [][]Action{
		{ActionRead},
		{ActionRead, ActionWrite},
		{ActionRead, ActionWrite, ActionCreate},
		AllActions,
	}

	var rules []Rule

	for r := range 1 + rng.IntN(6) {
		rule := Rule{
			ID:      fmt.Sprintf("r%d", r),
			Paths:   []Pattern{MustParsePattern(patterns[rng.IntN(len(patterns))])},
			Actions: actionSets[rng.IntN(len(actionSets))],
			Effect:  EffectAllow,
		}

		if rng.IntN(3) == 0 {
			rule.Effect = EffectDeny
		}

		switch rng.IntN(3) {
		case 0:
			rule.Subject = RuleSubject{Type: SubjectTypeAny}
		case 1:
			rule.Subject = RuleSubject{Type: SubjectTypeGroup, ID: fmt.Sprintf("g%d", rng.IntN(groups))}
		default:
			rule.Subject = RuleSubject{Type: SubjectTypeUser, ID: fmt.Sprintf("u%d", rng.IntN(users))}
		}

		// Exemptions are the case that makes "same group" and "same rights"
		// different things.
		if rng.IntN(3) == 0 {
			rule.Except = []RuleSubject{{Type: SubjectTypeUser, ID: fmt.Sprintf("u%d", rng.IntN(users))}}
		}

		if rng.IntN(3) == 0 {
			rule.Refs = []Pattern{MustParsePattern("refs/heads/main")}
		}

		rules = append(rules, rule)
	}

	spec.Rules = map[RepoID][]Rule{"repo": rules}

	p, err := Compile(spec)
	if err != nil {
		t.Fatalf("seed %d: Compile: %v", seed, err)
	}

	return p
}

// Ref filtering earns its place by *sharing*, not by safety.
//
// Safety comes from hashing the ref itself: two refs can never share a profile
// whatever the rule set. Dropping the MatchesRef check would therefore leak
// nothing — it would only split subjects who differ in a rule that does not
// apply at this ref, and make the cache miss for no reason.
//
// So this asserts the sharing, which is the property that would otherwise
// regress silently: a rule restricted to main must not separate two subjects
// asking about a topic branch.
func TestARuleThatCannotApplyDoesNotSplitAProfile(t *testing.T) {
	p, err := Compile(Spec{
		Version: "refs-1",
		Users: []User{
			{ID: "alice", Email: "a@example.com"},
			{ID: "bob", Email: "b@example.com"},
		},
		Groups:       []Group{{ID: "releasers", Members: []UserID{"alice"}}},
		Repositories: []Repository{{ID: "repo", Remote: "https://example.com/r.git", Forge: "github", DefaultBranch: "main"}},
		Rules: map[RepoID][]Rule{
			"repo": {
				{
					ID:      "everyone-reads",
					Subject: RuleSubject{Type: SubjectTypeAny},
					Paths:   []Pattern{MustParsePattern("**")},
					Actions: []Action{ActionRead},
					Effect:  EffectAllow,
				},
				{
					// Applies to alice, and only on main.
					ID:      "releasers-write-main",
					Subject: RuleSubject{Type: SubjectTypeGroup, ID: "releasers"},
					Paths:   []Pattern{MustParsePattern("**")},
					Refs:    []Pattern{MustParsePattern("refs/heads/main")},
					Actions: []Action{ActionRead, ActionWrite},
					Effect:  EffectAllow,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	alice := subjectOf(t, p, "alice")
	bob := subjectOf(t, p, "bob")

	topic := "refs/heads/topic"

	if p.Profile("repo", topic, ActionRead, alice) != p.Profile("repo", topic, ActionRead, bob) {
		t.Error("a main-only rule split two profiles on a topic branch; " +
			"the cache would miss for every release manager on every feature branch")
	}

	// And on main, where the rule does apply, they must not be shared.
	if p.Profile("repo", "refs/heads/main", ActionWrite, alice) ==
		p.Profile("repo", "refs/heads/main", ActionWrite, bob) {
		t.Error("a rule that applies to only one of them left their profiles equal")
	}
}
