package policy_test

import (
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/policy"
)

// The failure this whole file exists to prevent.
//
// Everything is denied by default, so `expect: deny` holds whether the deny
// rule is there or not. A file of "nobody outside security reads secrets"
// assertions stays green after somebody deletes every deny in the bundle —
// which is precisely the change those assertions were written to catch.
func TestADenialThatHoldsByDefaultIsReportedAsHollow(t *testing.T) {
	deny := rule("r-secrets", policy.SubjectTypeAny, "", policy.EffectDeny,
		[]string{"secrets/**"}, policy.ActionRead)

	withRule := compile(t, spec([]policy.User{{ID: "carol"}}, nil, deny))
	without := compile(t, spec([]policy.User{{ID: "carol"}}, nil))

	expectation := policy.Expectation{
		Name: "nobody reads secrets", Repository: "payments",
		Path: "secrets/prod.pem", Actions: []policy.Action{policy.ActionRead},
		Expect: policy.EffectDeny, Users: []policy.UserID{"carol"},
	}

	held, err := policy.Check(withRule, []policy.Expectation{expectation})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if len(held.Failures) != 0 || len(held.Hollow) != 0 {
		t.Errorf("with the rule present: %+v", held)
	}

	// Rule gone. The assertion still holds, and that is the problem.
	empty, err := policy.Check(without, []policy.Expectation{expectation})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if len(empty.Failures) != 0 {
		t.Errorf("a default denial was reported as a failure: %+v", empty.Failures)
	}

	if len(empty.Hollow) != 1 {
		t.Fatalf("a denial that held by default was not reported as hollow: %+v", empty)
	}
}

// Naming the rule turns the same assertion into one that fails.
func TestNamingTheRuleCatchesItsDeletion(t *testing.T) {
	deny := rule("r-secrets", policy.SubjectTypeAny, "", policy.EffectDeny,
		[]string{"secrets/**"}, policy.ActionRead)

	withRule := compile(t, spec([]policy.User{{ID: "carol"}}, nil, deny))
	without := compile(t, spec([]policy.User{{ID: "carol"}}, nil))

	expectation := policy.Expectation{
		Name: "nobody reads secrets", Repository: "payments",
		Path: "secrets/prod.pem", Actions: []policy.Action{policy.ActionRead},
		Expect: policy.EffectDeny, Rule: "r-secrets",
		Users: []policy.UserID{"carol"},
	}

	held, err := policy.Check(withRule, []policy.Expectation{expectation})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if len(held.Failures) != 0 {
		t.Fatalf("the rule is there and it failed: %+v", held.Failures)
	}

	gone, err := policy.Check(without, []policy.Expectation{expectation})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if len(gone.Failures) != 1 {
		t.Fatalf("deleting the named rule did not fail: %+v", gone)
	}

	// The failure says what happened instead, or somebody reads the bundle.
	if !strings.Contains(gone.Failures[0].Because, "r-secrets") {
		t.Errorf("the failure does not name the rule: %s", gone.Failures[0].Because)
	}
}

// An expectation naming somebody who has been deleted is not a policy that
// broke. It is a test that has stopped testing, and reporting it as a pass is
// how a file of assertions rots into decoration.
func TestAnExpectationAboutSomebodyWhoIsGoneIsAnError(t *testing.T) {
	p := compile(t, spec([]policy.User{{ID: "alice"}}, nil))

	_, err := policy.Check(p, []policy.Expectation{{
		Name: "dave cannot read", Repository: "payments", Path: "x",
		Actions: []policy.Action{policy.ActionRead},
		Expect:  policy.EffectDeny, Users: []policy.UserID{"dave"},
	}})

	if err == nil {
		t.Fatal("an expectation about a deleted user passed")
	}

	if !strings.Contains(err.Error(), "dave") {
		t.Errorf("the error does not name them: %v", err)
	}
}

// The same for an empty group. "Every contractor is denied secrets" over a
// group with no members asserts nothing, and would go green forever the day the
// last contractor left.
func TestAnExpectationAboutAnEmptyGroupIsAnError(t *testing.T) {
	p := compile(t, spec(
		[]policy.User{{ID: "alice"}},
		[]policy.Group{{ID: "contractors"}},
	))

	_, err := policy.Check(p, []policy.Expectation{{
		Name: "contractors cannot read", Repository: "payments", Path: "x",
		Actions: []policy.Action{policy.ActionRead},
		Expect:  policy.EffectDeny, Groups: []policy.GroupID{"contractors"},
	}})

	if err == nil {
		t.Fatal("an expectation about an empty group passed")
	}

	if !strings.Contains(err.Error(), "asserts nothing") {
		t.Errorf("the error does not say why: %v", err)
	}
}

// An expectation about a group covers everybody in it, transitively — which is
// what makes it survive people joining and leaving.
func TestAGroupExpectationCoversEveryMember(t *testing.T) {
	p := compile(t, spec(
		[]policy.User{{ID: "alice"}, {ID: "bob"}, {ID: "carol"}},
		[]policy.Group{
			{ID: "juniors", Members: []policy.UserID{"carol"}},
			{ID: "backend", Members: []policy.UserID{"alice"}, Includes: []policy.GroupID{"juniors"}},
		},
		rule("r-1", policy.SubjectTypeGroup, "backend", policy.EffectAllow,
			[]string{"payments/**"}, policy.ActionRead),
	))

	result, err := policy.Check(p, []policy.Expectation{{
		Name: "the backend team reads payments", Repository: "payments",
		Path: "payments/ledger.go", Actions: []policy.Action{policy.ActionRead},
		Expect: policy.EffectAllow, Groups: []policy.GroupID{"backend"},
	}})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	// alice directly, carol through juniors. Two people, no failures — bob is
	// not in the group and is not asserted about.
	if result.Checked != 2 || len(result.Failures) != 0 {
		t.Errorf("result = %+v, want two checks and no failures", result)
	}
}

// An expectation that asserts nothing is refused rather than counted.
func TestAnExpectationHasToAssertSomething(t *testing.T) {
	p := compile(t, spec([]policy.User{{ID: "alice"}}, nil))

	broken := map[string]policy.Expectation{
		"no name":      {Repository: "payments", Path: "x", Actions: []policy.Action{policy.ActionRead}, Expect: policy.EffectDeny, Users: []policy.UserID{"alice"}},
		"no subject":   {Name: "n", Repository: "payments", Path: "x", Actions: []policy.Action{policy.ActionRead}, Expect: policy.EffectDeny},
		"no actions":   {Name: "n", Repository: "payments", Path: "x", Expect: policy.EffectDeny, Users: []policy.UserID{"alice"}},
		"no path":      {Name: "n", Repository: "payments", Actions: []policy.Action{policy.ActionRead}, Expect: policy.EffectDeny, Users: []policy.UserID{"alice"}},
		"no expect":    {Name: "n", Repository: "payments", Path: "x", Actions: []policy.Action{policy.ActionRead}, Users: []policy.UserID{"alice"}},
		"unknown repo": {Name: "n", Repository: "nowhere", Path: "x", Actions: []policy.Action{policy.ActionRead}, Expect: policy.EffectDeny, Users: []policy.UserID{"alice"}},
	}

	for what, expectation := range broken {
		if _, err := policy.Check(p, []policy.Expectation{expectation}); err == nil {
			t.Errorf("accepted an expectation with %s", what)
		}
	}
}
