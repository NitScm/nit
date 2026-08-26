package policy_test

import (
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/policy"
)

// The change this exists for.
//
// Nobody touched a rule. One person joined a group, in a different file, and
// the diff of the YAML is one line that names neither the repository nor the
// paths it opens. That is the review a large organization cannot do by reading.
func TestJoiningAGroupShowsUpAsWhatItGrants(t *testing.T) {
	before := compile(t, spec(
		[]policy.User{{ID: "alice"}, {ID: "carol"}},
		[]policy.Group{{ID: "backend", Members: []policy.UserID{"alice"}}},
		rule("r-config", policy.SubjectTypeGroup, "backend", policy.EffectAllow,
			[]string{"config/**"}, policy.ActionRead),
	))

	after := compile(t, spec(
		[]policy.User{{ID: "alice"}, {ID: "carol"}},
		[]policy.Group{{ID: "backend", Members: []policy.UserID{"alice", "carol"}}},
		rule("r-config", policy.SubjectTypeGroup, "backend", policy.EffectAllow,
			[]string{"config/**"}, policy.ActionRead),
	))

	diff := policy.Compare(before, after)

	if len(diff.Changes) != 1 || diff.Changes[0].User != "carol" {
		t.Fatalf("changes = %+v, want carol only", diff.Changes)
	}

	gained := diff.Changes[0].Allowed
	if len(gained) != 1 {
		t.Fatalf("carol gained %d grants, want 1", len(gained))
	}

	if gained[0].Path != "config/**" || gained[0].Action != policy.ActionRead {
		t.Errorf("gained %s", gained[0])
	}

	// The column that answers "how did they get it?", which is where somebody
	// notices a group has grown a member nobody meant to add.
	if gained[0].Via != "backend" {
		t.Errorf("via = %q, want the group that brought it", gained[0].Via)
	}

	if !diff.Changes[0].Widens() {
		t.Error("gaining an allow is not reported as widening")
	}
}

// Deny wins, so the four directions are four different facts. A gained deny is
// less access; a lost deny is more.
func TestADeletedDenyIsReportedAsWidening(t *testing.T) {
	deny := rule("r-secrets", policy.SubjectTypeAny, "", policy.EffectDeny,
		[]string{"secrets/**"}, policy.ActionRead)

	allow := rule("r-all", policy.SubjectTypeAny, "", policy.EffectAllow,
		[]string{"**"}, policy.ActionRead)

	before := compile(t, spec([]policy.User{{ID: "alice"}}, nil, deny, allow))
	after := compile(t, spec([]policy.User{{ID: "alice"}}, nil, allow))

	diff := policy.Compare(before, after)

	if len(diff.Changes) != 1 {
		t.Fatalf("changes = %+v", diff.Changes)
	}

	change := diff.Changes[0]

	if len(change.NoLongerDenied) != 1 {
		t.Fatalf("lost denies = %+v, want the one that was deleted", change.NoLongerDenied)
	}

	// The whole reason the four lists are separate. Removing lines rarely looks
	// alarming in a text diff, and this is the change most worth alarm.
	if !change.Widens() {
		t.Error("deleting a deny was not reported as widening")
	}

	if change.Narrows() {
		t.Error("deleting a deny was also reported as narrowing")
	}

	if len(diff.Widening()) != 1 {
		t.Errorf("Widening() = %d, want 1", len(diff.Widening()))
	}
}

// A gained deny is a loss for the person, and has to read that way.
func TestAnAddedDenyIsReportedAsNarrowing(t *testing.T) {
	allow := rule("r-all", policy.SubjectTypeAny, "", policy.EffectAllow,
		[]string{"**"}, policy.ActionRead)

	deny := rule("r-secrets", policy.SubjectTypeAny, "", policy.EffectDeny,
		[]string{"secrets/**"}, policy.ActionRead)

	before := compile(t, spec([]policy.User{{ID: "alice"}}, nil, allow))
	after := compile(t, spec([]policy.User{{ID: "alice"}}, nil, allow, deny))

	diff := policy.Compare(before, after)

	change := diff.Changes[0]

	if len(change.Denied) != 1 {
		t.Fatalf("gained denies = %+v", change.Denied)
	}

	if change.Widens() {
		t.Error("adding a deny was reported as widening")
	}

	if !change.Narrows() {
		t.Error("adding a deny was not reported as narrowing")
	}
}

// Rewriting a bundle without changing what it means reports nothing.
//
// A tool that cried on every reorder would be ignored within a week, and the
// engine is order-independent precisely so that reordering is safe.
func TestReorderingRulesChangesNothing(t *testing.T) {
	a := rule("r-a", policy.SubjectTypeAny, "", policy.EffectAllow, []string{"src/**"}, policy.ActionRead)
	b := rule("r-b", policy.SubjectTypeAny, "", policy.EffectDeny, []string{"secrets/**"}, policy.ActionRead)

	before := compile(t, spec([]policy.User{{ID: "alice"}}, nil, a, b))
	after := compile(t, spec([]policy.User{{ID: "alice"}}, nil, b, a))

	diff := policy.Compare(before, after)

	if !diff.Empty() {
		t.Errorf("reordering reported %+v", diff)
	}
}

