// Package enforce applies a policy to a patch.
//
// It is the heart of nit and it is deliberately pure: given a parsed patch, a
// policy engine and a subject, it produces a filtered patch and a full report,
// with no IO, no clock and no git. Everything that can go wrong in
// authorization can therefore be reproduced by a table-driven test.
//
// Two directions, with different failure modes:
//
// Push (write direction) is fail-closed. If a patch touches a path the author
// may not write, the whole push is rejected with a report naming the offending
// paths and the rules that refused them. Silently stripping the file would be
// worse than refusing: the author would believe their change landed, upstream
// would receive a partial commit that may not even build, and the next pull
// would quietly revert their work. Stripping is available under ModeStrip, but
// only when the client explicitly asks for it.
//
// Pull (read direction) always filters. There is no meaningful way to "reject"
// a pull: the developer simply does not receive what they are not allowed to
// see, and the report tells them how many sections were withheld.
//
// Beyond per-path rules, enforce applies guards against the changes that
// subvert the model itself rather than violating it directly: a symlink
// pointing into a subtree the author cannot read, a submodule pointer to
// arbitrary content, and edits to CI definitions or .gitattributes, all of
// which turn write access to one harmless path into read access to everything.
package enforce
