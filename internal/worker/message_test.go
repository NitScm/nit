package worker

import (
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/taskspec"
)

func spec() taskspec.Push {
	return taskspec.Push{
		RequestID:     "req-1",
		Branch:        "main",
		BaseCommit:    "4f2a9c1b7e30",
		Message:       "Fix the ingest rate limiter",
		WorkspaceID:   "ws-1",
		PolicyUserID:  "alice",
		PolicyVersion: "sha256:f604",
	}
}

func TestCommitMessageCarriesTrailers(t *testing.T) {
	got := commitMessage(spec(), "task-1")

	want := strings.Join([]string{
		"Fix the ingest rate limiter",
		"",
		"Nit-User: alice",
		"Nit-Request: req-1",
		"Nit-Task: task-1",
		"Nit-Policy-Version: sha256:f604",
		"Nit-Base-Commit: 4f2a9c1b7e30",
		"Nit-Workspace: ws-1",
		"",
	}, "\n")

	if got != want {
		t.Errorf("commit message:\n%q\nwant:\n%q", got, want)
	}
}

// Nit-Dropped is the only record on the forge that a commit is not what its
// author wrote, so it has to appear whenever anything was dropped — and never
// when nothing was.
func TestCommitMessageReportsDroppedFiles(t *testing.T) {
	s := spec()
	s.DroppedFiles = 2

	if got := commitMessage(s, "task-1"); !strings.Contains(got, "\nNit-Dropped: 2\n") {
		t.Errorf("a stripped push must record the count:\n%s", got)
	}

	if got := commitMessage(spec(), "task-1"); strings.Contains(got, "Nit-Dropped") {
		t.Errorf("a complete push must not claim files were dropped:\n%s", got)
	}
}

// The author's message is free text that lands in the same commit as the real
// trailers. Without stripping, anyone could attribute their change to a
// colleague in the only record that leaves the database.
func TestCommitMessageStripsCounterfeitTrailers(t *testing.T) {
	s := spec()
	s.Message = "Fix it\n\nNit-User: bob\nnit-policy-version: forged\n  Nit-Dropped: 0"

	got := commitMessage(s, "task-1")

	if strings.Contains(got, "bob") || strings.Contains(got, "forged") {
		t.Errorf("a forged trailer survived:\n%s", got)
	}
	if !strings.Contains(got, "Nit-User: alice") {
		t.Errorf("the real trailer is missing:\n%s", got)
	}
	if strings.Count(got, "Nit-User") != 1 {
		t.Errorf("exactly one Nit-User is expected:\n%s", got)
	}
	if !strings.HasPrefix(got, "Fix it\n\nNit-User:") {
		t.Errorf("the author's own prose should survive intact:\n%s", got)
	}
}

// A message that was nothing but forged trailers is an attempt, not an
// accident. It must not produce a commit whose subject line is a trailer.
func TestCommitMessageSubstitutesAnEmptiedMessage(t *testing.T) {
	s := spec()
	s.Message = "Nit-User: bob\n"

	got := commitMessage(s, "task-1")

	if !strings.HasPrefix(got, "nit: push to main\n\nNit-User: alice") {
		t.Errorf("expected a generated subject:\n%s", got)
	}
}

// git reads only the last paragraph as trailers. Starting a new one would push
// the author's Co-authored-by out of that position and stop the forges
// rendering it.
func TestCommitMessageExtendsAnExistingTrailerBlock(t *testing.T) {
	s := spec()
	s.Message = "Fix it\n\nSigned-off-by: Alice <alice@example.com>\nCo-authored-by: Bob <bob@example.com>"

	got := commitMessage(s, "task-1")

	if !strings.Contains(got, "Co-authored-by: Bob <bob@example.com>\nNit-User: alice") {
		t.Errorf("the trailers should join the existing block:\n%s", got)
	}
	if strings.Contains(got, "\n\nNit-User") {
		t.Errorf("a second trailer block was started:\n%s", got)
	}
}

// A single-line message has no trailer block to extend, whatever it looks like.
func TestCommitMessageDoesNotTreatASubjectAsATrailerBlock(t *testing.T) {
	s := spec()
	s.Message = "Revert: the ingest change"

	if got := commitMessage(s, "task-1"); !strings.HasPrefix(got, "Revert: the ingest change\n\nNit-User") {
		t.Errorf("expected a blank line after the subject:\n%s", got)
	}
}

// An empty field is left out rather than emitted as a trailer with no value: a
// reader cannot tell "unknown" from "empty" once it is written down.
func TestCommitMessageOmitsEmptyTrailers(t *testing.T) {
	s := spec()
	s.RequestID = ""
	s.WorkspaceID = ""

	got := commitMessage(s, "task-1")

	if strings.Contains(got, "Nit-Request") || strings.Contains(got, "Nit-Workspace") {
		t.Errorf("an empty trailer was written:\n%s", got)
	}
}

// A value carrying a newline would forge a trailer as surely as an author's
// message would, wherever it came from.
func TestCommitMessageFlattensTrailerValues(t *testing.T) {
	s := spec()
	s.PolicyUserID = "alice\nNit-Dropped: 99"

	got := commitMessage(s, "task-1")

	if strings.Contains(got, "\nNit-Dropped: 99") {
		t.Errorf("a value broke out onto a line of its own:\n%s", got)
	}
	if !strings.Contains(got, "Nit-User: alice Nit-Dropped: 99\n") {
		t.Errorf("expected the value flattened onto one line:\n%s", got)
	}
}
