// Package patch models a git patch as a sequence of independent per-file
// sections and provides byte-exact filtering of those sections.
//
// The central design constraint is fidelity: nit rewrites patches on behalf of
// users, and a rewritten patch that differs from the author's intent in any way
// other than the removal of unauthorized files is a bug. Rather than parsing a
// patch into a model and rendering that model back to text (which loses
// information on binary chunks, unusual line endings, and non-UTF-8 paths),
// this package splits the raw bytes at file boundaries and re-emits the
// original bytes of the sections that survive filtering.
//
// Metadata extraction (paths, modes, operation, binary flag) is delegated to
// github.com/bluekeyes/go-gitdiff; the byte ranges are computed here.
//
// The expected input is the output of "git diff --binary --full-index". A
// leading preamble (for example the commit headers produced by
// "git format-patch") is tolerated and preserved verbatim.
package patch
