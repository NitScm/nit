package policyloader_test

import (
	"path/filepath"
	"testing"

	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/pkg/policy/policytest"
)

// TestConformance runs the shared Source suite against the loader.
//
// The loader is the implementation every other one is measured against, so it
// has to pass the same assertions an out-of-tree Source does — including the
// ones about a bundle that fails to compile, which is the behaviour a directory
// watcher is uniquely able to get wrong.
func TestConformance(t *testing.T) {
	policytest.Run(t, func(t *testing.T) policytest.Harness {
		dir := t.TempDir()

		writeConformanceBundle(t, dir, true)

		loader, err := policyloader.New(dir, quiet())
		if err != nil {
			t.Fatalf("policyloader.New: %v", err)
		}

		return policytest.Harness{
			Source: loader,

			// Reload rather than waiting for the watcher: the suite asserts
			// what the loader does, not how fast a timer fires, and a test that
			// slept would be slow when it passed and flaky when it failed.
			Publish: func(t *testing.T, readable bool) {
				writeConformanceBundle(t, dir, readable)

				if _, err := loader.Reload(); err != nil {
					t.Fatalf("Reload: %v", err)
				}
			},

			Break: func(t *testing.T) {
				write(t, filepath.Join(dir, "users.yaml"), "this: is not: a list\n  of users\n")

				// The error is the point, and it must not reach the served
				// bundle. A Reload that returned nil here would mean the loader
				// accepted something that does not compile.
				if _, err := loader.Reload(); err == nil {
					t.Fatal("Reload accepted a bundle that does not compile")
				}
			},
		}
	})
}

// writeBundle renders the shape policytest expects into the on-disk format.
//
// Written by hand rather than generated from policytest.Bundle: the suite
// speaks specs and the loader reads files, and something has to translate. Kept
// minimal on purpose — a bundle with one user, one group, one repository and
// one rule is enough to observe every assertion the suite makes.
func writeConformanceBundle(t *testing.T, dir string, readable bool) {
	t.Helper()

	effect := "deny"
	if readable {
		effect = "allow"
	}

	write(t, filepath.Join(dir, "users.yaml"), `
- id: dev
  email: dev@example.com
`)

	write(t, filepath.Join(dir, "groups.yaml"), `
- id: devs
  members: [dev]
`)

	write(t, filepath.Join(dir, "repositories.yaml"), `
- id: repo
  remote: https://example.com/r.git
  forge: github
  default_branch: main
`)

	write(t, filepath.Join(dir, "repositories", "repo", "rules.yaml"), `
- id: devs-read
  subject:
    type: group
    id: devs
  paths: ["**"]
  actions: [read]
  effect: `+effect+`
`)
}
