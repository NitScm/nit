package policy

import (
	"fmt"
	"sort"
	"strings"
)

// Comparing two bundles, in terms of what changes for people.
//
// # Why this exists
//
// A diff of the YAML tells a reviewer that four lines changed. It does not tell
// them that one of those lines put twelve people into a group that reads
// `config/**`, because the group is defined in another file and the rule that
// grants it was not touched. Past the size where one person knows every rule,
// reviewing a permission change by reading its diff is a signature rather than
// a review.
//
// So this expands the rules through group membership and reports what each
// person gained and lost.
//
// # What it does not do, and cannot
//
// It does not say "alice can now read src/main.go". Paths are infinite: there
// is no set of them to enumerate, and any sample would be a sample. What it
// compares is the rules *as they apply to each person* — which is exact, finite
// and derived rather than sampled.
//
// For a specific path, `nitctl policy explain` answers exactly and this does
// not try to replace it. The two are for different moments: explain is for a
// question somebody already has, and this is for the change nobody thought to
// ask about.

// Grant is one rule as it reaches one person.
type Grant struct {
	Repository RepoID `json:"repository"`
	Action     Action `json:"action"`
	Effect     Effect `json:"effect"`

	// Path and Ref are the pattern texts, not paths. See the package comment:
	// a pattern is what a rule actually says, and a path would be a guess at
	// what it covers.
	Path string `json:"path"`
	Ref  string `json:"ref"`

	RuleID string `json:"rule_id"`

	// Via is the group that brought this to them, empty when they are named.
	//
	// The most useful column in the report. "alice gained read on config/**"
	// invites the question "how?", and answering it is usually where somebody
	// notices that a group has grown a member nobody meant to add.
	Via string `json:"via"`
}

func (g Grant) String() string {
	where := g.Path
	if g.Ref != "" {
		where += " on " + g.Ref
	}

	via := ""
	if g.Via != "" {
		via = " via " + g.Via
	}

	return fmt.Sprintf("%s %s %s:%s (%s%s)", g.Effect, g.Action, g.Repository, where, g.RuleID, via)
}

func (g Grant) key() string {
	return strings.Join([]string{
		string(g.Repository), string(g.Action), string(g.Effect), g.Path, g.Ref, g.RuleID, g.Via,
	}, "\x00")
}

// Change is what moved for one person.
type Change struct {
	User UserID `json:"user"`

	// Allowed and Denied are what they gained; NoLongerAllowed and
	// NoLongerDenied are what they lost.
	//
	// Four lists rather than "added" and "removed", because deny wins (see
	// Evaluate) and the two directions do not mean the same thing. A gained
	// deny is *less* access. A lost deny is more — and it is the change most
	// easily missed in a text diff, because removing lines rarely looks
	// alarming.
	Allowed         []Grant `json:"allowed"`
	Denied          []Grant `json:"denied"`
	NoLongerAllowed []Grant `json:"no_longer_allowed"`
	NoLongerDenied  []Grant `json:"no_longer_denied"`
}

// Widens reports whether this person can plausibly reach more than before.
//
// Plausibly, and not certainly: another deny may still cover the same path. It
// is the flag worth raising in a review — a reviewer can check the ones it
// names, and does not have to check the rest.
func (c Change) Widens() bool { return len(c.Allowed) > 0 || len(c.NoLongerDenied) > 0 }

// Narrows reports whether they can reach less.
func (c Change) Narrows() bool { return len(c.Denied) > 0 || len(c.NoLongerAllowed) > 0 }

// Diff is the whole comparison.
type Diff struct {
	UsersAdded   []UserID `json:"users_added"`
	UsersRemoved []UserID `json:"users_removed"`

	ReposAdded   []RepoID `json:"repos_added"`
	ReposRemoved []RepoID `json:"repos_removed"`

	// Changes covers people present in both bundles whose grants moved, sorted
	// by user id.
	Changes []Change `json:"changes"`
}

// Empty reports whether nothing at all changed.
func (d Diff) Empty() bool {
	return len(d.UsersAdded) == 0 && len(d.UsersRemoved) == 0 &&
		len(d.ReposAdded) == 0 && len(d.ReposRemoved) == 0 && len(d.Changes) == 0
}

// Widening is everybody who can plausibly reach more than before.
func (d Diff) Widening() []Change {
	var out []Change

	for _, c := range d.Changes {
		if c.Widens() {
			out = append(out, c)
		}
	}

	return out
}

