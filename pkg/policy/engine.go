package policy

// Request is a single authorization question: may this subject perform this
// action on this path of this repository, at this ref?
type Request struct {
	Repo    RepoID
	Ref     string
	Subject Subject
	Path    string
	Action  Action
}

// Engine answers authorization questions.
type Engine interface {
	Evaluate(Request) Decision
}

// Evaluate returns the decision for a single request.
//
// Semantics, in order:
//
//  1. an unknown repository is denied;
//  2. a disabled user is denied;
//  3. every rule of the repository is considered — no early exit on the first
//     match, so the result does not depend on rule order in the bundle;
//  4. if any matching rule denies, the request is denied;
//  5. otherwise, if any matching rule allows, the request is allowed;
//  6. otherwise the request is denied (closed by default).
//
// Among several matching rules with the same effect, the one with the most
// specific pattern is reported. This affects the explanation only, never the
// outcome.
func (p *Policy) Evaluate(req Request) Decision {
	if _, ok := p.repos[req.Repo]; !ok {
		return denied(ReasonUnknownRepository, p.version)
	}

	if u, ok := p.users[req.Subject.UserID]; ok && u.Disabled {
		return denied(ReasonUserDisabled, p.version)
	}

	var (
		bestDeny     *Rule
		bestDenyPat  Pattern
		bestAllow    *Rule
		bestAllowPat Pattern
	)

	for _, r := range p.rules[req.Repo] {
		if !r.HasAction(req.Action) {
			continue
		}
		if !r.MatchesSubject(req.Subject) {
			continue
		}
		if !r.MatchesRef(req.Ref) {
			continue
		}

		pat, ok := r.MatchesPath(req.Path)
		if !ok {
			continue
		}

		if r.Effect == EffectDeny {
			if bestDeny == nil || pat.Specificity() > bestDenyPat.Specificity() {
				bestDeny, bestDenyPat = r, pat
			}
			continue
		}

		if bestAllow == nil || pat.Specificity() > bestAllowPat.Specificity() {
			bestAllow, bestAllowPat = r, pat
		}
	}

	switch {
	case bestDeny != nil:
		return Decision{
			Allowed:       false,
			Effect:        EffectDeny,
			Reason:        ReasonDeniedByRule,
			RuleID:        bestDeny.ID,
			Pattern:       bestDenyPat.String(),
			Description:   bestDeny.Description,
			PolicyVersion: p.version,
		}

	case bestAllow != nil:
		return Decision{
			Allowed:       true,
			Effect:        EffectAllow,
			Reason:        ReasonAllowedByRule,
			RuleID:        bestAllow.ID,
			Pattern:       bestAllowPat.String(),
			Description:   bestAllow.Description,
			PolicyVersion: p.version,
		}

	default:
		return denied(ReasonNoMatchingRule, p.version)
	}
}

// AllowsAll reports whether every (path, action) pair is allowed, and returns
// the first decision that refused. It is a convenience for callers that only
// need a yes/no answer.
func AllowsAll(e Engine, base Request, paths []string, actions ...Action) (bool, Decision) {
	for _, path := range paths {
		for _, action := range actions {
			req := base
			req.Path = path
			req.Action = action

			if d := e.Evaluate(req); !d.Allowed {
				return false, d
			}
		}
	}

	return true, Decision{Allowed: true}
}

// CachingEngine memoizes decisions for the duration of one operation.
//
// A patch touching a few thousand files asks the same question about the same
// directory over and over, and each question costs a glob match against every
// rule. The cache turns that into one evaluation per distinct (path, action).
// It is not safe for concurrent use and must not outlive a single request: it
// holds no invalidation, by design, so that a policy reload can never be
// observed halfway through an authorization pass.
type CachingEngine struct {
	engine Engine
	base   Request
	cache  map[cacheKey]Decision
}

type cacheKey struct {
	path   string
	action Action
}

// NewCachingEngine wraps engine for one operation. base supplies the fields
// that stay constant across the operation: repository, ref and subject.
func NewCachingEngine(engine Engine, base Request) *CachingEngine {
	return &CachingEngine{
		engine: engine,
		base:   base,
		cache:  make(map[cacheKey]Decision),
	}
}

// Evaluate returns the decision for one path and action, using the base
// request for every other field.
func (c *CachingEngine) Evaluate(path string, action Action) Decision {
	key := cacheKey{path: path, action: action}

	if d, ok := c.cache[key]; ok {
		return d
	}

	req := c.base
	req.Path = path
	req.Action = action

	d := c.engine.Evaluate(req)
	c.cache[key] = d

	return d
}

// Base returns the constant part of the requests this engine answers.
func (c *CachingEngine) Base() Request { return c.base }
