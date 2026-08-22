package policy

import (
	"errors"
	"fmt"
	"sort"
)

// Spec is the input of Compile: a policy bundle as authored, before validation.
// The config subpackage produces one from YAML; tests build one directly.
type Spec struct {
	Tenant TenantID

	// Version identifies the bundle, typically a content hash of its source
	// files. It is copied into every Decision.
	Version string

	Users        []User
	Groups       []Group
	Repositories []Repository

	// Rules are indexed by repository.
	Rules map[RepoID][]Rule
}

// Policy is a compiled, validated bundle. It is immutable and safe for
// concurrent use; reloading means compiling a new one and swapping the pointer.
type Policy struct {
	tenant  TenantID
	version string

	users  map[UserID]User
	groups map[GroupID]Group
	repos  map[RepoID]Repository
	rules  map[RepoID][]*Rule

	// memberships holds the transitive group closure of each user, computed
	// once at compile time rather than on every path of every patch.
	memberships map[UserID][]GroupID
}

// ErrUnknownUser is returned when resolving a subject that is not in the
// bundle.
var ErrUnknownUser = errors.New("policy: unknown user")

// Compile validates a Spec and returns a Policy. Every error a bundle can
// contain is reported here, at load time, so that Evaluate can be total.
func Compile(spec Spec) (*Policy, error) {
	if spec.Tenant == "" {
		spec.Tenant = DefaultTenant
	}

	p := &Policy{
		tenant:  spec.Tenant,
		version: spec.Version,
		users:   make(map[UserID]User, len(spec.Users)),
		groups:  make(map[GroupID]Group, len(spec.Groups)),
		repos:   make(map[RepoID]Repository, len(spec.Repositories)),
		rules:   make(map[RepoID][]*Rule, len(spec.Rules)),
	}

	for _, u := range spec.Users {
		if u.ID == "" {
			return nil, fmt.Errorf("policy: user with empty id")
		}
		if _, dup := p.users[u.ID]; dup {
			return nil, fmt.Errorf("policy: duplicate user %q", u.ID)
		}
		p.users[u.ID] = u
	}

	for _, g := range spec.Groups {
		if g.ID == "" {
			return nil, fmt.Errorf("policy: group with empty id")
		}
		if _, dup := p.groups[g.ID]; dup {
			return nil, fmt.Errorf("policy: duplicate group %q", g.ID)
		}
		p.groups[g.ID] = g
	}

	for _, r := range spec.Repositories {
		if r.ID == "" {
			return nil, fmt.Errorf("policy: repository with empty id")
		}
		if _, dup := p.repos[r.ID]; dup {
			return nil, fmt.Errorf("policy: duplicate repository %q", r.ID)
		}
		p.repos[r.ID] = r
	}

	if err := p.checkGroupReferences(); err != nil {
		return nil, err
	}
	if err := p.resolveMemberships(); err != nil {
		return nil, err
	}
	if err := p.compileRules(spec.Rules); err != nil {
		return nil, err
	}

	return p, nil
}

// Version returns the identifier of the compiled bundle.
func (p *Policy) Version() string { return p.version }

// Tenant returns the tenant the bundle belongs to.
func (p *Policy) Tenant() TenantID { return p.tenant }

// Repository looks up a repository by id.
func (p *Policy) Repository(id RepoID) (Repository, bool) {
	r, ok := p.repos[id]
	return r, ok
}

// Repositories returns every repository under nit control, sorted by id.
func (p *Policy) Repositories() []Repository {
	out := make([]Repository, 0, len(p.repos))
	for _, r := range p.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// User looks up a user by id.
func (p *Policy) User(id UserID) (User, bool) {
	u, ok := p.users[id]
	return u, ok
}

// Rules returns the rules attached to a repository, in bundle order. The order
// carries no semantics; it is preserved only so reports read like the file.
func (p *Policy) Rules(repo RepoID) []*Rule {
	return p.rules[repo]
}

// Subject resolves a user into an authorization subject, expanding groups.
func (p *Policy) Subject(id UserID) (Subject, error) {
	if _, ok := p.users[id]; !ok {
		return Subject{}, fmt.Errorf("%w: %q", ErrUnknownUser, id)
	}

	return Subject{UserID: id, Groups: p.memberships[id]}, nil
}

func (p *Policy) checkGroupReferences() error {
	for _, g := range p.groups {
		for _, m := range g.Members {
			if _, ok := p.users[m]; !ok {
				return fmt.Errorf("policy: group %q references unknown user %q", g.ID, m)
			}
		}
		for _, inc := range g.Includes {
			if _, ok := p.groups[inc]; !ok {
				return fmt.Errorf("policy: group %q includes unknown group %q", g.ID, inc)
			}
		}
	}
	return nil
}

// resolveMemberships computes, for each user, the full set of groups they
// belong to.
//
// "g includes h" means every member of h is also a member of g. Membership
// therefore propagates from the included group up to the including one.
func (p *Policy) resolveMemberships() error {
	if err := p.detectGroupCycles(); err != nil {
		return err
	}

	// includedBy[h] lists the groups that include h.
	includedBy := make(map[GroupID][]GroupID, len(p.groups))
	for _, g := range p.groups {
		for _, inc := range g.Includes {
			includedBy[inc] = append(includedBy[inc], g.ID)
		}
	}

	direct := make(map[UserID][]GroupID, len(p.users))
	for _, g := range p.groups {
		for _, m := range g.Members {
			direct[m] = append(direct[m], g.ID)
		}
	}

	p.memberships = make(map[UserID][]GroupID, len(p.users))

	for uid := range p.users {
		seen := make(map[GroupID]struct{})
		queue := append([]GroupID(nil), direct[uid]...)

		for len(queue) > 0 {
			g := queue[0]
			queue = queue[1:]

			if _, ok := seen[g]; ok {
				continue
			}
			seen[g] = struct{}{}

			queue = append(queue, includedBy[g]...)
		}

		groups := make([]GroupID, 0, len(seen))
		for g := range seen {
			groups = append(groups, g)
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })

		p.memberships[uid] = groups
	}

	return nil
}

