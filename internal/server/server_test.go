package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/blob"
	"github.com/NitScm/nit/internal/compress"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/server"
	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/internal/taskspec"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
)

var testNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

const baseCommit = "aaaa1111bbbb2222"

type fixture struct {
	t *testing.T

	store  *memory.Store
	blobs  blob.Store
	queue  *queue.Queue
	server *server.Server
	http   *httptest.Server
	signer *synctoken.Signer

	user      *store.User
	other     *store.User
	repo      *store.Repository
	workspace *store.Workspace

	token      string
	otherToken string
	adminToken string
}

// testPolicy grants devs src/ and docs/, and hides secrets/ from them.
func testPolicy(t *testing.T) *policy.Policy {
	t.Helper()

	p, err := policy.Compile(policy.Spec{
		Version: "test-1",
		Users: []policy.User{
			{ID: "dev", Email: "dev@example.com"},
			{ID: "other", Email: "other@example.com"},
			{ID: "ops", Email: "ops@example.com"},
		},
		Groups: []policy.Group{
			{ID: "devs", Members: []policy.UserID{"dev", "other"}},
			{ID: "platform", Members: []policy.UserID{"ops"}},
		},
		Repositories: []policy.Repository{
			{ID: "backend-api", Remote: "https://example.com/backend-api.git", Forge: "github", DefaultBranch: "main"},
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

// newAdminFixture is newFixture with the operations API enabled, so the tests
// that need an operator do not have to duplicate the whole setup.
func newAdminFixture(t *testing.T) *fixture {
	t.Helper()

	return newFixtureWith(t, []policy.GroupID{"platform"}, []string{"http://localhost:4200"})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	return newFixtureWith(t, nil, nil)
}

func newFixtureWith(t *testing.T, adminGroups []policy.GroupID, origins []string) *fixture {
	t.Helper()

	ctx := context.Background()
	f := &fixture{t: t, store: memory.New()}

	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	f.blobs = blobs

	compiled := testPolicy(t)
	source := policyloader.NewStatic(compiled)

	f.queue = queue.New(f.store.Tasks(), queue.Options{
		Clock: func() time.Time { return testNow },
	})

	f.signer, err = synctoken.NewSigner([]byte(strings.Repeat("k", synctoken.MinKeyBytes)))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	authService := auth.NewService(f.store, source, policy.DefaultTenant,
		func() time.Time { return testNow })

	f.server, err = server.New(server.Config{
		Tenant:            policy.DefaultTenant,
		EventPollInterval: time.Millisecond,
		EventMaxWait:      200 * time.Millisecond,
		AdminGroups:       adminGroups,
		AllowedOrigins:    origins,
	}, server.Deps{
		Store:      f.store,
		Queue:      f.queue,
		Blobs:      f.blobs,
		Policy:     source,
		Auth:       authService,
		SyncTokens: f.signer,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	f.http = httptest.NewServer(f.server.Handler())
	t.Cleanup(f.http.Close)

	// Seed the two accounts, the repository and one workspace.
	f.user = f.seedUser(ctx, "dev", "dev@example.com")
	f.other = f.seedUser(ctx, "other", "other@example.com")
	admin := f.seedUser(ctx, "ops", "ops@example.com")

	if err := f.store.Repositories().Reconcile(ctx, policy.DefaultTenant, []*store.Repository{{
		TenantID:      policy.DefaultTenant,
		PolicyRepoID:  "backend-api",
		Remote:        "https://example.com/backend-api.git",
		Forge:         "github",
		DefaultBranch: "main",
	}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	f.repo, err = f.store.Repositories().ByPolicyID(ctx, policy.DefaultTenant, "backend-api")
	if err != nil {
		t.Fatalf("ByPolicyID: %v", err)
	}

	f.workspace, err = f.store.Workspaces().Create(ctx, &store.Workspace{
		TenantID: policy.DefaultTenant,
		UserID:   f.user.ID,
		Label:    "laptop",
	})
	if err != nil {
		t.Fatalf("Create workspace: %v", err)
	}

	f.token = f.issueToken(ctx, authService, f.user.ID)
	f.otherToken = f.issueToken(ctx, authService, f.other.ID)
	f.adminToken = f.issueToken(ctx, authService, admin.ID)

	return f
}

func (f *fixture) seedUser(ctx context.Context, id policy.UserID, email string) *store.User {
	f.t.Helper()

	user, err := f.store.Users().Upsert(ctx, &store.User{
		TenantID:     policy.DefaultTenant,
		PolicyUserID: id,
		Email:        email,
	})
	if err != nil {
		f.t.Fatalf("Upsert %s: %v", id, err)
	}

	return user
}

func (f *fixture) issueToken(ctx context.Context, service *auth.Service, userID store.ID) string {
	f.t.Helper()

	token, _, err := service.Issue(ctx, userID, "test", time.Hour)
	if err != nil {
		f.t.Fatalf("Issue: %v", err)
	}

	return token
}

// seedSyncPoint records a sync point and returns the matching token.
func (f *fixture) seedSyncPoint(branch, commit string) protocol.SyncToken {
	f.t.Helper()

	ctx := context.Background()

	if err := f.store.SyncPoints().Put(ctx, &store.SyncPoint{
		TenantID:       policy.DefaultTenant,
		WorkspaceID:    f.workspace.ID,
		RepositoryID:   f.repo.ID,
		Branch:         branch,
		UpstreamCommit: commit,
		PolicyVersion:  "test-1",
	}); err != nil {
		f.t.Fatalf("Put sync point: %v", err)
	}

	token, err := f.signer.Sign(synctoken.Payload{
		Workspace:      f.workspace.ID,
		Repository:     f.repo.ID,
		Branch:         branch,
		UpstreamCommit: commit,
		PolicyVersion:  "test-1",
		IssuedAt:       testNow.Unix(),
	})
	if err != nil {
		f.t.Fatalf("Sign: %v", err)
	}

	return token
}

// do performs a request with the caller's token.
func (f *fixture) do(method, path, token string, body any) *http.Response {
	f.t.Helper()

	var reader io.Reader

	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			f.t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, f.http.URL+path, reader)
	if err != nil {
		f.t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := f.http.Client().Do(req)
	if err != nil {
		f.t.Fatalf("Do: %v", err)
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

// uploadPatch compresses and uploads a patch, returning its descriptor.
func (f *fixture) uploadPatch(raw string) protocol.Blob {
	f.t.Helper()

	compressed, err := compress.Compress([]byte(raw), protocol.EncodingZstd)
	if err != nil {
		f.t.Fatalf("Compress: %v", err)
	}

	resp := f.do(http.MethodPost, protocol.RouteBlobs, f.token, compressed)
	if resp.StatusCode != http.StatusCreated {
		f.t.Fatalf("upload returned %d", resp.StatusCode)
	}

	descriptor := decode[protocol.Blob](f.t, resp)
	descriptor.Encoding = protocol.EncodingZstd
	descriptor.UncompressedSize = int64(len(raw))

	return descriptor
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

func (f *fixture) pushRequest(requestID, branch string, sync protocol.SyncToken, patch protocol.Blob) protocol.PushRequest {
	return protocol.PushRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       requestID,
		Repository:      "backend-api",
		Branch:          branch,
		Workspace:       protocol.WorkspaceID(f.workspace.ID),
		BaseSync:        sync,
		Message:         "test change",
		Patch:           patch,
		Mode:            protocol.PushModeReject,
	}
}

// ---------------------------------------------------------------------------

func TestHealthzIsUnauthenticated(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodGet, protocol.RouteHealthz, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	health := decode[server.Health](t, resp)
	if health.PolicyVersion != "test-1" {
		t.Errorf("PolicyVersion = %q", health.PolicyVersion)
	}
}

func TestAuthenticationIsRequired(t *testing.T) {
	f := newFixture(t)

	for _, route := range []string{protocol.RouteWhoAmI, protocol.RouteRepos, protocol.RouteWorkspaces} {
		resp := f.do(http.MethodGet, route, "", nil)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a token returned %d, want 401", route, resp.StatusCode)
		}
	}
}

func TestWhoAmI(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodGet, protocol.RouteWhoAmI, f.token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	who := decode[server.WhoAmI](t, resp)

	if who.User != "dev" {
		t.Errorf("User = %q", who.User)
	}
	if len(who.Groups) != 1 || who.Groups[0] != "devs" {
		t.Errorf("Groups = %v, want [devs]", who.Groups)
	}
}

func TestPushAccepted(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))

	resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor))

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	push := decode[protocol.PushResponse](t, resp)

	if !push.Accepted || push.TaskID == "" {
		t.Fatalf("push not accepted: %+v", push)
	}
	if push.Report.FilesAccepted != 1 || push.Report.FilesDenied != 0 {
		t.Errorf("report = %+v", push.Report)
	}

	// The queued task must carry a spec the worker can execute without
	// re-deriving anything.
	task, err := f.store.Tasks().ByID(context.Background(), store.ID(push.TaskID))
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	var spec taskspec.Push
	if err := json.Unmarshal(task.Payload, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}

	if spec.BaseCommit != baseCommit {
		t.Errorf("BaseCommit = %q, want the sync point commit %q", spec.BaseCommit, baseCommit)
	}
	if spec.AuthorEmail != "dev@example.com" {
		t.Errorf("AuthorEmail = %q; the author must come from the session", spec.AuthorEmail)
	}
	if spec.PatchDigest == "" {
		t.Error("the spec references no patch")
	}
	if task.PartitionKey != "backend-api:main" {
		t.Errorf("PartitionKey = %q; pushes must serialize per branch", task.PartitionKey)
	}
}

// The headline behaviour: one unauthorized path fails the whole push, and
// nothing is queued.
func TestPushRejectedOnUnauthorizedPath(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go") + section("secrets/prod.env"))

	resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor))

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	body := decode[protocol.Error](t, resp)

	if body.Code != protocol.CodeUnauthorizedPaths {
		t.Errorf("code = %q", body.Code)
	}
	if len(body.Denials) == 0 {
		t.Fatal("no denials returned; the developer cannot act on this")
	}

	var named bool
	for _, d := range body.Denials {
		if d.Path == "secrets/prod.env" {
			named = true
			if d.Description == "" {
				t.Error("the denial carries no rule description")
			}
		}
	}
	if !named {
		t.Errorf("denials do not name the offending path: %+v", body.Denials)
	}

	tasks, err := f.store.Tasks().List(context.Background(), store.TaskFilter{Tenant: policy.DefaultTenant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("%d tasks queued; a rejected push must cost no work", len(tasks))
	}
}

func TestPushDryRunQueuesNothing(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))

	req := f.pushRequest("req-1", "main", sync, descriptor)
	req.DryRun = true

	resp := f.do(http.MethodPost, protocol.RoutePush, f.token, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	push := decode[protocol.PushResponse](t, resp)
	if push.Accepted {
		t.Error("a dry run must not be accepted")
	}
	if push.Report.FilesAccepted != 1 {
		t.Errorf("report = %+v", push.Report)
	}

	tasks, err := f.store.Tasks().List(context.Background(), store.TaskFilter{Tenant: policy.DefaultTenant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("a dry run queued %d tasks", len(tasks))
	}
}

// A retried submission must return its original task, not create a second one:
// otherwise a lost response becomes a duplicated upstream commit.
func TestPushIsIdempotent(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))
	req := f.pushRequest("req-1", "main", sync, descriptor)

	first := decode[protocol.PushResponse](t, f.do(http.MethodPost, protocol.RoutePush, f.token, req))
	second := decode[protocol.PushResponse](t, f.do(http.MethodPost, protocol.RoutePush, f.token, req))

	if first.TaskID != second.TaskID {
		t.Errorf("retry created a second task: %s then %s", first.TaskID, second.TaskID)
	}

	tasks, err := f.store.Tasks().List(context.Background(), store.TaskFilter{Tenant: policy.DefaultTenant})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tasks) != 1 {
		t.Errorf("%d tasks exist, want 1", len(tasks))
	}
}

func TestPushSyncPointChecks(t *testing.T) {
	t.Run("no sync point at all", func(t *testing.T) {
		f := newFixture(t)
		descriptor := f.uploadPatch(section("src/app.go"))

		resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
			f.pushRequest("req-1", "main", "", descriptor))

		body := decode[protocol.Error](t, resp)
		if body.Code != protocol.CodeUnknownSyncPoint {
			t.Errorf("code = %q, want %q", body.Code, protocol.CodeUnknownSyncPoint)
		}
	})

	t.Run("stale sync token", func(t *testing.T) {
		f := newFixture(t)
		stale := f.seedSyncPoint("main", "old-commit")

		// The workspace moved on since the token was issued.
		f.seedSyncPoint("main", "new-commit")

		descriptor := f.uploadPatch(section("src/app.go"))

		resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
			f.pushRequest("req-1", "main", stale, descriptor))

		body := decode[protocol.Error](t, resp)
		if body.Code != protocol.CodeStaleSyncPoint {
			t.Errorf("code = %q, want %q", body.Code, protocol.CodeStaleSyncPoint)
		}
	})

	// The attack the signature exists to stop: naming a base of one's choosing
	// so the server applies the patch there.
	t.Run("forged sync token", func(t *testing.T) {
		f := newFixture(t)
		f.seedSyncPoint("main", baseCommit)

		attacker, err := synctoken.NewSigner([]byte(strings.Repeat("x", synctoken.MinKeyBytes)))
		if err != nil {
			t.Fatalf("NewSigner: %v", err)
		}

		forged, err := attacker.Sign(synctoken.Payload{
			Workspace:      f.workspace.ID,
			Repository:     f.repo.ID,
			Branch:         "main",
			UpstreamCommit: baseCommit,
		})
		if err != nil {
			t.Fatalf("Sign: %v", err)
		}

		descriptor := f.uploadPatch(section("src/app.go"))

		resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
			f.pushRequest("req-1", "main", forged, descriptor))

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for a forged token", resp.StatusCode)
		}
	})

	// A token minted for one branch must not authorize a base on another.
	t.Run("token from another branch", func(t *testing.T) {
		f := newFixture(t)

		mainToken := f.seedSyncPoint("main", baseCommit)
		f.seedSyncPoint("release", "other-commit")

		descriptor := f.uploadPatch(section("src/app.go"))

		resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
			f.pushRequest("req-1", "release", mainToken, descriptor))

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 for a cross-branch token", resp.StatusCode)
		}
	})
}

