package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ExecGit runs the git binary.
type ExecGit struct {
	// Binary is the git executable; defaults to "git".
	Binary string

	// Env is added to the environment of every invocation, for credentials and
	// proxy settings.
	Env []string
}

// NewExecGit returns an ExecGit using the git binary found on PATH.
func NewExecGit() *ExecGit {
	return &ExecGit{Binary: "git"}
}

func (g *ExecGit) binary() string {
	if g.Binary == "" {
		return "git"
	}
	return g.Binary
}

// baseEnv returns the environment shared by every invocation.
//
// Interactive prompts are disabled: a worker that blocks on a credential prompt
// holds its branch queue until the lease expires, and the failure looks like a
// hang rather than an authentication error.
func (g *ExecGit) baseEnv() []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
	)
	return append(env, g.Env...)
}

func (g *ExecGit) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.binary(), args...)
	cmd.Dir = dir
	cmd.Env = g.baseEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

func (g *ExecGit) runStdin(ctx context.Context, dir string, stdin []byte, args ...string) error {
	cmd := exec.CommandContext(ctx, g.binary(), args...)
	cmd.Dir = dir
	cmd.Env = g.baseEnv()
	cmd.Stdin = bytes.NewReader(stdin)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

// Version implements Git.
func (g *ExecGit) Version(ctx context.Context) (string, error) {
	out, err := g.run(ctx, "", "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Clone implements Git.
func (g *ExecGit) Clone(ctx context.Context, remote, dir string, opts CloneOptions) (Repo, error) {
	args := []string{"clone"}

	if opts.Bare {
		args = append(args, "--bare")
	}
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch, "--single-branch")
	}
	if opts.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(opts.Depth))
	}

	args = append(args, "--", remote, dir)

	if _, err := g.run(ctx, "", args...); err != nil {
		return nil, err
	}

	return &execRepo{git: g, dir: dir}, nil
}

// Open implements Git.
func (g *ExecGit) Open(ctx context.Context, dir string) (Repo, error) {
	if _, err := g.run(ctx, dir, "rev-parse", "--git-dir"); err != nil {
		return nil, err
	}
	return &execRepo{git: g, dir: dir}, nil
}

type execRepo struct {
	git *ExecGit
	dir string
}

func (r *execRepo) Dir() string { return r.dir }

func (r *execRepo) Close() error { return nil }

func (r *execRepo) ResolveRef(ctx context.Context, ref string) (string, error) {
	out, err := r.git.run(ctx, r.dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *execRepo) Fetch(ctx context.Context, remote string, refspecs ...string) error {
	args := append([]string{"fetch", "--prune", remote}, refspecs...)
	_, err := r.git.run(ctx, r.dir, args...)
	return err
}

func (r *execRepo) Checkout(ctx context.Context, revision string) error {
	_, err := r.git.run(ctx, r.dir, "checkout", "--force", revision)
	return err
}

// diffArgs are the flags nit's patch pipeline depends on.
//
//	--binary       binary files travel as deltas instead of being skipped
//	--full-index   full blob hashes, required for three-way application
//	--find-renames renames stay renames instead of a delete plus an add, which
//	               matters because a rename is authorized on both sides
//	--no-color, --no-ext-diff, --no-textconv
//	               a user's diff configuration must not alter what nit
//	               authorizes; an external differ could hide a change entirely
var diffArgs = []string{
	"--no-color",
	"--no-ext-diff",
	"--no-textconv",
	"--binary",
	"--full-index",
	"--find-renames",
}

func (r *execRepo) Diff(ctx context.Context, from, to string) ([]byte, error) {
	args := append([]string{"diff"}, diffArgs...)
	args = append(args, from, to)

	return r.git.run(ctx, r.dir, args...)
}

func (r *execRepo) ChangedPaths(ctx context.Context, from, to string) ([]string, error) {
	out, err := r.git.run(ctx, r.dir,
		"diff", "--name-only", "--no-renames", "-z", from, to)
	if err != nil {
		return nil, err
	}

	// -z separates paths with NUL, which is the only way to handle paths
	// containing newlines without guessing.
	var paths []string
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}

	return paths, nil
}