// detectGroupCycles reports inclusion cycles. Without this check, membership
// resolution would still terminate, but the bundle would express something its
// author almost certainly did not mean.
func (p *Policy) detectGroupCycles() error {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current path
		black = 2 // fully explored
	)

	state := make(map[GroupID]int, len(p.groups))

	var visit func(GroupID, []GroupID) error
	visit = func(g GroupID, path []GroupID) error {
		switch state[g] {
		case grey:
			return fmt.Errorf("policy: group inclusion cycle: %v -> %v", path, g)
		case black:
			return nil
		}

		state[g] = grey
		path = append(path, g)

		for _, inc := range p.groups[g].Includes {
			if err := visit(inc, path); err != nil {
				return err
			}
		}

		state[g] = black
		return nil
	}

	ids := make([]GroupID, 0, len(p.groups))
	for id := range p.groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if err := visit(id, nil); err != nil {
			return err
		}
	}

	return nil
}

func (p *Policy) compileRules(rules map[RepoID][]Rule) error {
	for repo, list := range rules {
		if _, ok := p.repos[repo]; !ok {
			return fmt.Errorf("policy: rules declared for unknown repository %q", repo)
		}

		compiled := make([]*Rule, 0, len(list))

		for i := range list {
			r := list[i]

			if r.ID == "" {
				r.ID = fmt.Sprintf("%s#%d", repo, i)
			}
			if err := validateRule(&r); err != nil {
				return fmt.Errorf("policy: repository %q, rule %s: %w", repo, r.ID, err)
			}
			if err := p.checkRuleSubject(&r); err != nil {
				return fmt.Errorf("policy: repository %q, rule %s: %w", repo, r.ID, err)
			}

			compiled = append(compiled, &r)
		}

		p.rules[repo] = compiled
	}

	return nil
}

func validateRule(r *Rule) error {
	if !r.Effect.Valid() {
		return fmt.Errorf("invalid effect %q", r.Effect)
	}
	if !r.Subject.Type.Valid() {
		return fmt.Errorf("invalid subject type %q", r.Subject.Type)
	}
	if r.Subject.Type != SubjectTypeAny && r.Subject.ID == "" {
		return fmt.Errorf("subject of type %q needs an id", r.Subject.Type)
	}

	for _, ex := range r.Except {
		if !ex.Type.Valid() {
			return fmt.Errorf("invalid except subject type %q", ex.Type)
		}
		if ex.Type == SubjectTypeAny {
			return errors.New("an except entry of type \"any\" would disable the rule entirely")
		}
		if ex.ID == "" {
			return fmt.Errorf("except subject of type %q needs an id", ex.Type)
		}
	}
	if len(r.Paths) == 0 {
		return errors.New("no path pattern")
	}
	if len(r.Actions) == 0 {
		return errors.New("no action")
	}

	seen := make(map[Action]struct{}, len(r.Actions))
	for _, a := range r.Actions {
		if !a.Valid() {
			return fmt.Errorf("invalid action %q", a)
		}
		if _, dup := seen[a]; dup {
			return fmt.Errorf("duplicate action %q", a)
		}
		seen[a] = struct{}{}
	}

	// A subject that may write a file it may not read would be writing blind:
	// it can overwrite content it is not allowed to see, and it cannot produce
	// a meaningful diff against a file absent from its workspace. Rejecting the
	// bundle is the only place this can be caught before it causes data loss.
	if _, hasRead := seen[ActionRead]; !hasRead && r.Effect == EffectAllow {
		for _, a := range []Action{ActionWrite, ActionCreate, ActionDelete} {
			if _, ok := seen[a]; ok {
				return fmt.Errorf("grants %q without %q; write access implies read access", a, ActionRead)
			}
		}
	}

	return nil
}

func (p *Policy) checkRuleSubject(r *Rule) error {
	subjects := append([]RuleSubject{r.Subject}, r.Except...)

	for _, s := range subjects {
		switch s.Type {
		case SubjectTypeUser:
			if _, ok := p.users[UserID(s.ID)]; !ok {
				return fmt.Errorf("unknown user %q", s.ID)
			}
		case SubjectTypeGroup:
			if _, ok := p.groups[GroupID(s.ID)]; !ok {
				return fmt.Errorf("unknown group %q", s.ID)
			}
		}
	}

	return nil
}
