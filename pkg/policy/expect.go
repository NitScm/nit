package policy

import (
	"fmt"
	"sort"
	"strings"
)

// Assertions about a bundle, checked against it.
//
// # Why a bundle needs tests at all
//
// `validate` says a bundle is well-formed. `diff` says what a change does to
// people. Neither protects a rule from being deleted.
//
// The rule that matters most is usually a deny, and a deny that disappears
// during a refactor takes nothing with it: the bundle still compiles, every
// other rule still works, and nothing looks wrong until somebody reads
// something they should not have. `diff` would have reported it — to whoever
// happened to be reviewing that pull request, at the end of a long list.
//
// An expectation is the same statement written down once and checked forever.
//
// # Why they live outside the bundle
//
// The bundle's version is a SHA-256 over every YAML file in its directory, and
// that version is stamped on every decision and every audit record. A test file
// inside the bundle would make editing a test change the version that
// identifies the rules — so a decision from last month would appear to have
// come from rules nobody can reconstruct. The loader refuses that arrangement
// rather than producing it.

// Expectation is one thing a bundle is supposed to do.
type Expectation struct {
	// Name is what a failure is reported as, so make it the sentence you would
	// say out loud: "contractors cannot read production secrets".
	Name string `yaml:"name" json:"name"`

	Repository RepoID `yaml:"repository" json:"repository"`
	Path       string `yaml:"path" json:"path"`

	// Ref is optional and empty means the default branch is not the point.
	Ref string `yaml:"ref" json:"ref"`

	// Users and Groups are who this is about. At least one is required.
	//
	// A group asserts something about the rule — "the backend team can write
	// payments/**" — and stays true as people join and leave, which is what you
	// want: membership changes are `diff`'s job, and a test that broke on every
	// hire would be deleted by the second month.
	Users  []UserID  `yaml:"users" json:"users"`
	Groups []GroupID `yaml:"groups" json:"groups"`

	// Actions is what they are supposed to be able, or unable, to do.
	Actions []Action `yaml:"actions" json:"actions"`

	// Expect is "allow" or "deny".
	Expect Effect `yaml:"expect" json:"expect"`

	// Rule is the rule that has to produce the outcome.
	//
	// Optional, and it is the difference between a test and a placebo when the
	// expectation is a denial. Everything is denied by default, so `expect:
	// deny` holds whether the deny rule is there or not — a file of "nobody
	// outside security reads secrets" assertions stays green after somebody
	// deletes every deny in the bundle.
	//
	// Naming the rule makes the assertion about the rule. Without it, Check
	// still reports how many denials held by default rather than by anything,
	// because a test that proves nothing should at least say so.
	Rule string `yaml:"rule" json:"rule"`
}

// Result is what a run of expectations produced.
type Result struct {
	Checked  int       `json:"checked"`
	Failures []Failure `json:"failures"`

	// Hollow are denials that held because everything is denied by default,
	// with no rule involved.
	//
	// Not failures — asserting that the default holds is legitimate. But a file
	// where most assertions are hollow is a file that would stay green through
	// the deletion of every deny rule in the bundle, and its author should find
	// that out from the tool rather than from an incident.
	Hollow []Failure `json:"held_by_default"`
}

// Failure is one expectation that did not hold.
type Failure struct {
	Expectation string `json:"expectation"`
	User        UserID `json:"user"`
	Repository  RepoID `json:"repository"`
	Path        string `json:"path"`
	Action      Action `json:"action"`

	Wanted Effect `json:"wanted"`
	Got    Effect `json:"got"`

	// Because is the decision's own explanation — the rule that produced it, or
	// the absence of one. A failure that only says "expected allow" sends
	// somebody to read the whole bundle.
	Because string `json:"because"`
}

func (f Failure) String() string {
	return fmt.Sprintf("%s: %s %s %s:%s — wanted %s, got %s (%s)",
		f.Expectation, f.User, f.Action, f.Repository, f.Path, f.Wanted, f.Got, f.Because)
}

// ErrExpectation is a malformed or unsatisfiable expectation.
//
// It is an error and never a failure. An expectation naming a user who has been
// deleted is not a policy that broke — it is a test that has stopped testing,
// and reporting it as a pass is how a file of assertions rots into decoration.
type ErrExpectation struct {
	Expectation string
	Problem     string
}

