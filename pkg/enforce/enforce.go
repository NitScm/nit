package enforce

import (
	"errors"
	"fmt"
	"strings"

	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/policy"
)

// Mode selects what happens to a push carrying unauthorized sections.
type Mode string

const (
	// ModeReject refuses the whole push. This is the default, and the only
	// mode that preserves the author's intent: either the change lands as
	// written, or it does not land at all.
	ModeReject Mode = "reject"

	// ModeStrip drops the unauthorized sections and lets the rest through. It
	// exists because some workflows genuinely want it, but it must be an
	// explicit, per-invocation choice by the client: the resulting upstream
	// commit differs from what the author committed locally, and the two states
	// have to be reconciled afterwards.
	ModeStrip Mode = "strip"
)

// Options is the common configuration of an enforcement pass.
type Options struct {
	// Engine answers authorization questions. Required.
	Engine policy.Engine

	// Repo and Ref scope the request. Ref is the fully qualified upstream ref
	// ("refs/heads/main"); rules restricted to refs never match when it is
	// empty.
	Repo policy.RepoID
	Ref  string

	// Subject is the resolved principal, groups already expanded.
	Subject policy.Subject

	// Guards are the structural protections. The zero value applies none, which
	// is almost never what a deployment wants: use DefaultGuards.
	Guards Guards
}

func (o Options) validate() error {
	if o.Engine == nil {
		return errors.New("enforce: no policy engine")
	}
	if o.Repo == "" {
		return errors.New("enforce: no repository")
	}
	if o.Subject.UserID == "" {
		return errors.New("enforce: no subject")
	}
	return nil
}

func (o Options) request() policy.Request {
	return policy.Request{
		Repo:    o.Repo,
		Ref:     o.Ref,
		Subject: o.Subject,
	}
}

// Result is the outcome of an enforcement pass over a whole patch.
type Result struct {
	Mode Mode

	// Verdicts holds one entry per section of the input patch, in order.
	Verdicts []Verdict

	// Kept and Dropped partition the input sections.
	Kept    []*patch.Change
	Dropped []*patch.Change

	// Patch is the rewritten patch, byte-identical to the input for every
	// section it retains. It is nil when nothing survives, and nil when the
	// pass rejected the push.
	Patch []byte

	// Rejected is true when the pass refused the operation as a whole rather
	// than filtering it.
	Rejected bool

	PolicyVersion string
}

// OK reports whether the operation may proceed with Result.Patch.
func (r *Result) OK() bool { return !r.Rejected && len(r.Patch) > 0 }

// DroppedPaths lists the paths of the sections that did not survive.
func (r *Result) DroppedPaths() []string {
	out := make([]string, 0, len(r.Dropped))
	for _, c := range r.Dropped {
		out = append(out, c.DisplayPath())
	}
	return out
}

// Denials returns every failed check, across all sections, for reporting to the
// user and for the audit log.
func (r *Result) Denials() []Check {
	var out []Check
	for _, v := range r.Verdicts {
		out = append(out, v.Denials()...)
	}
	return out
}

// Explain renders a multi-line, human-readable report. This is what the CLI
// prints when a push is refused: a denial a developer cannot act on becomes a
// support ticket.
func (r *Result) Explain() string {
	var b strings.Builder

	for _, v := range r.Verdicts {
		if v.Allowed {
			continue
		}

		fmt.Fprintf(&b, "%s\n", v.Change)

		for _, c := range v.Denials() {
			fmt.Fprintf(&b, "    %s\n", c)

			if c.Decision.Description != "" {
				fmt.Fprintf(&b, "        %s\n", c.Decision.Description)
			}
		}
	}

	return b.String()
}

// Push applies the policy in the write direction.
//
// Every section is evaluated in full, even after the first denial, so the
// author gets the complete list of problems in one round trip instead of
// discovering them one push at a time.
func Push(set *patch.Set, opts Options, mode Mode) (*Result, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	switch mode {
	case ModeReject, ModeStrip:
	case "":
		mode = ModeReject
	default:
		return nil, fmt.Errorf("enforce: unknown mode %q", mode)
	}

	engine := policy.NewCachingEngine(opts.Engine, opts.request())

	result := &Result{Mode: mode}

	for _, change := range set.Changes {
		verdict := Verdict{Change: change, Allowed: true}

		reqs := requirementsFor(change)
		reqs = append(reqs, opts.Guards.requirements(change)...)

		for _, req := range reqs {
			decision := engine.Evaluate(req.path, req.action)

			verdict.Checks = append(verdict.Checks, Check{
				Path:     req.path,
				Action:   req.action,
				Guard:    req.guard,
				Decision: decision,
			})

			if !decision.Allowed {
				verdict.Allowed = false
			}

			if result.PolicyVersion == "" {
				result.PolicyVersion = decision.PolicyVersion
			}
		}

		result.Verdicts = append(result.Verdicts, verdict)

		if verdict.Allowed {
			result.Kept = append(result.Kept, change)
		} else {
			result.Dropped = append(result.Dropped, change)
		}
	}

	if mode == ModeReject && len(result.Dropped) > 0 {
		result.Rejected = true
		return result, nil
	}

	allowed := make(map[int]bool, len(result.Kept))
	for _, c := range result.Kept {
		allowed[c.Index] = true
	}

	result.Patch = set.Render(func(c *patch.Change) bool { return allowed[c.Index] })

	return result, nil
}

// Pull applies the policy in the read direction.
//
// A pull is always filtered, never rejected: the developer receives what they
// are allowed to see, and the report tells them what was withheld so that a
// missing file is never mistaken for a deleted one.
//
// A section is kept only if every path it touches is readable. A rename with
// one readable side is dropped whole: emitting half of it would either delete a
// file the developer cannot see or create one out of nowhere.
func Pull(set *patch.Set, opts Options) (*Result, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	engine := policy.NewCachingEngine(opts.Engine, opts.request())

	result := &Result{Mode: ModeStrip}

	for _, change := range set.Changes {
		verdict := Verdict{Change: change, Allowed: true}

		for _, path := range change.Paths() {
			decision := engine.Evaluate(path, policy.ActionRead)

			verdict.Checks = append(verdict.Checks, Check{
				Path:     path,
				Action:   policy.ActionRead,
				Decision: decision,
			})

			if !decision.Allowed {
				verdict.Allowed = false
			}

			if result.PolicyVersion == "" {
				result.PolicyVersion = decision.PolicyVersion
			}
		}

		result.Verdicts = append(result.Verdicts, verdict)

		if verdict.Allowed {
			result.Kept = append(result.Kept, change)
		} else {
			result.Dropped = append(result.Dropped, change)
		}
	}

	allowed := make(map[int]bool, len(result.Kept))
	for _, c := range result.Kept {
		allowed[c.Index] = true
	}

	result.Patch = set.Render(func(c *patch.Change) bool { return allowed[c.Index] })

	return result, nil
}