// A workspace id names a sync point, which decides where a patch is applied.
// Accepting somebody else's would let a caller push onto another developer's
// base.
func TestPushRejectsAnotherUsersWorkspace(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))

	resp := f.do(http.MethodPost, protocol.RoutePush, f.otherToken,
		f.pushRequest("req-1", "main", sync, descriptor))

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPushRejectsUnknownRepository(t *testing.T) {
	f := newFixture(t)

	descriptor := f.uploadPatch(section("src/app.go"))
	req := f.pushRequest("req-1", "main", "", descriptor)
	req.Repository = "nope"

	resp := f.do(http.MethodPost, protocol.RoutePush, f.token, req)

	body := decode[protocol.Error](t, resp)
	if body.Code != protocol.CodeUnknownRepository {
		t.Errorf("code = %q", body.Code)
	}
}

func TestPushRejectsUnknownBlob(t *testing.T) {
	f := newFixture(t)

	f.seedSyncPoint("main", baseCommit)

	resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", f.seedSyncPoint("main", baseCommit), protocol.Blob{
			Digest:   blob.Digest([]byte("never uploaded")),
			Encoding: protocol.EncodingZstd,
		}))

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPullQueuesTask(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)

	resp := f.do(http.MethodPost, protocol.RoutePull, f.token, protocol.PullRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       "req-1",
		Repository:      "backend-api",
		Branch:          "main",
		Workspace:       protocol.WorkspaceID(f.workspace.ID),
		Sync:            sync,
	})

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	pull := decode[protocol.PullResponse](t, resp)

	task, err := f.store.Tasks().ByID(context.Background(), store.ID(pull.TaskID))
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	// Pulls are read-only and must not serialize against anything.
	if task.PartitionKey != "" {
		t.Errorf("PartitionKey = %q, want empty for a pull", task.PartitionKey)
	}

	var spec taskspec.Pull
	if err := json.Unmarshal(task.Payload, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.FromCommit != baseCommit {
		t.Errorf("FromCommit = %q, want %q", spec.FromCommit, baseCommit)
	}
}

