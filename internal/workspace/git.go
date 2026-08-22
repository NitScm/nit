package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runGit executes git in a directory.
//
// The workspace package shells out for the handful of operations that are
// client-side only — init, status — rather than widening the gitx.Repo
// interface, which exists to describe what a *worker* needs. Keeping the two
// separate stops server-side code from growing a dependency on commands that
// only make sense next to a developer.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// runGitInit creates a repository with the branch nit will track.
//
// The branch is named at init time so the first synchronization commit lands on
// it: a checkout whose local branch is "master" while the server tracks "main"
// works, but every later message about it is confusing.
func runGitInit(ctx context.Context, dir, branch string) error {
	if branch == "" {
		branch = "main"
	}

	if _, err := runGit(ctx, dir, "init", "--initial-branch="+branch, "."); err != nil {
		return err
	}

	// A workspace is not a place people configure git by hand, and a machine
	// with no global identity would fail at the first synchronization commit
	// with an error that says nothing about nit.
	if _, err := runGit(ctx, dir, "config", "user.name", "nit"); err != nil {
		return err
	}
	if _, err := runGit(ctx, dir, "config", "user.email", "nit@localhost"); err != nil {
		return err
	}

	return nil
}
