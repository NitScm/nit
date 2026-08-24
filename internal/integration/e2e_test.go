// Package integration exercises the whole product loop: a client calls the
// API, a worker executes the task, and a real git repository changes.
//
// Every layer is covered by its own tests, but the seams between them are where
// the expensive mistakes live — a spec field the server sets and the worker
// reads under a different name, a sync token minted with one meaning and
// verified with another. Only a test that drives the actual HTTP API and lets a
// real worker run finds those.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/internal/compress"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/server"
	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/internal/worker"
	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

type stack struct {
	t *testing.T

	http     *httptest.Server
	store    *memory.Store
	blobs    blob.Store
	queue    *queue.Queue
	worker   *worker.Worker
	upstream string

	token       string
	workspaceID string

	// sync is the client's current token: exactly what a CLI would persist.
	sync protocol.SyncToken
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

func testPolicy(t *testing.T, upstream string) *policy.Policy {
	t.Helper()

	p, err := policy.Compile(policy.Spec{
		Version: "test-1",
		Users:   []policy.User{{ID: "dev", Email: "dev@example.com"}},
		Groups:  []policy.Group{{ID: "devs", Members: []policy.UserID{"dev"}}},
		Repositories: []policy.Repository{
			{ID: "backend-api", Remote: upstream, Forge: "generic", DefaultBranch: "main"},
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
					ID:          "secrets-are-off-limits",
					Subject:     policy.RuleSubject{Type: policy.SubjectTypeAny},
					Paths:       []policy.Pattern{policy.MustParsePattern("secrets/")},
					Actions:     policy.AllActions,
					Effect:      policy.EffectDeny,
					Description: "ask the platform team",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	return p
}

func newStack(t *testing.T) *stack {
	t.Helper()
	requireGit(t)

	ctx := context.Background()
	s := &stack{t: t, store: memory.New()}

	// A bare repository standing in for the forge.
	seed := t.TempDir()

	git(t, seed, "init", "--initial-branch=main", ".")
	git(t, seed, "config", "user.email", "test@example.com")
	git(t, seed, "config", "user.name", "Test")

	write(t, seed, "src/app.go", "package main\n\nfunc main() {}\n")
	write(t, seed, "docs/readme.md", "hello\n")
	write(t, seed, "secrets/prod.env", "TOKEN=abc\n")

	git(t, seed, "add", "--all")
	git(t, seed, "commit", "-m", "init")

	s.upstream = filepath.Join(t.TempDir(), "upstream.git")
	git(t, seed, "clone", "--bare", ".", s.upstream)

	blobs, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	s.blobs = blobs

	compiled := testPolicy(t, s.upstream)
	source := policyloader.NewStatic(compiled)

	signer, err := newSigner([]byte(strings.Repeat("k", synctoken.MinKeyBytes)))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	s.queue = queue.New(s.store.Tasks(), queue.Options{})

	authService := auth.NewService(s.store, policy.OneSource{Source: source}, policy.DefaultTenant, nil)

	srv, err := server.New(server.Config{
		Tenant:            policy.DefaultTenant,
		EventPollInterval: 5 * time.Millisecond,
		EventMaxWait:      5 * time.Second,
	}, server.Deps{
		Store:      s.store,
		Queue:      s.queue,
		Blobs:      s.blobs,
		Policy:     policy.OneSource{Source: source},
		Auth:       authService,
		SyncTokens: signer,
		Log:        quiet,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	s.http = httptest.NewServer(srv.Handler())
	t.Cleanup(s.http.Close)

	s.worker, err = worker.New(worker.Config{
		WorkDir: t.TempDir(),
		Tenant:  policy.DefaultTenant,
	}, worker.Deps{
		Store:      s.store,
		Blobs:      s.blobs,
		Git:        gitx.NewExecGit(),
		Policy:     policy.OneSource{Source: source},
		SyncTokens: signer,
		Log:        quiet,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	// Seed the account and its credential, as an operator would.
	user, err := s.store.Users().Upsert(ctx, &store.User{
		TenantID:     policy.DefaultTenant,
		PolicyUserID: "dev",
		Email:        "dev@example.com",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := s.store.Repositories().Reconcile(ctx, policy.DefaultTenant, []*store.Repository{{
		TenantID:      policy.DefaultTenant,
		PolicyRepoID:  "backend-api",
		Remote:        s.upstream,
		Forge:         "generic",
		DefaultBranch: "main",
	}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	s.token, _, err = authService.Issue(ctx, user.ID, "laptop", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	s.workspaceID = s.createWorkspace()

	return s
}

// do performs an authenticated request.
func (s *stack) do(method, path string, body any) *http.Response {
	s.t.Helper()

	var reader io.Reader

	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			s.t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, s.http.URL+path, reader)
	if err != nil {
		s.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.http.Client().Do(req)
	if err != nil {
		s.t.Fatalf("Do: %v", err)
	}

	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return out
}

func (s *stack) createWorkspace() string {
	s.t.Helper()

	resp := s.do(http.MethodPost, protocol.RouteWorkspaces, server.CreateWorkspaceRequest{Label: "laptop"})
	if resp.StatusCode != http.StatusCreated {
		s.t.Fatalf("create workspace: %d", resp.StatusCode)
	}

	return decode[server.WorkspaceView](s.t, resp).ID
}

// runQueue drains the queue by executing every claimable task, as a worker
// process would.
func (s *stack) runQueue() {
	s.t.Helper()

	ctx := context.Background()

	for range 10 {
		handle, err := s.queue.Claim(ctx, "worker-1")
		if err != nil {
			return
		}

		result, err := s.worker.Handle(handle.Context(), handle.Task)
		if err != nil {
			if _, failErr := handle.Fail(ctx, asProtocolError(err)); failErr != nil {
				s.t.Fatalf("Fail: %v", failErr)
			}
			continue
		}

		if err := handle.Complete(ctx, result); err != nil {
			s.t.Fatalf("Complete: %v", err)
		}
	}
}

func asProtocolError(err error) *protocol.Error {
	var perr *protocol.Error
	if errors.As(err, &perr) {
		return perr
	}
	return &protocol.Error{Code: "internal", Message: err.Error()}
}

// pull runs a full pull: request, drain the queue, fetch the patch, and adopt
// the new sync token — exactly the sequence the CLI performs.
func (s *stack) pull(requestID string) (protocol.PullResult, string) {
	s.t.Helper()

	resp := s.do(http.MethodPost, protocol.RoutePull, protocol.PullRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       requestID,
		Repository:      "backend-api",
		Branch:          "main",
		Workspace:       protocol.WorkspaceID(s.workspaceID),
		Sync:            s.sync,
	})
	if resp.StatusCode != http.StatusAccepted {
		s.t.Fatalf("pull: %d", resp.StatusCode)
	}

	taskID := decode[protocol.PullResponse](s.t, resp).TaskID

	s.runQueue()

	task := s.waitForTask(taskID)

	if task.State != protocol.TaskSucceeded {
		s.t.Fatalf("pull task %s: %v", task.State, task.Error)
	}
	if task.PullResult == nil {
		s.t.Fatal("no pull result")
	}

	body := ""
	if task.PullResult.Patch != nil {
		body = s.fetchPatch(taskID)
	}

	s.sync = task.PullResult.NextSync

	return *task.PullResult, body
}

// waitForTask polls the API until a task reaches a terminal state.
func (s *stack) waitForTask(id string) *protocol.Task {
	s.t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		resp := s.do(http.MethodGet, protocol.RouteTasks+"/"+id, nil)
		task := decode[protocol.Task](s.t, resp)

		if task.State.Terminal() {
			return &task
		}

		time.Sleep(5 * time.Millisecond)
	}

	s.t.Fatalf("task %s did not finish", id)
	return nil
}

// fetchPatch downloads and decompresses a task's patch.
func (s *stack) fetchPatch(taskID string) string {
	s.t.Helper()

	resp := s.do(http.MethodGet, protocol.TaskPatchPath(taskID), nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("fetch patch: %d", resp.StatusCode)
	}

	compressed, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("ReadAll: %v", err)
	}

	raw, err := compress.Decompress(compressed, protocol.EncodingZstd, 0)
	if err != nil {
		s.t.Fatalf("Decompress: %v", err)
	}

	return string(raw)
}

// push runs a full push: upload, submit, drain the queue.
func (s *stack) push(requestID, message, rawPatch string) *protocol.Task {
	s.t.Helper()

	compressed, err := compress.Compress([]byte(rawPatch), protocol.EncodingZstd)
	if err != nil {
		s.t.Fatalf("Compress: %v", err)
	}

	upload := s.do(http.MethodPost, protocol.RouteBlobs, compressed)
	if upload.StatusCode != http.StatusCreated {
		s.t.Fatalf("upload: %d", upload.StatusCode)
	}

	descriptor := decode[protocol.Blob](s.t, upload)
	descriptor.Encoding = protocol.EncodingZstd
	descriptor.UncompressedSize = int64(len(rawPatch))

	resp := s.do(http.MethodPost, protocol.RoutePush, protocol.PushRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       requestID,
		Repository:      "backend-api",
		Branch:          "main",
		Workspace:       protocol.WorkspaceID(s.workspaceID),
		BaseSync:        s.sync,
		Message:         message,
		Patch:           descriptor,
		Mode:            protocol.PushModeReject,
	})

	if resp.StatusCode != http.StatusAccepted {
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("push rejected with %d: %s", resp.StatusCode, body)
	}

	taskID := decode[protocol.PushResponse](s.t, resp).TaskID

	s.runQueue()

	return s.waitForTask(taskID)
}

// pushExpectingRejection submits a push that authorization must refuse.
func (s *stack) pushExpectingRejection(requestID, rawPatch string) protocol.Error {
	s.t.Helper()

	compressed, err := compress.Compress([]byte(rawPatch), protocol.EncodingZstd)
	if err != nil {
		s.t.Fatalf("Compress: %v", err)
	}

	upload := s.do(http.MethodPost, protocol.RouteBlobs, compressed)
	descriptor := decode[protocol.Blob](s.t, upload)
	descriptor.Encoding = protocol.EncodingZstd

	resp := s.do(http.MethodPost, protocol.RoutePush, protocol.PushRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       requestID,
		Repository:      "backend-api",
		Branch:          "main",
		Workspace:       protocol.WorkspaceID(s.workspaceID),
		BaseSync:        s.sync,
		Message:         "should not land",
		Patch:           descriptor,
		Mode:            protocol.PushModeReject,
	})

	if resp.StatusCode != http.StatusForbidden {
		s.t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	return decode[protocol.Error](s.t, resp)
}

func (s *stack) upstreamHead() string {
	s.t.Helper()
	return git(s.t, s.upstream, "rev-parse", "refs/heads/main")
}

func (s *stack) upstreamFile(path string) string {
	s.t.Helper()
	return git(s.t, s.upstream, "show", "refs/heads/main:"+path)
}

func (s *stack) commitUpstream(path, content, message string) {
	s.t.Helper()

	work := s.t.TempDir()

	git(s.t, work, "clone", "--branch", "main", s.upstream, "w")

	repo := filepath.Join(work, "w")

	write(s.t, repo, path, content)
	git(s.t, repo, "add", "--all")
	git(s.t, repo, "-c", "user.email=o@example.com", "-c", "user.name=Other", "commit", "-m", message)
	git(s.t, repo, "push", "origin", "main")
}

// ---------------------------------------------------------------------------

// The whole product in one test: clone a filtered projection, change it, push
// it back, and see the change on the forge with the confidential subtree never
// having left it.
func TestCloneChangePushCycle(t *testing.T) {
	s := newStack(t)

	// --- clone: a workspace with no sync point gets a filtered snapshot ---

	result, snapshot := s.pull("req-clone")

	if result.Patch == nil {
		t.Fatal("no snapshot delivered")
	}
	if s.sync == "" {
		t.Fatal("no sync token issued")
	}

	set, err := patch.Parse([]byte(snapshot))
	if err != nil {
		t.Fatalf("the snapshot does not parse: %v", err)
	}

	for _, p := range set.Paths() {
		if strings.HasPrefix(p, "secrets/") {
			t.Fatalf("the snapshot leaked %s", p)
		}
	}
	if result.Report.FilesWithheld != 1 {
		t.Errorf("FilesWithheld = %d, want 1 (secrets/prod.env)", result.Report.FilesWithheld)
	}

	// --- the developer applies the snapshot into a local workspace ---

	local := t.TempDir()

	git(t, local, "init", "--initial-branch=main", ".")
	git(t, local, "config", "user.email", "dev@example.com")
	git(t, local, "config", "user.name", "dev")

	if err := os.WriteFile(filepath.Join(local, "snapshot.patch"), []byte(snapshot), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	git(t, local, "apply", "snapshot.patch")

	if err := os.Remove(filepath.Join(local, "snapshot.patch")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// The confidential subtree is simply not there.
	if _, err := os.Stat(filepath.Join(local, "secrets")); !os.IsNotExist(err) {
		t.Error("the workspace contains the confidential subtree")
	}

	git(t, local, "add", "--all")
	git(t, local, "commit", "-m", "nit: sync backend-api@main")

	base := git(t, local, "rev-parse", "HEAD")

	// --- change and push ---

	write(t, local, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	git(t, local, "add", "--all")

	changes := git(t, local, "diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"--binary", "--full-index", "--find-renames", "--cached", base) + "\n"

	task := s.push("req-push", "add a print", changes)

	if task.State != protocol.TaskSucceeded {
		t.Fatalf("push task %s: %v", task.State, task.Error)
	}
	if task.PushResult == nil {
		t.Fatal("no push result")
	}

	if got := s.upstreamFile("src/app.go"); got != "package main\n\nfunc main() { println(1) }" {
		t.Errorf("upstream content = %q", got)
	}
	if task.PushResult.UpstreamCommit != s.upstreamHead() {
		t.Errorf("reported %s, upstream is at %s", task.PushResult.UpstreamCommit, s.upstreamHead())
	}

	// The confidential file is untouched upstream: it never reached the client
	// and could not have been changed by it.
	if got := s.upstreamFile("secrets/prod.env"); got != "TOKEN=abc" {
		t.Errorf("the confidential file changed: %q", got)
	}

	// Nothing else moved, so the workspace is still a faithful projection and
	// the client gets a token letting it push again without pulling.
	if task.PushResult.NextSync == "" {
		t.Error("no next sync token after an unimpeded push")
	}
}

// A confidential path in a push is refused end to end, and nothing reaches the
// forge.
func TestUnauthorizedPushNeverReachesTheForge(t *testing.T) {
	s := newStack(t)

	s.pull("req-clone")

	before := s.upstreamHead()

	body := s.pushExpectingRejection("req-push", section("secrets/prod.env")+section("src/app.go"))

	if body.Code != protocol.CodeUnauthorizedPaths {
		t.Errorf("code = %q", body.Code)
	}

	var named bool
	for _, d := range body.Denials {
		if d.Path == "secrets/prod.env" {
			named = true

			if d.Description != "ask the platform team" {
				t.Errorf("description = %q; a denial must tell the developer what to do", d.Description)
			}
		}
	}
	if !named {
		t.Errorf("denials do not name the offending path: %+v", body.Denials)
	}

	s.runQueue()

	if s.upstreamHead() != before {
		t.Error("a refused push changed the forge")
	}
}

// A second pull after someone else's commit delivers only what changed, and
// only what is readable.
func TestIncrementalPull(t *testing.T) {
	s := newStack(t)

	s.pull("req-clone")

	s.commitUpstream("docs/readme.md", "hello, world\n", "update the docs")
	s.commitUpstream("secrets/prod.env", "TOKEN=rotated\n", "rotate the secret")

	result, delivered := s.pull("req-pull-2")

	if result.Patch == nil {
		t.Fatal("no patch delivered")
	}
	if !strings.Contains(delivered, "diff --git a/docs/readme.md") {
		t.Error("the readable change was not delivered")
	}
	if strings.Contains(delivered, "diff --git a/secrets/prod.env") {
		t.Error("the confidential change was delivered")
	}
	if result.Report.FilesDelivered != 1 || result.Report.FilesWithheld != 1 {
		t.Errorf("report = %+v", result.Report)
	}
	if result.Report.UpstreamCommit != s.upstreamHead() {
		t.Errorf("UpstreamCommit = %q, want the tip", result.Report.UpstreamCommit)
	}
}

// A workspace pushing from a base the branch has moved past must be told to
// pull rather than have its patch applied somewhere it does not belong.
func TestStaleWorkspaceIsToldToPull(t *testing.T) {
	s := newStack(t)

	s.pull("req-clone")

	// Someone else pushes; the client's sync point is now behind.
	s.commitUpstream("docs/readme.md", "moved on\n", "concurrent change")

	// The stored sync point only advances when this workspace pulls, so a push
	// with the old token is still accepted here and reconciled by the worker's
	// rebase. Simulate the server-side record moving instead, which is what a
	// second workspace of the same developer would cause.
	resp := s.do(http.MethodPost, protocol.RoutePull, protocol.PullRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       "req-pull-2",
		Repository:      "backend-api",
		Branch:          "main",
		Workspace:       protocol.WorkspaceID(s.workspaceID),
		Sync:            s.sync,
	})
	resp.Body.Close()

	s.runQueue()

	// The client did not adopt the new token: it still holds the old one, which
	// is exactly the state after a failed apply.
	compressed, err := compress.Compress([]byte(section("src/app.go")), protocol.EncodingZstd)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}

	upload := s.do(http.MethodPost, protocol.RouteBlobs, compressed)
	descriptor := decode[protocol.Blob](t, upload)
	descriptor.Encoding = protocol.EncodingZstd

	push := s.do(http.MethodPost, protocol.RoutePush, protocol.PushRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       "req-push",
		Repository:      "backend-api",
		Branch:          "main",
		Workspace:       protocol.WorkspaceID(s.workspaceID),
		BaseSync:        s.sync,
		Message:         "stale push",
		Patch:           descriptor,
		Mode:            protocol.PushModeReject,
	})

	if push.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", push.StatusCode)
	}

	body := decode[protocol.Error](t, push)
	if body.Code != protocol.CodeStaleSyncPoint {
		t.Errorf("code = %q, want %q", body.Code, protocol.CodeStaleSyncPoint)
	}

	// And the client is not stuck: pulling again from the token it actually
	// holds gets it back in step.
	if _, _ = s.pull("req-pull-3"); s.sync == "" {
		t.Fatal("the client could not recover")
	}
}

// section builds a minimal modify section for a path.
func section(path string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"
}
