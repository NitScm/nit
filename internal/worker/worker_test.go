package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/internal/compress"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/internal/taskspec"
	"github.com/NitScm/nit/internal/worker"
	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

var testNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// harness stands up a worker over a real upstream repository.
//
// The upstream is a bare repo on disk and the remote is its path: git treats it
// exactly as it treats a remote server, so the whole clone/apply/rebase/push
// cycle is exercised for real rather than against a mock that agrees with the
// implementation by construction.
type harness struct {
	t *testing.T

	store    *memory.Store
	blobs    blob.Store
	worker   *worker.Worker
	signer   *synctoken.Signer
	upstream string // path to the bare repository

	user      *store.User
	workspace *store.Workspace
	repo      *store.Repository
}

func requireGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()

	full := filepath.Join(dir, name)

	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// testPolicy grants devs src/ and docs/, and hides secrets/ from them.
func testPolicy(t *testing.T) *policy.Policy {
	t.Helper()

	p, err := policy.Compile(policy.Spec{
		Version: "test-1",
		Users: []policy.User{
			{ID: "dev", Email: "dev@example.com"},
		},
		Groups: []policy.Group{
			{ID: "devs", Members: []policy.UserID{"dev"}},
		},
		Repositories: []policy.Repository{
			{ID: "backend-api", Forge: "generic", DefaultBranch: "main"},
		},
		Rules: map[policy.RepoID][]policy.Rule{
			"backend-api": {
				{
					ID:      "devs-own-src-and-docs",
					Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "devs"},
					Paths: []policy.Pattern{
						policy.MustParsePattern("src/"),
						policy.MustParsePattern("docs/"),
					},
					Actions: []policy.Action{
						policy.ActionRead, policy.ActionWrite,
						policy.ActionCreate, policy.ActionDelete,
					},
					Effect: policy.EffectAllow,
				},
				{
					ID:      "secrets-are-off-limits",
					Subject: policy.RuleSubject{Type: policy.SubjectTypeAny},
					Paths:   []policy.Pattern{policy.MustParsePattern("secrets/")},
					Actions: policy.AllActions,
					Effect:  policy.EffectDeny,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	return p
}

// newHarness builds an upstream repository with one commit and wires a worker.
func newHarness(t *testing.T) *harness {
	return newHarnessWithPolicy(t, testPolicy(t))
}

// newHarnessWithPolicy is newHarness for a test that needs more than one
// subject — sharing between users cannot be tested with a policy that has one.
func newHarnessWithPolicy(t *testing.T, p *policy.Policy) *harness {
	t.Helper()
	requireGit(t)

	ctx := context.Background()
	h := &harness{t: t, store: memory.New()}

	// Seed a working tree, then push it into a bare repository that stands in
	// for the forge.
	seed := t.TempDir()

	git(t, seed, "init", "--initial-branch=main", ".")
	git(t, seed, "config", "user.email", "test@example.com")
	git(t, seed, "config", "user.name", "Test")

	write(t, seed, "src/app.go", "package main\n\nfunc main() {}\n")
	write(t, seed, "docs/readme.md", "hello\n")
	write(t, seed, "secrets/prod.env", "TOKEN=abc\n")

	git(t, seed, "add", "--all")
	git(t, seed, "commit", "-m", "init")

	h.upstream = filepath.Join(t.TempDir(), "upstream.git")

	git(t, seed, "clone", "--bare", ".", h.upstream)

	// A bare repo refuses a push to its checked-out branch only when it is not
	// bare; nothing to configure here.

	blobs, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	h.blobs = blobs

	h.signer, err = newSigner([]byte(strings.Repeat("k", synctoken.MinKeyBytes)))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	h.worker, err = worker.New(worker.Config{
		WorkDir: t.TempDir(),
		Tenant:  policy.DefaultTenant,
	}, worker.Deps{
		Store:      h.store,
		Blobs:      h.blobs,
		Git:        gitx.NewExecGit(),
		Policy:     policy.OneSource{Source: policyloader.NewStatic(p)},
		SyncTokens: h.signer,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	h.user, err = h.store.Users().Upsert(ctx, &store.User{
		TenantID:     policy.DefaultTenant,
		PolicyUserID: "dev",
		Email:        "dev@example.com",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	h.workspace, err = h.store.Workspaces().Create(ctx, &store.Workspace{
		TenantID: policy.DefaultTenant,
		UserID:   h.user.ID,
		Label:    "laptop",
	})
	if err != nil {
		t.Fatalf("Create workspace: %v", err)
	}

	if err := h.store.Repositories().Reconcile(ctx, policy.DefaultTenant, []*store.Repository{{
		TenantID:      policy.DefaultTenant,
		PolicyRepoID:  "backend-api",
		Remote:        h.upstream,
		Forge:         "generic",
		DefaultBranch: "main",
	}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	h.repo, err = h.store.Repositories().ByPolicyID(ctx, policy.DefaultTenant, "backend-api")
	if err != nil {
		t.Fatalf("ByPolicyID: %v", err)
	}

	// The workspace is projected from the upstream tip, as it would be after a
	// clone. A push arrives with a sync point already recorded.
	if err := h.store.SyncPoints().Put(ctx, &store.SyncPoint{
		TenantID:       policy.DefaultTenant,
		WorkspaceID:    h.workspace.ID,
		RepositoryID:   h.repo.ID,
		Branch:         "main",
		UpstreamCommit: h.upstreamHead("main"),
		PolicyVersion:  "test-1",
	}); err != nil {
		t.Fatalf("Put sync point: %v", err)
	}

	return h
}

// upstreamHead returns the commit a branch points to in the bare repository.
func (h *harness) upstreamHead(branch string) string {
	h.t.Helper()
	return git(h.t, h.upstream, "rev-parse", "refs/heads/"+branch)
}

// upstreamFile returns the content of a file at the tip of a branch.
func (h *harness) upstreamFile(branch, path string) string {
	h.t.Helper()
	return git(h.t, h.upstream, "show", "refs/heads/"+branch+":"+path)
}

// commitUpstream adds a commit directly to the bare repository, standing in for
// another developer pushing while a task is queued.
func (h *harness) commitUpstream(branch, path, content, message string) string {
	h.t.Helper()

	work := h.t.TempDir()

	git(h.t, work, "clone", "--branch", branch, h.upstream, "w")

	repo := filepath.Join(work, "w")

	write(h.t, repo, path, content)
	git(h.t, repo, "add", "--all")
	git(h.t, repo, "-c", "user.email=other@example.com", "-c", "user.name=Other", "commit", "-m", message)
	git(h.t, repo, "push", "origin", branch)

	return h.upstreamHead(branch)
}

// storePatch puts a patch in the blob store, as the control plane would.
func (h *harness) storePatch(raw string) string {
	h.t.Helper()

	descriptor, err := h.blobs.Put(context.Background(), strings.NewReader(raw), "", 0)
	if err != nil {
		h.t.Fatalf("Put: %v", err)
	}

	return descriptor.Digest
}

func (h *harness) pushTask(spec taskspec.Push) *store.Task {
	h.t.Helper()

	payload, err := json.Marshal(spec)
	if err != nil {
		h.t.Fatalf("marshal: %v", err)
	}

	return &store.Task{
		ID:           "task-1",
		TenantID:     policy.DefaultTenant,
		RequestID:    spec.RequestID,
		Kind:         protocol.TaskPush,
		UserID:       h.user.ID,
		WorkspaceID:  h.workspace.ID,
		RepositoryID: h.repo.ID,
		Branch:       spec.Branch,
		Payload:      payload,
	}
}

func (h *harness) pullTask(spec taskspec.Pull) *store.Task {
	h.t.Helper()

	payload, err := json.Marshal(spec)
	if err != nil {
		h.t.Fatalf("marshal: %v", err)
	}

	return &store.Task{
		ID:           "task-1",
		TenantID:     policy.DefaultTenant,
		RequestID:    spec.RequestID,
		Kind:         protocol.TaskPull,
		UserID:       h.user.ID,
		WorkspaceID:  h.workspace.ID,
		RepositoryID: h.repo.ID,
		Branch:       spec.Branch,
		Payload:      payload,
	}
}

func (h *harness) basePushSpec(base, digest string) taskspec.Push {
	return taskspec.Push{
		RequestID:     "req-1",
		Repository:    "backend-api",
		Remote:        h.upstream,
		Forge:         "generic",
		Branch:        "main",
		BaseCommit:    base,
		PatchDigest:   digest,
		PatchEncoding: protocol.EncodingNone,
		Message:       "apply the change",
		AuthorName:    "dev",
		AuthorEmail:   "dev@example.com",
		UserID:        h.user.ID,
		WorkspaceID:   h.workspace.ID,
		PolicyUserID:  "dev",
		PolicyVersion: "test-1",
	}
}

// diffPatch produces a real patch by editing a clone of upstream.
func (h *harness) diffPatch(edit func(dir string)) (raw string, base string) {
	h.t.Helper()

	work := h.t.TempDir()

	git(h.t, work, "clone", "--branch", "main", h.upstream, "w")

	repo := filepath.Join(work, "w")
	base = git(h.t, repo, "rev-parse", "HEAD")

	edit(repo)

	git(h.t, repo, "add", "--all")

	raw = git(h.t, repo, "diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"--binary", "--full-index", "--find-renames", "--cached", base)

	return raw + "\n", base
}

// ---------------------------------------------------------------------------

func TestPushAppliesAndPublishes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, base := h.diffPatch(func(dir string) {
		write(t, dir, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	})

	task := h.pushTask(h.basePushSpec(base, h.storePatch(raw)))

	result, err := h.worker.Handle(ctx, task)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var push protocol.PushResult
	if err := json.Unmarshal(result, &push); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if push.UpstreamCommit == "" {
		t.Fatal("no upstream commit reported")
	}
	if push.UpstreamCommit != h.upstreamHead("main") {
		t.Errorf("reported %s but upstream is at %s", push.UpstreamCommit, h.upstreamHead("main"))
	}

	if got := h.upstreamFile("main", "src/app.go"); got != "package main\n\nfunc main() { println(1) }" {
		t.Errorf("upstream content = %q", got)
	}

	// The commit must be attributed to the authenticated identity, not to
	// whatever the client claimed.
	if author := git(t, h.upstream, "log", "-1", "--format=%an <%ae>", "refs/heads/main"); author != "dev <dev@example.com>" {
		t.Errorf("author = %q", author)
	}
	if message := git(t, h.upstream, "log", "-1", "--format=%s", "refs/heads/main"); message != "apply the change" {
		t.Errorf("message = %q", message)
	}

	// Nothing moved under this push, so the workspace is still a faithful
	// projection and its sync point advances.
	if push.NextSync == "" {
		t.Fatal("no next sync token issued for an unimpeded push")
	}

	payload, err := h.signer.Verify(push.NextSync)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if payload.UpstreamCommit != push.UpstreamCommit {
		t.Errorf("token names %s, want %s", payload.UpstreamCommit, push.UpstreamCommit)
	}

	stored, err := h.store.SyncPoints().Get(ctx, h.workspace.ID, h.repo.ID, "main")
	if err != nil {
		t.Fatalf("Get sync point: %v", err)
	}
	if stored.UpstreamCommit != push.UpstreamCommit {
		t.Errorf("stored sync point = %s, want %s", stored.UpstreamCommit, push.UpstreamCommit)
	}

	// The audit line has to name who acted. An audit trail that attributes a
	// push to the repository answers no question anyone will ask of it.
	records, err := h.store.Audit().Query(ctx, store.AuditQuery{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("Query audit: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d audit records, want 1", len(records))
	}
	if records[0].Action != "push.applied" {
		t.Errorf("Action = %q", records[0].Action)
	}
	if records[0].ActorLabel != "dev" {
		t.Errorf("ActorLabel = %q, want the author %q", records[0].ActorLabel, "dev")
	}
}

// Another developer's commit landing while the task was queued must be
// preserved, not overwritten.
func TestPushRebasesOntoConcurrentWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, base := h.diffPatch(func(dir string) {
		write(t, dir, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	})

	// Someone else pushes a different file while our task waits.
	h.commitUpstream("main", "docs/other.md", "from someone else\n", "concurrent change")

	task := h.pushTask(h.basePushSpec(base, h.storePatch(raw)))

	result, err := h.worker.Handle(ctx, task)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var push protocol.PushResult
	if err := json.Unmarshal(result, &push); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Both changes must be present upstream.
	if got := h.upstreamFile("main", "src/app.go"); !strings.Contains(got, "println(1)") {
		t.Errorf("our change is missing: %q", got)
	}
	if got := h.upstreamFile("main", "docs/other.md"); got != "from someone else" {
		t.Errorf("the concurrent change was lost: %q", got)
	}

	// The workspace no longer matches upstream — it does not have the other
	// developer's commit — so no sync token is issued and the developer must
	// pull.
	if push.NextSync != "" {
		t.Error("a sync token was issued after a rebase; the workspace is not a faithful projection any more")
	}
}

// A conflicting change cannot be resolved by anyone but its author, so it must
// fail permanently rather than cycle through the queue.
func TestPushConflictIsPermanent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, base := h.diffPatch(func(dir string) {
		write(t, dir, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	})

	h.commitUpstream("main", "src/app.go", "package main\n\nfunc main() { println(999) }\n", "conflicting change")

	task := h.pushTask(h.basePushSpec(base, h.storePatch(raw)))

	_, err := h.worker.Handle(ctx, task)
	if err == nil {
		t.Fatal("expected a conflict")
	}

	var perr *protocol.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error is not a protocol error: %v", err)
	}
	if perr.Code != protocol.CodeConflict {
		t.Errorf("code = %q, want %q", perr.Code, protocol.CodeConflict)
	}

	// Nothing must have landed.
	if got := h.upstreamFile("main", "src/app.go"); !strings.Contains(got, "999") {
		t.Errorf("the failed push modified upstream: %q", got)
	}
}

// Strip mode means what lands differs from what the author committed, so the
// workspace stops being a faithful projection.
func TestPushWithStrippedPatchDoesNotAdvanceSyncPoint(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, base := h.diffPatch(func(dir string) {
		write(t, dir, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	})

	spec := h.basePushSpec(base, h.storePatch(raw))
	spec.DroppedFiles = 2

	result, err := h.worker.Handle(ctx, h.pushTask(spec))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var push protocol.PushResult
	if err := json.Unmarshal(result, &push); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if push.NextSync != "" {
		t.Error("a stripped push must not advance the workspace's sync point")
	}
	if push.UpstreamCommit == "" {
		t.Error("the push should still have landed")
	}
}

// The forge is the one record an auditor can read without the database, so the
// trailers have to survive onto the real commit — and be readable by git's own
// trailer parser, not just present as text.
func TestPushedCommitCarriesTrailers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, base := h.diffPatch(func(dir string) {
		write(t, dir, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	})

	spec := h.basePushSpec(base, h.storePatch(raw))
	spec.DroppedFiles = 3

	if _, err := h.worker.Handle(ctx, h.pushTask(spec)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, want := range []struct{ key, value string }{
		{"Nit-User", "dev"},
		{"Nit-Request", "req-1"},
		{"Nit-Task", "task-1"},
		{"Nit-Policy-Version", "test-1"},
		{"Nit-Base-Commit", base},
		{"Nit-Workspace", string(h.workspace.ID)},
		{"Nit-Dropped", "3"},
	} {
		got := git(t, h.upstream, "log", "-1",
			"--format=%(trailers:key="+want.key+",valueonly)", "refs/heads/main")

		if got != want.value {
			t.Errorf("%s = %q, want %q", want.key, got, want.value)
		}
	}
}

// A rebase rewrites the commit. The trailers must come through it, or every
// push that queued behind another one would lose its trail.
func TestTrailersSurviveARebase(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, base := h.diffPatch(func(dir string) {
		write(t, dir, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	})

	h.commitUpstream("main", "docs/other.md", "from someone else", "unrelated")

	if _, err := h.worker.Handle(ctx, h.pushTask(h.basePushSpec(base, h.storePatch(raw)))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := git(t, h.upstream, "log", "-1", "--format=%(trailers:key=Nit-Task,valueonly)", "refs/heads/main")
	if got != "task-1" {
		t.Errorf("Nit-Task = %q after a rebase", got)
	}

	// A rebase rewrites commits, and only the author line survives it. Without
	// an explicit committer the identity stamped here is whoever ran the
	// worker, which quietly breaks the guarantee that a commit is authored and
	// committed as the authenticated user (D19) on every push that happens to
	// queue behind another one.
	for _, want := range []struct{ format, value string }{
		{"%an <%ae>", "dev <dev@example.com>"},
		{"%cn <%ce>", "dev <dev@example.com>"},
	} {
		if got := git(t, h.upstream, "log", "-1", "--format="+want.format, "refs/heads/main"); got != want.value {
			t.Errorf("%s = %q, want %q", want.format, got, want.value)
		}
	}
}

func TestPushCompressedPatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	raw, base := h.diffPatch(func(dir string) {
		write(t, dir, "docs/readme.md", "hello, world\n")
	})

	compressed, err := compress.Compress([]byte(raw), protocol.EncodingZstd)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	descriptor, err := h.blobs.Put(ctx, strings.NewReader(string(compressed)), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	spec := h.basePushSpec(base, descriptor.Digest)
	spec.PatchEncoding = protocol.EncodingZstd

	if _, err := h.worker.Handle(ctx, h.pushTask(spec)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := h.upstreamFile("main", "docs/readme.md"); got != "hello, world" {
		t.Errorf("content = %q", got)
	}
}

func TestPushMissingPatchIsPermanent(t *testing.T) {
	h := newHarness(t)

	spec := h.basePushSpec(h.upstreamHead("main"), blob.Digest([]byte("never stored")))

	_, err := h.worker.Handle(context.Background(), h.pushTask(spec))

	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != "missing_patch" {
		t.Errorf("got %v, want a permanent missing_patch error", err)
	}
}

// ---------------------------------------------------------------------------

// The pull worker's whole job: deliver what the developer may read, and nothing
// else.
func TestPullFiltersUnreadablePaths(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	from := h.upstreamHead("main")

	h.commitUpstream("main", "src/app.go", "package main\n\nfunc updated() {}\n", "update src")
	h.commitUpstream("main", "secrets/prod.env", "TOKEN=rotated\n", "rotate the secret")

	result, err := h.worker.Handle(ctx, h.pullTask(taskspec.Pull{
		RequestID:     "req-1",
		Repository:    "backend-api",
		Remote:        h.upstream,
		Forge:         "generic",
		Branch:        "main",
		FromCommit:    from,
		UserID:        h.user.ID,
		WorkspaceID:   h.workspace.ID,
		PolicyUserID:  "dev",
		PolicyVersion: "test-1",
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var pull protocol.PullResult
	if err := json.Unmarshal(result, &pull); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if pull.Patch == nil {
		t.Fatal("no patch produced")
	}
	if pull.Report.FilesWithheld != 1 {
		t.Errorf("FilesWithheld = %d, want 1", pull.Report.FilesWithheld)
	}
	if pull.Report.FilesDelivered != 1 {
		t.Errorf("FilesDelivered = %d, want 1", pull.Report.FilesDelivered)
	}

	delivered := h.fetchPatch(pull.Patch)

	if strings.Contains(delivered, "diff --git a/secrets/prod.env") {
		t.Error("the unreadable file was delivered")
	}
	if !strings.Contains(delivered, "diff --git a/src/app.go") {
		t.Error("the readable file was not delivered")
	}

	// The report names no withheld path: doing so would leak the very structure
	// the read rules exist to hide.
	if strings.Contains(string(result), "secrets/prod.env") {
		t.Error("the result names a withheld path")
	}

	if pull.NextSync == "" {
		t.Fatal("no sync token issued")
	}

	payload, err := h.signer.Verify(pull.NextSync)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if payload.UpstreamCommit != h.upstreamHead("main") {
		t.Errorf("token names %s, want the tip %s", payload.UpstreamCommit, h.upstreamHead("main"))
	}
}

// A workspace with no sync point gets every readable file, not a diff.
func TestPullSnapshotForNewWorkspace(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	result, err := h.worker.Handle(ctx, h.pullTask(taskspec.Pull{
		RequestID:     "req-1",
		Repository:    "backend-api",
		Remote:        h.upstream,
		Forge:         "generic",
		Branch:        "main",
		FromCommit:    "",
		UserID:        h.user.ID,
		WorkspaceID:   h.workspace.ID,
		PolicyUserID:  "dev",
		PolicyVersion: "test-1",
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var pull protocol.PullResult
	if err := json.Unmarshal(result, &pull); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if pull.Patch == nil {
		t.Fatal("no snapshot produced")
	}

	delivered := h.fetchPatch(pull.Patch)

	set, err := patch.Parse([]byte(delivered))
	if err != nil {
		t.Fatalf("the snapshot does not parse: %v", err)
	}

	paths := set.Paths()
	if len(paths) != 2 {
		t.Errorf("snapshot covers %v, want src/app.go and docs/readme.md", paths)
	}

	for _, p := range paths {
		if strings.HasPrefix(p, "secrets/") {
			t.Errorf("the snapshot includes %s", p)
		}
	}

	// Every section must be an addition: the workspace starts from nothing.
	for _, c := range set.Changes {
		if c.Op != patch.OpAdd {
			t.Errorf("%s is a %s, want an add in a snapshot", c.DisplayPath(), c.Op)
		}
	}
}

func TestPullUpToDate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tip := h.upstreamHead("main")

	result, err := h.worker.Handle(ctx, h.pullTask(taskspec.Pull{
		RequestID:     "req-1",
		Repository:    "backend-api",
		Remote:        h.upstream,
		Forge:         "generic",
		Branch:        "main",
		FromCommit:    tip,
		UserID:        h.user.ID,
		WorkspaceID:   h.workspace.ID,
		PolicyUserID:  "dev",
		PolicyVersion: "test-1",
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var pull protocol.PullResult
	if err := json.Unmarshal(result, &pull); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if pull.Patch != nil {
		t.Error("a patch was produced for a workspace already at the tip")
	}
	if pull.NextSync == "" {
		t.Error("no sync token issued; the client cannot confirm it is current")
	}
}

// A change entirely inside an unreadable area produces no patch at all — and
// must still move the sync point, or the client asks again forever.
func TestPullWithNothingReadableStillAdvances(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	from := h.upstreamHead("main")

	h.commitUpstream("main", "secrets/prod.env", "TOKEN=rotated\n", "rotate the secret")

	result, err := h.worker.Handle(ctx, h.pullTask(taskspec.Pull{
		RequestID:     "req-1",
		Repository:    "backend-api",
		Remote:        h.upstream,
		Forge:         "generic",
		Branch:        "main",
		FromCommit:    from,
		UserID:        h.user.ID,
		WorkspaceID:   h.workspace.ID,
		PolicyUserID:  "dev",
		PolicyVersion: "test-1",
	}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var pull protocol.PullResult
	if err := json.Unmarshal(result, &pull); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if pull.Patch != nil {
		t.Error("a patch was produced although nothing readable changed")
	}
	if pull.Report.FilesWithheld != 1 {
		t.Errorf("FilesWithheld = %d, want 1", pull.Report.FilesWithheld)
	}
	if pull.NextSync == "" {
		t.Error("the sync point must still advance, or the client asks again forever")
	}
}

// fetchPatch reads a produced patch back out of the blob store.
func (h *harness) fetchPatch(descriptor *protocol.Blob) string {
	h.t.Helper()

	reader, err := h.blobs.Get(context.Background(), descriptor.Digest)
	if err != nil {
		h.t.Fatalf("Get: %v", err)
	}
	defer reader.Close()

	compressed, err := io.ReadAll(reader)
	if err != nil {
		h.t.Fatalf("ReadAll: %v", err)
	}

	raw, err := compress.Decompress(compressed, descriptor.Encoding, 0)
	if err != nil {
		h.t.Fatalf("Decompress: %v", err)
	}

	return string(raw)
}

func TestUnknownTaskKind(t *testing.T) {
	h := newHarness(t)

	_, err := h.worker.Handle(context.Background(), &store.Task{
		Kind:    "nonsense",
		Payload: []byte(`{}`),
	})
	if err == nil {
		t.Error("an unknown task kind must fail")
	}
}
