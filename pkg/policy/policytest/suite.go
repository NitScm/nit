// Package policytest is a conformance suite every policy.Source must pass.
//
// A Source sits on the request path of every push and every pull, and what it
// returns decides what a developer may read and write. The obligations that
// matter are therefore not "it returns a bundle" but the ones that are easy to
// get wrong and invisible when you do:
//
//   - Current is cheap and does not block, because it is called per request.
//   - Current never returns nil, because a nil bundle is a panic in the
//     authorization path.
//   - A bundle that fails to compile changes nothing. The last good one stays
//     in force — failing open would grant access nobody authorized, and failing
//     closed would take an outage on every typo.
//   - A bundle already handed out never changes underneath its holder. A worker
//     calls Current once and uses it for a whole task; a bundle that mutated
//     mid-task would make its decisions disagree with the audit record of them.
//
// The last one is not a nicety. pkg/policy.Profile fingerprints a subject's
// rights so a filtered projection can be shared, and the fingerprint is only
// meaningful if the bundle it was computed from is the bundle that was applied.
package policytest

import (
	"sync"
	"testing"
	"time"

	"github.com/NitScm/nit/pkg/policy"
)

// Harness lets the suite drive an implementation.
//
// Publish and Break are optional. A Source that cannot change — policy.Static
// is the obvious one — leaves them nil and the refresh assertions skip
// themselves rather than being paraphrased into something weaker.
type Harness struct {
	Source policy.Source

	// Publish installs a bundle in which the user "dev" may read everything in
	// the repository "repo", or may not, and returns once Current would serve
	// it. Blocking here rather than sleeping in the suite is what keeps these
	// tests from being timing-dependent.
	//
	// The suite asks for a *behaviour*, not a version. A Source whose version
	// is a hash of its bundle — which the directory loader's is — cannot honour
	// one chosen by a caller, and a suite that demanded it would be
	// unimplementable by the implementation it was written for.
	//
	// Build the bundle with Bundle, which has the shape the assertions expect:
	// a user "dev" in a group "devs", a repository "repo", and one read rule.
	Publish func(t *testing.T, readable bool)

	// Break installs a bundle that does not compile, and returns once the
	// implementation has had the chance to notice.
	Break func(t *testing.T)
}

// Factory builds a harness for one test.
type Factory func(t *testing.T) Harness

// Run executes the whole suite against an implementation.
func Run(t *testing.T, newHarness Factory) {
	t.Helper()

	t.Run("CurrentIsNeverNil", func(t *testing.T) { testNeverNil(t, newHarness) })
	t.Run("CurrentIsUsable", func(t *testing.T) { testUsable(t, newHarness) })
	t.Run("CurrentIsCheap", func(t *testing.T) { testCheap(t, newHarness) })
	t.Run("CurrentIsSafeForConcurrentUse", func(t *testing.T) { testConcurrent(t, newHarness) })
	t.Run("CurrentIsStableWhenNothingChanges", func(t *testing.T) { testStable(t, newHarness) })
	t.Run("APublishedBundleTakesEffect", func(t *testing.T) { testPublish(t, newHarness) })
	t.Run("ABundleAlreadyHandedOutDoesNotChange", func(t *testing.T) { testImmutable(t, newHarness) })
	t.Run("ABrokenBundleChangesNothing", func(t *testing.T) { testBroken(t, newHarness) })
}

// Bundle returns a spec the suite can publish, with one rule whose effect is
// easy to assert.
//
// Exported so an implementation's own tests can build the same shape, and so a
// harness that has to seed something before the suite runs starts from what the
// suite expects.
func Bundle(version string, readable bool) policy.Spec {
	effect := policy.EffectDeny
	if readable {
		effect = policy.EffectAllow
	}

	return policy.Spec{
		Tenant:  policy.DefaultTenant,
		Version: version,
		Users:   []policy.User{{ID: "dev", Email: "dev@example.com"}},
		Groups:  []policy.Group{{ID: "devs", Members: []policy.UserID{"dev"}}},
		Repositories: []policy.Repository{{
			ID: "repo", Remote: "https://example.com/r.git", Forge: "github", DefaultBranch: "main",
		}},
		Rules: map[policy.RepoID][]policy.Rule{
			"repo": {{
				ID:      "devs-read",
				Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "devs"},
				Paths:   []policy.Pattern{policy.MustParsePattern("**")},
				Actions: []policy.Action{policy.ActionRead},
				Effect:  effect,
			}},
		},
	}
}

// canRead asks the question the suite uses to tell two bundles apart by
// behaviour rather than by version string.
func canRead(t *testing.T, p *policy.Policy) bool {
	t.Helper()

	subject, err := p.Subject("dev")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	return p.Evaluate(policy.Request{
		Repo:    "repo",
		Ref:     "refs/heads/main",
		Subject: subject,
		Path:    "src/app.go",
		Action:  policy.ActionRead,
	}).Allowed
}