// A workspace with no sync point asks for a full snapshot rather than failing:
// that is what "nit clone" does.
func TestPullWithoutSyncPointRequestsSnapshot(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodPost, protocol.RoutePull, f.token, protocol.PullRequest{
		ProtocolVersion: protocol.Version,
		RequestID:       "req-1",
		Repository:      "backend-api",
		Branch:          "main",
		Workspace:       protocol.WorkspaceID(f.workspace.ID),
	})

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	pull := decode[protocol.PullResponse](t, resp)

	task, err := f.store.Tasks().ByID(context.Background(), store.ID(pull.TaskID))
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}

	var spec taskspec.Pull
	if err := json.Unmarshal(task.Payload, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.FromCommit != "" {
		t.Errorf("FromCommit = %q, want empty for a full snapshot", spec.FromCommit)
	}
}

func TestTaskIsScopedToItsOwner(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))

	push := decode[protocol.PushResponse](t, f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor)))

	path := protocol.RouteTasks + "/" + push.TaskID

	if resp := f.do(http.MethodGet, path, f.token, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("owner got %d, want 200", resp.StatusCode)
	}

	// 404 rather than 403: a caller has no business learning which task ids
	// exist.
	if resp := f.do(http.MethodGet, path, f.otherToken, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("stranger got %d, want 404", resp.StatusCode)
	}
}

