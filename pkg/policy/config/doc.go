// Package config loads a policy bundle from disk into a compiled policy.
//
// The bundle is the source of truth and it lives in files, not in a database.
// That is a deliberate choice: authorization rules are reviewed, versioned and
// rolled back exactly like code, through pull requests, with history and
// blame. A database row has none of those properties, and "who granted this
// access and when?" is the first question asked after an incident.
//
// The bundle layout is:
//
//	users.yaml
//	groups.yaml
//	repositories.yaml
//	repositories/<repository-id>/rules.yaml
//
// Decoding is strict: an unknown field is an error, not a shrug. A typo in a
// security policy that silently disables a rule is the worst possible failure
// mode, so the loader refuses to guess.
//
// Every load computes a Version: a content hash over the whole bundle, carried
// into every Decision and every audit record, so that any past decision can be
// replayed against the exact rules that produced it.
package config