func (r *execRepo) Apply(ctx context.Context, patch []byte, opts ApplyOptions) error {
	args := []string{"apply", "--whitespace=nowarn"}

	if opts.ThreeWay {
		args = append(args, "--3way")
	}
	if opts.Check {
		args = append(args, "--check")
	}

	if err := r.git.runStdin(ctx, r.dir, patch, args...); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}

	return nil
}

// Rebase replays the current branch onto another commit.
func (r *execRepo) Rebase(ctx context.Context, onto string, committer Author) error {
	// A rebase rewrites commits, so git needs a committer. Two things go wrong
	// without one, and the second is the quiet one: on a machine with no
	// configured identity git refuses outright, and on a machine that has one
	// it stamps that identity — so a push that happened to be rebased is
	// committed by whoever runs the worker rather than by the developer who
	// pushed it.
	_, err := r.git.run(ctx, r.dir,
		"-c", "user.name="+committer.Name,
		"-c", "user.email="+committer.Email,
		"rebase", onto)
	if err == nil {
		return nil
	}

	// Leave the clone usable. A half-rebased worktree would poison the clone
	// cache for every later task that reuses it. The abort fails harmlessly
	// when the rebase never started, which is why its error is only reported
	// alongside the real one.
	if _, abortErr := r.git.run(ctx, r.dir, "rebase", "--abort"); abortErr != nil && !isConflict(err) {
		return fmt.Errorf("%w (and the abort failed: %v)", err, abortErr)
	}

	// Only a conflict is a conflict. Reporting anything else as one sends a
	// developer to resolve something that is not there — and they will land
	// back here every time, because nothing they can do will fix it.
	if isConflict(err) {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}

	return err
}

// isConflict reports whether git failed because the replay did not apply.
//
// git says so in words rather than in an exit code: every rebase failure exits
// non-zero, and a missing identity leaves a rebase in progress exactly as a
// conflict does, so the state of the worktree cannot tell them apart either.
func isConflict(err error) bool {
	if err == nil {
		return false
	}

	text := err.Error()

	return strings.Contains(text, "CONFLICT") ||
		strings.Contains(text, "could not apply") ||
		strings.Contains(text, "patch does not apply")
}

// EmptyTree returns the hash of the empty tree, writing the object if needed.
func (r *execRepo) EmptyTree(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, r.git.binary(), "hash-object", "-t", "tree", "-w", "--stdin")
	cmd.Dir = r.dir
	cmd.Env = r.git.baseEnv()
	cmd.Stdin = bytes.NewReader(nil)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git hash-object: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

func (r *execRepo) CommitAll(ctx context.Context, message string, author Author) (string, error) {
	if _, err := r.git.run(ctx, r.dir, "add", "--all"); err != nil {
		return "", err
	}

	args := []string{
		"-c", "user.name=" + author.Name,
		"-c", "user.email=" + author.Email,
		"commit",
		"--message", message,
		"--author", fmt.Sprintf("%s <%s>", author.Name, author.Email),
	}
	if !author.When.IsZero() {
		args = append(args, "--date", author.When.Format("2006-01-02T15:04:05-07:00"))
	}

	if _, err := r.git.run(ctx, r.dir, args...); err != nil {
		return "", err
	}

	return r.ResolveRef(ctx, "HEAD")
}

func (r *execRepo) Push(ctx context.Context, remote, branch string, opts PushOptions) error {
	args := []string{"push"}

	if opts.ExpectedRemoteCommit != "" {
		args = append(args, "--force-with-lease="+branch+":"+opts.ExpectedRemoteCommit)
	}

	args = append(args, remote, "HEAD:refs/heads/"+branch)

	_, err := r.git.run(ctx, r.dir, args...)
	return err
}

// Compile-time checks that the exec implementation satisfies the interfaces.
var (
	_ Git  = (*ExecGit)(nil)
	_ Repo = (*execRepo)(nil)
)