func (e ErrExpectation) Error() string {
	if e.Expectation == "" {
		return "policy: " + e.Problem
	}

	return fmt.Sprintf("policy: %q: %s", e.Expectation, e.Problem)
}

// Check runs every expectation and reports what did not hold.
//
// The error return is for expectations that could not be run at all. It is
// separate from the failures on purpose: one means the policy is wrong, the
// other means the test is, and conflating them lets a rotten test file look
// like a healthy one.
func Check(p *Policy, expectations []Expectation) (Result, error) {
	var result Result

	for _, e := range expectations {
		if err := e.validate(); err != nil {
			return Result{}, err
		}

		subjects, err := e.subjects(p)
		if err != nil {
			return Result{}, err
		}

		if _, known := p.Repository(e.Repository); !known {
			return Result{}, ErrExpectation{e.Name, fmt.Sprintf("no repository %q in this bundle", e.Repository)}
		}

		for _, id := range subjects {
			subject, err := p.Subject(id)
			if err != nil {
				return Result{}, ErrExpectation{e.Name, fmt.Sprintf("no user %q in this bundle", id)}
			}

			for _, action := range e.Actions {
				decision := p.Evaluate(Request{
					Repo: e.Repository, Ref: e.Ref, Subject: subject,
					Path: e.Path, Action: action,
				})

				result.Checked++

				got := EffectDeny
				if decision.Allowed {
					got = EffectAllow
				}

				failed := Failure{
					Expectation: e.Name, User: id, Repository: e.Repository,
					Path: e.Path, Action: action,
					Wanted: e.Expect, Got: got, Because: decision.String(),
				}

				if got != e.Expect {
					result.Failures = append(result.Failures, failed)

					continue
				}

				// The outcome is right. Was it right for the reason the author
				// meant?
				if e.Rule != "" && decision.RuleID != e.Rule {
					failed.Because = fmt.Sprintf("wanted rule %s, got %s", e.Rule, decision.String())
					result.Failures = append(result.Failures, failed)

					continue
				}

				if e.Expect == EffectDeny && e.Rule == "" && decision.Reason == ReasonNoMatchingRule {
					result.Hollow = append(result.Hollow, failed)
				}
			}
		}
	}

	return result, nil
}

func (e Expectation) validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return ErrExpectation{"", "an expectation needs a name; it is what a failure is reported as"}
	}

	if e.Repository == "" {
		return ErrExpectation{e.Name, "no repository"}
	}

	if e.Path == "" {
		return ErrExpectation{e.Name, "no path"}
	}

	if len(e.Actions) == 0 {
		return ErrExpectation{e.Name, "no actions"}
	}

	for _, a := range e.Actions {
		if !a.Valid() {
			return ErrExpectation{e.Name, fmt.Sprintf("%q is not an action", a)}
		}
	}

	if e.Expect != EffectAllow && e.Expect != EffectDeny {
		return ErrExpectation{e.Name, fmt.Sprintf("expect is %q, and has to be allow or deny", e.Expect)}
	}

	if len(e.Users) == 0 && len(e.Groups) == 0 {
		return ErrExpectation{e.Name, "no users and no groups: this asserts nothing"}
	}

	return nil
}

// subjects resolves who an expectation is about.
//
// A group that exists and is empty is an error, not an empty pass. "Every
// contractor is denied secrets" over a group with no members is a sentence that
// asserts nothing, and it would go green forever the day the last contractor
// left.
func (e Expectation) subjects(p *Policy) ([]UserID, error) {
	seen := map[UserID]bool{}

	for _, id := range e.Users {
		seen[id] = true
	}

	for _, group := range e.Groups {
		members := 0

		for _, user := range p.Users() {
			subject, err := p.Subject(user.ID)
			if err != nil {
				continue
			}

			if subject.InGroup(group) {
				seen[user.ID] = true
				members++
			}
		}

		if members == 0 {
			return nil, ErrExpectation{e.Name,
				fmt.Sprintf("group %q has no members, so this asserts nothing", group)}
		}
	}

	out := make([]UserID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	return out, nil
}