// Somebody who left, and somebody who arrived, are reported as such rather
// than as a wall of grant changes nobody needs to read.
func TestPeopleComingAndGoingAreSaidOnce(t *testing.T) {
	allow := rule("r-all", policy.SubjectTypeAny, "", policy.EffectAllow,
		[]string{"**"}, policy.ActionRead)

	before := compile(t, spec([]policy.User{{ID: "alice"}, {ID: "bob"}}, nil, allow))
	after := compile(t, spec([]policy.User{{ID: "alice"}, {ID: "carol"}}, nil, allow))

	diff := policy.Compare(before, after)

	if len(diff.UsersAdded) != 1 || diff.UsersAdded[0] != "carol" {
		t.Errorf("added = %v", diff.UsersAdded)
	}

	if len(diff.UsersRemoved) != 1 || diff.UsersRemoved[0] != "bob" {
		t.Errorf("removed = %v", diff.UsersRemoved)
	}

	// And neither appears in Changes, which is for people in both bundles.
	for _, change := range diff.Changes {
		if change.User == "carol" || change.User == "bob" {
			t.Errorf("%s is both a membership change and an arrival", change.User)
		}
	}
}

// Widening a path pattern is the commonest permission change there is, and it
// touches no group, no person and no rule id.
//
// This was missing, and a mutation that dropped the path from a grant's
// identity survived every other test in this file: config/** and ** collapsed
// into one grant, and the diff came back empty.
func TestWideningAPathIsAChange(t *testing.T) {
	narrow := rule("r-1", policy.SubjectTypeAny, "", policy.EffectAllow,
		[]string{"config/**"}, policy.ActionRead)

	wide := rule("r-1", policy.SubjectTypeAny, "", policy.EffectAllow,
		[]string{"**"}, policy.ActionRead)

	before := compile(t, spec([]policy.User{{ID: "alice"}}, nil, narrow))
	after := compile(t, spec([]policy.User{{ID: "alice"}}, nil, wide))

	diff := policy.Compare(before, after)

	if diff.Empty() {
		t.Fatal("changing config/** to ** reported nothing")
	}

	change := diff.Changes[0]

	if len(change.Allowed) != 1 || change.Allowed[0].Path != "**" {
		t.Errorf("gained = %+v, want the wider path", change.Allowed)
	}

	if len(change.NoLongerAllowed) != 1 || change.NoLongerAllowed[0].Path != "config/**" {
		t.Errorf("lost = %+v, want the narrower path", change.NoLongerAllowed)
	}

	if !change.Widens() {
		t.Error("a wider path was not reported as widening")
	}
}

// The same for actions and refs: neither is in a rule's id either.
func TestGainingAnActionOrLosingARefIsAChange(t *testing.T) {
	readOnly := rule("r-1", policy.SubjectTypeAny, "", policy.EffectAllow,
		[]string{"src/**"}, policy.ActionRead)

	readWrite := rule("r-1", policy.SubjectTypeAny, "", policy.EffectAllow,
		[]string{"src/**"}, policy.ActionRead, policy.ActionWrite)

	before := compile(t, spec([]policy.User{{ID: "alice"}}, nil, readOnly))
	after := compile(t, spec([]policy.User{{ID: "alice"}}, nil, readWrite))

	gained := policy.Compare(before, after).Changes[0].Allowed

	if len(gained) != 1 || gained[0].Action != policy.ActionWrite {
		t.Errorf("gained = %+v, want write", gained)
	}

	// A rule that stops being restricted to one branch now applies to every
	// branch, which is a widening that changes no path and no action.
	onMain := readOnly
	onMain.Refs = []policy.Pattern{policy.MustParsePattern("refs/heads/main")}

	restricted := compile(t, spec([]policy.User{{ID: "alice"}}, nil, onMain))
	everywhere := compile(t, spec([]policy.User{{ID: "alice"}}, nil, readOnly))

	loosened := policy.Compare(restricted, everywhere).Changes

	if len(loosened) != 1 || !loosened[0].Widens() {
		t.Errorf("dropping a ref restriction reported %+v", loosened)
	}
}

// A grant reads as a sentence, because it goes in front of a reviewer.
func TestAGrantSaysWhatItIs(t *testing.T) {
	before := compile(t, spec([]policy.User{{ID: "alice"}}, nil))
	after := compile(t, spec([]policy.User{{ID: "alice"}}, nil,
		rule("r-1", policy.SubjectTypeUser, "alice", policy.EffectAllow,
			[]string{"src/**"}, policy.ActionWrite, policy.ActionRead)))

	diff := policy.Compare(before, after)
	text := diff.Changes[0].Allowed[0].String()

	for _, want := range []string{"allow", "src/**", "r-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q does not carry %q", text, want)
		}
	}
}

// ---------------------------------------------------------------------------

func compile(t *testing.T, s policy.Spec) *policy.Policy {
	t.Helper()

	p, err := policy.Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	return p
}

func spec(users []policy.User, groups []policy.Group, rules ...policy.Rule) policy.Spec {
	return policy.Spec{
		Users:        users,
		Groups:       groups,
		Repositories: []policy.Repository{{ID: "payments"}},
		Rules:        map[policy.RepoID][]policy.Rule{"payments": rules},
	}
}

func rule(id string, kind policy.SubjectType, subject string, effect policy.Effect,
	paths []string, actions ...policy.Action) policy.Rule {
	patterns := make([]policy.Pattern, 0, len(paths))
	for _, p := range paths {
		patterns = append(patterns, policy.MustParsePattern(p))
	}

	return policy.Rule{
		ID:      id,
		Subject: policy.RuleSubject{Type: kind, ID: subject},
		Paths:   patterns,
		Actions: actions,
		Effect:  effect,
	}
}
