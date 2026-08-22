package worker

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/internal/taskspec"
)

// Trailers on the commit nit publishes.
//
// The forge is the one place an auditor, a reviewer or a compliance export can
// reach without a database. Identity already survives the trip — the commit is
// authored by the authenticated user — but everything else about the decision
// lived only in the audit table, so a commit seen on GitHub could not be traced
// back to the request that produced it, to the policy version that authorized
// it, or, worst of all, to the fact that files were dropped from it.
//
// These are git trailers, so `git interpret-trailers`, `git log --grep` and the
// forges' own parsers all read them without special support:
//
//	Fix the ingest rate limiter
//
//	Nit-User: alice
//	Nit-Request: 01J8Z3Q2M7C4V9K1
//	Nit-Task: be59af45-9694-416e-ace2-da5cffc7f145
//	Nit-Policy-Version: sha256:f6040b6d6a8381dc
//	Nit-Base-Commit: 4f2a9c1b7e30
//	Nit-Workspace: 7c1e9f2a-3b4c-4d5e-8f90-a1b2c3d4e5f6
//	Nit-Dropped: 2
//
// Nit-User is the bundle identity rather than the author's display name: names
// and addresses collide and change, and the bundle id is the key every other
// record uses.
//
// Nit-Dropped is a count, not a list. The paths are in the audit trail, where
// they can be queried; an unbounded commit message is a poor place for them.
const (
	trailerUser          = "Nit-User"
	trailerRequest       = "Nit-Request"
	trailerTask          = "Nit-Task"
	trailerPolicyVersion = "Nit-Policy-Version"
	trailerBaseCommit    = "Nit-Base-Commit"
	trailerWorkspace     = "Nit-Workspace"
	trailerDropped       = "Nit-Dropped"
)

// counterfeitTrailer matches any line claiming to be one of nit's trailers.
//
// The author's message is free text that ends up in the same commit as the real
// trailers, so without this a developer could write
//
//	nit push -m $'Fix it\n\nNit-User: bob'
//
// and attribute their change to a colleague in the only record that leaves the
// database. Matching the whole Nit- namespace rather than the seven keys above
// means a trailer added later is protected the day it is added, not the day
// somebody remembers to update this expression.
var counterfeitTrailer = regexp.MustCompile(`(?im)^[ \t]*Nit-[A-Za-z0-9-]*[ \t]*:.*$`)

// trailerLine matches a line that git would read as a trailer.
var trailerLine = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*:[ \t]`)

// commitMessage builds the message of the commit that lands upstream.
func commitMessage(spec taskspec.Push, taskID store.ID) string {
	trailers := []struct{ key, value string }{
		{trailerUser, string(spec.PolicyUserID)},
		{trailerRequest, spec.RequestID},
		{trailerTask, string(taskID)},
		{trailerPolicyVersion, spec.PolicyVersion},
		{trailerBaseCommit, spec.BaseCommit},
		{trailerWorkspace, string(spec.WorkspaceID)},
	}

	var block strings.Builder

	for _, trailer := range trailers {
		value := sanitizeTrailerValue(trailer.value)
		if value == "" {
			continue
		}

		block.WriteString(trailer.key + ": " + value + "\n")
	}

	if spec.DroppedFiles > 0 {
		block.WriteString(trailerDropped + ": " + strconv.Itoa(spec.DroppedFiles) + "\n")
	}

	return appendTrailers(sanitizeMessage(spec.Message, spec.Branch), block.String())
}

// sanitizeMessage strips counterfeit trailers from the author's message.
//
// A message left empty by that — one that was nothing but forged trailers — is
// an attempt, not an accident, so it gets a subject of nit's own rather than a
// commit whose first line is a trailer.
func sanitizeMessage(message, branch string) string {
	cleaned := strings.TrimSpace(counterfeitTrailer.ReplaceAllString(message, ""))

	// Removing lines from the middle of a message leaves the blank lines that
	// surrounded them; collapse the runs so the result reads like prose.
	cleaned = strings.TrimSpace(regexp.MustCompile(`\n{3,}`).ReplaceAllString(cleaned, "\n\n"))

	if cleaned == "" {
		return "nit: push to " + branch
	}

	return cleaned
}

// appendTrailers joins a message and a trailer block.
//
// git only reads the *last* paragraph of a message as trailers. When the author
// already ended theirs with one — Co-authored-by, Signed-off-by — starting a
// new paragraph would push theirs out of that position and stop every forge
// rendering it, so the lines join the existing block instead.
func appendTrailers(message, block string) string {
	if block == "" {
		return message
	}

	separator := "\n\n"
	if endsWithTrailerBlock(message) {
		separator = "\n"
	}

	return message + separator + block
}

// endsWithTrailerBlock reports whether the last paragraph is entirely trailers.
func endsWithTrailerBlock(message string) bool {
	paragraphs := strings.Split(strings.TrimSpace(message), "\n\n")

	last := paragraphs[len(paragraphs)-1]

	// A subject line on its own is not a trailer block, whatever it looks like:
	// a commit message that is one line long has no trailers to extend.
	if len(paragraphs) == 1 {
		return false
	}

	for _, line := range strings.Split(last, "\n") {
		if !trailerLine.MatchString(line) {
			return false
		}
	}

	return true
}

// sanitizeTrailerValue flattens a value onto one line.
//
// Every value here comes from the server rather than from the client, so this
// guards against a policy bundle with a surprising user id rather than against
// an attack — but a newline in a value would forge a trailer just as well
// wherever it came from.
func sanitizeTrailerValue(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
}