func TestTaskEventsReturnWhenStateChanges(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))

	push := decode[protocol.PushResponse](t, f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor)))

	// A worker takes the task while the client waits.
	go func() {
		time.Sleep(10 * time.Millisecond)

		handle, err := f.queue.Claim(context.Background(), "worker-1")
		if err != nil {
			return
		}
		_ = handle.Complete(context.Background(), []byte(`{"upstream_commit":"cafe"}`))
	}()

	resp := f.do(http.MethodGet, "/v1/tasks/"+push.TaskID+"/events", f.token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	event := decode[protocol.Event](t, resp)

	if event.State == protocol.TaskQueued {
		t.Fatal("the long poll returned before the state changed")
	}
	if event.State.Terminal() && event.Task == nil {
		t.Error("a terminal event must carry the full task so the client needs no follow-up")
	}
}

func TestTaskPatchRequiresACompletedTask(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))

	push := decode[protocol.PushResponse](t, f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor)))

	resp := f.do(http.MethodGet, "/v1/tasks/"+push.TaskID+"/patch", f.token, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 while the task is still queued", resp.StatusCode)
	}
}

func TestUploadRejectsDigestMismatch(t *testing.T) {
	f := newFixture(t)

	req, err := http.NewRequest(http.MethodPost, f.http.URL+protocol.RouteBlobs,
		strings.NewReader("actual content"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-Nit-Digest", blob.Digest([]byte("something else")))

	resp, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestWorkspaceLifecycle(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodPost, protocol.RouteWorkspaces, f.token,
		server.CreateWorkspaceRequest{Label: "desktop"})

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	created := decode[server.WorkspaceView](t, resp)
	if created.ID == "" {
		t.Fatal("no workspace id returned")
	}

	list := decode[[]server.WorkspaceView](t, f.do(http.MethodGet, protocol.RouteWorkspaces, f.token, nil))
	if len(list) != 2 {
		t.Errorf("got %d workspaces, want 2", len(list))
	}

	// Another user's workspaces are their own.
	otherList := decode[[]server.WorkspaceView](t, f.do(http.MethodGet, protocol.RouteWorkspaces, f.otherToken, nil))
	if len(otherList) != 0 {
		t.Errorf("stranger sees %d workspaces, want 0", len(otherList))
	}
}

func TestRepositoriesAreScopedToReadableOnes(t *testing.T) {
	f := newFixture(t)

	repos := decode[[]server.RepositoryView](t, f.do(http.MethodGet, protocol.RouteRepos, f.token, nil))

	if len(repos) != 1 || repos[0].ID != "backend-api" {
		t.Errorf("repos = %+v", repos)
	}
}

func TestAuditRecordsTheDecision(t *testing.T) {
	f := newFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("secrets/prod.env"))

	f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor)).Body.Close()

	records, err := f.store.Audit().Query(context.Background(), store.AuditQuery{
		Tenant:    policy.DefaultTenant,
		RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("got %d audit records, want a summary plus one per denied path", len(records))
	}

	var sawRejection, sawDeniedPath bool

	for _, r := range records {
		if r.Action == "push.rejected" {
			sawRejection = true
		}
		if r.Action == "push.denied_path" && r.Path == "secrets/prod.env" {
			sawDeniedPath = true

			if r.RuleID != "secrets-are-off-limits" {
				t.Errorf("RuleID = %q; the audit trail must attribute the decision", r.RuleID)
			}
			if r.PolicyVersion != "test-1" {
				t.Errorf("PolicyVersion = %q; a past decision must be replayable", r.PolicyVersion)
			}
		}
	}

	if !sawRejection || !sawDeniedPath {
		t.Error("the audit trail does not record both the rejection and the denied path")
	}
}

func TestUnsupportedProtocolVersion(t *testing.T) {
	f := newFixture(t)

	descriptor := f.uploadPatch(section("src/app.go"))
	req := f.pushRequest("req-1", "main", "", descriptor)
	req.ProtocolVersion = "99"

	resp := f.do(http.MethodPost, protocol.RoutePush, f.token, req)

	body := decode[protocol.Error](t, resp)
	if body.Code != protocol.CodeUnsupportedVersion {
		t.Errorf("code = %q", body.Code)
	}
}

// A client typo on a field that carries a push mode or a sync token must fail
// rather than be silently ignored.
func TestUnknownFieldsAreRejected(t *testing.T) {
	f := newFixture(t)

	resp := f.do(http.MethodPost, protocol.RoutePush, f.token,
		[]byte(`{"protocol_version":"1","request_id":"r","repository":"backend-api","typo":true}`))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
