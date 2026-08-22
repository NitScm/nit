package server_test

import (
	"net/http"
	"testing"

	"github.com/NitScm/nit/internal/server"
	"github.com/NitScm/nit/pkg/protocol"
)

// The operations API must be invisible to an ordinary developer: 404, not 403,
// so its existence is not confirmed to someone who cannot use it.
func TestAdminAPIIsHiddenFromNonAdmins(t *testing.T) {
	f := newFixture(t)

	for _, route := range []string{
		"/v1/admin/tasks", "/v1/admin/audit", "/v1/admin/stats", "/v1/admin/policy",
	} {
		resp := f.do(http.MethodGet, route, f.token, nil)
		resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s returned %d for a non-admin, want 404", route, resp.StatusCode)
		}
	}
}

func TestAdminTasksAndStats(t *testing.T) {
	f := newAdminFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("src/app.go"))

	push := decode[protocol.PushResponse](t, f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor)))

	// --- list ---

	tasks := decode[[]server.TaskView](t, f.do(http.MethodGet, "/v1/admin/tasks", f.adminToken, nil))

	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}

	task := tasks[0]

	if task.User != "dev" {
		t.Errorf("User = %q; an operator needs the owner, not an opaque id", task.User)
	}
	if task.Repository != "backend-api" {
		t.Errorf("Repository = %q", task.Repository)
	}
	if task.State != protocol.TaskQueued {
		t.Errorf("State = %q", task.State)
	}

	// --- filters ---

	filtered := decode[[]server.TaskView](t,
		f.do(http.MethodGet, "/v1/admin/tasks?state=succeeded", f.adminToken, nil))
	if len(filtered) != 0 {
		t.Errorf("the state filter returned %d tasks, want 0", len(filtered))
	}

	byRepo := decode[[]server.TaskView](t,
		f.do(http.MethodGet, "/v1/admin/tasks?repository=backend-api&kind=push", f.adminToken, nil))
	if len(byRepo) != 1 {
		t.Errorf("the repository filter returned %d tasks, want 1", len(byRepo))
	}

	// --- detail ---

	detail := decode[server.AdminTaskDetail](t,
		f.do(http.MethodGet, "/v1/admin/tasks/"+push.TaskID, f.adminToken, nil))

	if detail.ID != push.TaskID {
		t.Errorf("ID = %q", detail.ID)
	}
	if len(detail.Payload) == 0 {
		t.Error("no payload; an operator cannot see what the worker was told to do")
	}

	// --- stats ---

	stats := decode[server.Stats](t, f.do(http.MethodGet, "/v1/admin/stats", f.adminToken, nil))

	if stats.QueueDepth != 1 {
		t.Errorf("QueueDepth = %d, want 1", stats.QueueDepth)
	}
	if stats.Tasks["queued"] != 1 {
		t.Errorf("Tasks = %v", stats.Tasks)
	}
	if stats.PolicyVersion != "test-1" {
		t.Errorf("PolicyVersion = %q", stats.PolicyVersion)
	}
	if stats.Repositories != 1 {
		t.Errorf("Repositories = %d, want 1", stats.Repositories)
	}
}

// The audit endpoint is what the whole product exists to make possible.
func TestAdminAudit(t *testing.T) {
	f := newAdminFixture(t)

	sync := f.seedSyncPoint("main", baseCommit)
	descriptor := f.uploadPatch(section("secrets/prod.env"))

	f.do(http.MethodPost, protocol.RoutePush, f.token,
		f.pushRequest("req-1", "main", sync, descriptor)).Body.Close()

	records := decode[[]server.AuditView](t,
		f.do(http.MethodGet, "/v1/admin/audit?request_id=req-1", f.adminToken, nil))

	if len(records) < 2 {
		t.Fatalf("got %d records, want a summary plus one per denied path", len(records))
	}

	var denial *server.AuditView

	for i := range records {
		if records[i].Action == "push.denied_path" {
			denial = &records[i]
		}
	}

	if denial == nil {
		t.Fatal("the denied path was not recorded")
	}
	if denial.Path != "secrets/prod.env" {
		t.Errorf("Path = %q", denial.Path)
	}
	if denial.RuleID != "secrets-are-off-limits" {
		t.Errorf("RuleID = %q; the audit trail must attribute the decision", denial.RuleID)
	}
	if denial.Actor != "dev" {
		t.Errorf("Actor = %q", denial.Actor)
	}
	if denial.Repository != "backend-api" {
		t.Errorf("Repository = %q", denial.Repository)
	}

	// Filters narrow it down the way an investigation does.
	byUser := decode[[]server.AuditView](t,
		f.do(http.MethodGet, "/v1/admin/audit?user=dev&limit=10", f.adminToken, nil))
	if len(byUser) == 0 {
		t.Error("the user filter returned nothing")
	}

	resp := f.do(http.MethodGet, "/v1/admin/audit?since=not-a-timestamp", f.adminToken, nil)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed timestamp returned %d, want 400", resp.StatusCode)
	}
}

// The console shows the rules so nobody has to read YAML on a server.
func TestAdminPolicy(t *testing.T) {
	f := newAdminFixture(t)

	view := decode[server.PolicyView](t, f.do(http.MethodGet, "/v1/admin/policy", f.adminToken, nil))

	if view.Version != "test-1" {
		t.Errorf("Version = %q", view.Version)
	}
	if len(view.Repositories) != 1 {
		t.Fatalf("got %d repositories, want 1", len(view.Repositories))
	}

	repo := view.Repositories[0]

	if repo.ID != "backend-api" {
		t.Errorf("ID = %q", repo.ID)
	}
	if len(repo.Rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(repo.Rules))
	}

	var deny *server.PolicyRuleView

	for i := range repo.Rules {
		if repo.Rules[i].Effect == "deny" {
			deny = &repo.Rules[i]
		}
	}

	if deny == nil {
		t.Fatal("the deny rule is missing")
	}
	if deny.Subject != "any" {
		t.Errorf("Subject = %q", deny.Subject)
	}
	if len(deny.Paths) != 1 || deny.Paths[0] != "secrets/" {
		t.Errorf("Paths = %v", deny.Paths)
	}
	if deny.Description == "" {
		t.Error("the rule description is missing; it is what a denial shows the developer")
	}
}

// A browser front end on its own origin needs CORS; nothing else does, and
// there is no wildcard.
func TestCORS(t *testing.T) {
	f := newAdminFixture(t)

	req, err := http.NewRequest(http.MethodOptions, f.http.URL+"/v1/admin/stats", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Origin", "http://localhost:4200")

	resp, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:4200" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if !contains(resp.Header.Values("Vary"), "Origin") {
		t.Error("Vary: Origin is missing; a shared cache could serve one origin's headers to another")
	}

	// An origin that is not listed gets no permission at all.
	req.Header.Set("Origin", "https://evil.example.com")

	resp2, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp2.Body.Close()

	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin was allowed: %q", got)
	}
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
