package policy

// Source provides the bundle currently in force.
//
// The interface is here, in the engine's own package, rather than beside the
// loader that watches a directory: a directory is one way to obtain a bundle,
// not the definition of one. An implementation may read from object storage,
// generate rules from another system, or — the case this was extracted for —
// compose a bundle from files with group membership resolved against a company
// directory, so that `subject: {type: group, id: platform}` means what that
// company already means by it.
//
// What an implementation may not do is decide anything. It returns a compiled
// bundle; Evaluate does the rest, and stays the single decision point (D9).
//
// See docs/EXTENSIONS.md.
type Source interface {
	// Current returns the bundle to evaluate against. It is called on the
	// request path, so it must be cheap and must not block: implementations
	// that fetch from elsewhere refresh in the background and serve the last
	// good bundle meanwhile, which is what the directory loader does.
	Current() *Policy
}

// Static is a Source wrapping one immutable bundle.
//
// Useful in tests, in the worker, and anywhere the bundle is decided once at
// start-up rather than watched.
type Static struct{ Policy *Policy }

// Current implements Source.
func (s Static) Current() *Policy { return s.Policy }
