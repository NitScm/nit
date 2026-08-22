package policy

import "fmt"

// Action is an operation a subject may be authorized to perform on a path.
//
// read and write are not enough. Deleting a file is not "writing" it, and
// creating one is not modifying it: a reviewer who grants write access to a
// config directory rarely means "and you may delete everything in it". The
// admin action guards paths that can subvert the whole model (see the enforce
// package): CI definitions, .gitattributes, .gitmodules, symlink creation.
type Action string

const (
	ActionRead   Action = "read"
	ActionWrite  Action = "write"
	ActionCreate Action = "create"
	ActionDelete Action = "delete"
	ActionAdmin  Action = "admin"
)

// AllActions lists every action, in reporting order.
var AllActions = []Action{ActionRead, ActionWrite, ActionCreate, ActionDelete, ActionAdmin}

// Valid reports whether a is a known action.
func (a Action) Valid() bool {
	switch a {
	case ActionRead, ActionWrite, ActionCreate, ActionDelete, ActionAdmin:
		return true
	default:
		return false
	}
}

// Effect is what a matching rule does.
type Effect string

const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Valid reports whether e is a known effect.
func (e Effect) Valid() bool {
	return e == EffectAllow || e == EffectDeny
}

// SubjectType selects who a rule applies to.
type SubjectType string

const (
	SubjectTypeUser  SubjectType = "user"
	SubjectTypeGroup SubjectType = "group"

	// SubjectTypeAny matches every authenticated subject. It exists so a bundle
	// can state repository-wide baselines ("nobody touches .github/") without
	// enumerating groups.
	SubjectTypeAny SubjectType = "any"
)

// Valid reports whether t is a known subject type.
func (t SubjectType) Valid() bool {
	switch t {
	case SubjectTypeUser, SubjectTypeGroup, SubjectTypeAny:
		return true
	default:
		return false
	}
}

// RuleSubject is the principal selector of a rule.
type RuleSubject struct {
	Type SubjectType
	ID   string
}

// Matches reports whether the rule subject selects s.
func (rs RuleSubject) Matches(s Subject) bool {
	switch rs.Type {
	case SubjectTypeAny:
		return true
	case SubjectTypeUser:
		return UserID(rs.ID) == s.UserID
	case SubjectTypeGroup:
		return s.InGroup(GroupID(rs.ID))
	default:
		return false
	}
}

func (rs RuleSubject) String() string {
	if rs.Type == SubjectTypeAny {
		return "any"
	}
	return fmt.Sprintf("%s:%s", rs.Type, rs.ID)
}

// Rule grants or denies a set of actions on a set of paths, for one subject,
// optionally restricted to some refs.
type Rule struct {
	// ID identifies the rule in decisions and audit records. It is derived from
	// the bundle when the author does not supply one, and it is what makes a
	// decision explainable.
	ID string

	Subject RuleSubject

	// Except carves subjects out of Subject.
	//
	// Deny always wins over allow, which makes exemptions impossible to express
	// with an allow rule: "nobody reads secrets/, except the platform team"
	// cannot be written as a universal deny plus a team-wide allow, because the
	// deny would swallow the allow. Without Except the only workaround is to
	// enumerate every non-exempt group in the deny rule, which silently grants
	// access to every group created afterwards — a policy language that fails
	// open as the organization grows.
	Except []RuleSubject

	// Paths are the path patterns the rule covers. See Pattern for the syntax.
	Paths []Pattern

	// Refs restricts the rule to matching refs (for example
	// "refs/heads/feature/**"). Empty means every ref.
	//
	// This is what expresses the single most requested policy in practice:
	// "nobody pushes straight to main".
	Refs []Pattern

	Actions []Action

	Effect Effect

	// Description is free text surfaced to users when the rule denies them.
	// A denial the developer cannot understand becomes a support ticket.
	Description string
}

// MatchesSubject reports whether the rule applies to s: its subject selects s
// and no exemption excludes them.
func (r *Rule) MatchesSubject(s Subject) bool {
	if !r.Subject.Matches(s) {
		return false
	}

	for _, ex := range r.Except {
		if ex.Matches(s) {
			return false
		}
	}

	return true
}

// HasAction reports whether the rule covers the given action.
func (r *Rule) HasAction(a Action) bool {
	for _, ra := range r.Actions {
		if ra == a {
			return true
		}
	}
	return false
}

// MatchesRef reports whether the rule applies to the given ref. A rule with no
// ref restriction applies everywhere, including when the caller supplies no
// ref (path-only queries such as read filtering of a whole tree).
func (r *Rule) MatchesRef(ref string) bool {
	if len(r.Refs) == 0 {
		return true
	}
	if ref == "" {
		return false
	}

	for _, p := range r.Refs {
		if p.Match(ref) {
			return true
		}
	}
	return false
}

// MatchesPath reports whether any of the rule's path patterns covers path, and
// returns the pattern that matched.
func (r *Rule) MatchesPath(path string) (Pattern, bool) {
	for _, p := range r.Paths {
		if p.Match(path) {
			return p, true
		}
	}
	return Pattern{}, false
}