// Compare reports what moved between two bundles.
//
// Both may be nil-free compiled policies; nothing here does IO, and nothing
// here can fail — a bundle that compiled is a bundle every question below has
// an answer for.
func Compare(before, after *Policy) Diff {
	var d Diff

	d.UsersAdded, d.UsersRemoved = comparedUsers(before, after)
	d.ReposAdded, d.ReposRemoved = comparedRepos(before, after)

	// Only people in both bundles have a *change*; the rest are an addition or
	// a removal, which is reported above and would be noise repeated here.
	for _, user := range after.Users() {
		if _, both := before.User(user.ID); !both {
			continue
		}

		was := grantsFor(before, user.ID)
		now := grantsFor(after, user.ID)

		change := Change{User: user.ID}

		for key, grant := range now {
			if _, had := was[key]; had {
				continue
			}

			if grant.Effect == EffectDeny {
				change.Denied = append(change.Denied, grant)
			} else {
				change.Allowed = append(change.Allowed, grant)
			}
		}

		for key, grant := range was {
			if _, kept := now[key]; kept {
				continue
			}

			if grant.Effect == EffectDeny {
				change.NoLongerDenied = append(change.NoLongerDenied, grant)
			} else {
				change.NoLongerAllowed = append(change.NoLongerAllowed, grant)
			}
		}

		if !change.Widens() && !change.Narrows() {
			continue
		}

		sortGrants(change.Allowed)
		sortGrants(change.Denied)
		sortGrants(change.NoLongerAllowed)
		sortGrants(change.NoLongerDenied)

		d.Changes = append(d.Changes, change)
	}

	sort.Slice(d.Changes, func(i, j int) bool { return d.Changes[i].User < d.Changes[j].User })

	return d
}

// grantsFor expands every rule that reaches one person.
//
// Through their transitive group membership, which is the whole point: the
// change that needs catching is usually a membership, not a rule.
func grantsFor(p *Policy, id UserID) map[string]Grant {
	out := map[string]Grant{}

	subject, err := p.Subject(id)
	if err != nil {
		return out
	}

	for _, repo := range p.Repositories() {
		for _, rule := range p.Rules(repo.ID) {
			if !rule.MatchesSubject(subject) {
				continue
			}

			via := attribution(rule, subject)

			refs := []string{""}
			if len(rule.Refs) > 0 {
				refs = refs[:0]
				for _, ref := range rule.Refs {
					refs = append(refs, ref.String())
				}
			}

			for _, action := range rule.Actions {
				for _, path := range rule.Paths {
					for _, ref := range refs {
						grant := Grant{
							Repository: repo.ID,
							Action:     action,
							Effect:     rule.Effect,
							Path:       path.String(),
							Ref:        ref,
							RuleID:     rule.ID,
							Via:        via,
						}

						out[grant.key()] = grant
					}
				}
			}
		}
	}

	return out
}

// attribution says how a rule reached somebody.
//
// A rule naming them directly is reported as such; otherwise the group it
// selects. "everyone" rules say so, because "via everyone" reads differently
// from a grant somebody was deliberately given.
func attribution(rule *Rule, subject Subject) string {
	switch rule.Subject.Type {
	case SubjectTypeUser:
		return ""
	case SubjectTypeGroup:
		return string(rule.Subject.ID)
	default:
		return string(rule.Subject.Type)
	}
}

func comparedUsers(before, after *Policy) (added, removed []UserID) {
	for _, u := range after.Users() {
		if _, had := before.User(u.ID); !had {
			added = append(added, u.ID)
		}
	}

	for _, u := range before.Users() {
		if _, kept := after.User(u.ID); !kept {
			removed = append(removed, u.ID)
		}
	}

	return added, removed
}

func comparedRepos(before, after *Policy) (added, removed []RepoID) {
	for _, r := range after.Repositories() {
		if _, had := before.Repository(r.ID); !had {
			added = append(added, r.ID)
		}
	}

	for _, r := range before.Repositories() {
		if _, kept := after.Repository(r.ID); !kept {
			removed = append(removed, r.ID)
		}
	}

	return added, removed
}

func sortGrants(grants []Grant) {
	sort.Slice(grants, func(i, j int) bool { return grants[i].key() < grants[j].key() })
}