func testNeverNil(t *testing.T, newHarness Factory) {
	h := newHarness(t)

	if h.Source.Current() == nil {
		t.Fatal("Current returned nil; the authorization path would panic on the next request")
	}
}

// A bundle has to arrive ready to evaluate. A Source that returned something
// half-built would push the failure into the request path, where the only
// available answer is a 500.
func testUsable(t *testing.T, newHarness Factory) {
	h := newHarness(t)
	p := h.Source.Current()

	if p.Version() == "" {
		t.Error("the bundle has no version; a sync point and an audit record both name one")
	}

	if _, err := p.Subject("dev"); err != nil {
		t.Errorf("Subject on a freshly served bundle: %v", err)
	}
}

// Called once per request, so an implementation that fetched anything per call
// would put a network round trip in front of every push.
func testCheap(t *testing.T, newHarness Factory) {
	h := newHarness(t)

	const calls = 10_000

	start := time.Now()

	for range calls {
		if h.Source.Current() == nil {
			t.Fatal("Current returned nil")
		}
	}

	// Wildly generous for an atomic load — 10µs each — and still orders of
	// magnitude below anything that reads a file or opens a connection.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("%d calls took %s; Current is on the request path and must not do work",
			calls, elapsed)
	}
}

func testConcurrent(t *testing.T, newHarness Factory) {
	h := newHarness(t)

	var wg sync.WaitGroup

	for range 16 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range 500 {
				if h.Source.Current() == nil {
					t.Error("Current returned nil")
					return
				}
			}
		}()
	}

	// Concurrent with a publish, if the implementation supports one: a reader
	// racing a refresh is the ordinary case, not an edge one.
	if h.Publish != nil {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range 5 {
				h.Publish(t, i%2 == 0)
			}
		}()
	}

	wg.Wait()
}

func testStable(t *testing.T, newHarness Factory) {
	h := newHarness(t)

	first := h.Source.Current().Version()

	for range 100 {
		if got := h.Source.Current().Version(); got != first {
			t.Fatalf("the version changed to %s with no publish in between", got)
		}
	}
}

func testPublish(t *testing.T, newHarness Factory) {
	h := newHarness(t)
	if h.Publish == nil {
		t.Skip("this Source cannot be changed")
	}

	h.Publish(t, true)

	first := h.Source.Current()

	if !canRead(t, first) {
		t.Error("the published bundle allows the read, and the served one does not")
	}

	h.Publish(t, false)

	second := h.Source.Current()

	// The decision, not only the version: a Source serving a stale bundle under
	// a fresh version string would pass a version check and apply the wrong
	// rules, which is the failure a version comparison cannot see.
	if canRead(t, second) {
		t.Error("the second bundle denies the read, and the served one still allows it")
	}

	// And the version moves with it, because a sync point taken under one
	// bundle has to be distinguishable from one taken under the next.
	if first.Version() == second.Version() {
		t.Errorf("both bundles report version %q, though they decide differently",
			first.Version())
	}
}

// A worker calls Current once and uses that bundle for a whole task. If it
// changed underneath, the task's decisions and the audit record of them would
// describe different rule sets.
func testImmutable(t *testing.T, newHarness Factory) {
	h := newHarness(t)
	if h.Publish == nil {
		t.Skip("this Source cannot be changed")
	}

	h.Publish(t, true)

	held := h.Source.Current()
	version := held.Version()

	h.Publish(t, false)

	if held.Version() != version {
		t.Errorf("a bundle already handed out changed version, %q to %q", version, held.Version())
	}
	if !canRead(t, held) {
		t.Error("a bundle already handed out changed its decision after a publish")
	}
}

// Failing open would grant access nobody authorized. Failing closed would take
// an outage on every typo. Neither: the last good bundle stays in force.
func testBroken(t *testing.T, newHarness Factory) {
	h := newHarness(t)
	if h.Break == nil || h.Publish == nil {
		t.Skip("this Source cannot be given a broken bundle")
	}

	h.Publish(t, true)

	before := h.Source.Current()

	h.Break(t)

	after := h.Source.Current()

	if after == nil {
		t.Fatal("Current returned nil after a bundle failed to compile")
	}
	if after.Version() != before.Version() {
		t.Errorf("version moved from %q to %q; a bundle that does not compile changes nothing",
			before.Version(), after.Version())
	}
	if !canRead(t, after) {
		t.Error("the served bundle stopped allowing what the last good one allowed")
	}

	// And recovery works: a broken bundle must not wedge the source.
	h.Publish(t, false)

	if canRead(t, h.Source.Current()) {
		t.Error("the source did not recover after a broken bundle")
	}
}
