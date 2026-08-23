package worker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/taskspec"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// sharingPolicy has three subjects on purpose: two developers whose read rights
// are identical, and an auditor whose are not.
//
// The pair is what makes sharing possible; the odd one out is what makes the
// test worth writing.
func sharingPolicy(t *testing.T) *policy.Policy {
	t.Helper()

	p, err := policy.Compile(policy.Spec{
		Version: "sharing-1",
		Users: []policy.User{
			{ID: "dev", Email: "dev@example.com"},
			{ID: "dev2", Email: "dev2@example.com"},
			{ID: "auditor", Email: "auditor@example.com"},
		},
		Groups: []policy.Group{
			{ID: "devs", Members: []policy.UserID{"dev", "dev2"}},
			{ID: "auditors", Members: []policy.UserID{"auditor"}},
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
					// Reads docs only. Nothing under src/.
					ID:      "auditors-read-docs",
					Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "auditors"},
					Paths:   []policy.Pattern{policy.MustParsePattern("docs/")},
					Actions: []policy.Action{policy.ActionRead},
					Effect:  policy.EffectAllow,
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

// pullAs runs a pull for one policy user, creating the stored records it needs.
func (h *harness) pullAs(t *testing.T, policyUser policy.UserID, requestID, from string) protocol.PullResult {
	t.Helper()

	ctx := context.Background()

	user, err := h.store.Users().Upsert(ctx, &store.User{
		TenantID:     policy.DefaultTenant,
		PolicyUserID: policyUser,
		Email:        string(policyUser) + "@example.com",
	})
	if err != nil {
		t.Fatalf("Upsert %s: %v", policyUser, err)
	}

	workspace, err := h.store.Workspaces().Create(ctx, &store.Workspace{
		TenantID: policy.DefaultTenant,
		UserID:   user.ID,
		Label:    "laptop",
	})
	if err != nil {
		t.Fatalf("Create workspace for %s: %v", policyUser, err)
	}

	raw, err := h.worker.Handle(ctx, h.pullTask(taskspec.Pull{
		RequestID:     requestID,
		Repository:    "backend-api",
		Remote:        h.upstream,
		Forge:         "generic",
		Branch:        "main",
		FromCommit:    from,
		UserID:        user.ID,
		WorkspaceID:   workspace.ID,
		PolicyUserID:  policyUser,
		PolicyVersion: "sharing-1",
	}))
	if err != nil {
		t.Fatalf("Handle for %s: %v", policyUser, err)
	}

	var result protocol.PullResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	return result
}

// The failure this cache could cause, asserted directly: one person receiving
// another's files.
//
// Two developers in the same group must share a projection — that is the whole
// point — and an auditor with different read rights must never be served
// theirs. The auditor may read docs/ and not src/, so a leak would be visible
// as a src/ hunk in what the auditor receives.
func TestPullCacheNeverSharesAcrossRights(t *testing.T) {
	h := newHarnessWithPolicy(t, sharingPolicy(t))

	from := h.upstreamHead("main")

	h.commitUpstream("main", "src/app.go", "package main\n\nfunc updated() {}\n", "update src")
	h.commitUpstream("main", "docs/readme.md", "hello again\n", "update docs")
	h.commitUpstream("main", "secrets/prod.env", "TOKEN=rotated\n", "rotate the secret")

	first := h.pullAs(t, "dev", "req-dev", from)
	second := h.pullAs(t, "dev2", "req-dev2", from)
	audit := h.pullAs(t, "auditor", "req-auditor", from)

	// The pair shares. Identical rights, identical bytes, one computation.
	if first.Patch == nil || second.Patch == nil {
		t.Fatal("a developer received no patch")
	}
	if first.Patch.Digest != second.Patch.Digest {
		t.Errorf("two developers with identical rights got different projections:\n%s\n%s",
			first.Patch.Digest, second.Patch.Digest)
	}

	// Equal digests alone prove nothing: content addressing gives identical
	// bytes the same name whether they were computed once or twice. The audit
	// trail is what records which happened.
	if reused := h.reusedProjection(t, "req-dev"); reused {
		t.Error("the first pull reported a reused projection; there was nothing to reuse")
	}
	if reused := h.reusedProjection(t, "req-dev2"); !reused {
		t.Error("the second developer's pull recomputed the projection instead of reusing it")
	}
	if reused := h.reusedProjection(t, "req-auditor"); reused {
		t.Error("the auditor reused a projection, which can only be somebody else's")
	}

	// The auditor does not.
	if audit.Patch == nil {
		t.Fatal("the auditor received no patch, though docs/ changed")
	}
	if audit.Patch.Digest == first.Patch.Digest {
		t.Fatal("the auditor was served the developers' projection")
	}

	delivered := h.fetchPatch(audit.Patch)

	if strings.Contains(delivered, "src/app.go") {
		t.Error("the auditor received a file only developers may read")
	}
	if strings.Contains(delivered, "secrets/prod.env") {
		t.Error("the auditor received a file nobody may read")
	}
	if !strings.Contains(delivered, "docs/readme.md") {
		t.Error("the auditor did not receive the file they may read")
	}

	if audit.Report.FilesDelivered != 1 {
		t.Errorf("auditor FilesDelivered = %d, want 1", audit.Report.FilesDelivered)
	}
	if first.Report.FilesDelivered != 2 {
		t.Errorf("developer FilesDelivered = %d, want 2", first.Report.FilesDelivered)
	}
}

// A shared projection has to be the *same* projection, not merely one with the
// same digest: the counts travel with it and a client reads them.
func TestPullCacheReturnsTheWholeReport(t *testing.T) {
	h := newHarnessWithPolicy(t, sharingPolicy(t))

	from := h.upstreamHead("main")

	h.commitUpstream("main", "src/app.go", "package main\n\nfunc updated() {}\n", "update src")
	h.commitUpstream("main", "secrets/prod.env", "TOKEN=rotated\n", "rotate the secret")

	first := h.pullAs(t, "dev", "req-1", from)
	second := h.pullAs(t, "dev2", "req-2", from)

	if first.Report.FilesTotal != second.Report.FilesTotal ||
		first.Report.FilesDelivered != second.Report.FilesDelivered ||
		first.Report.FilesWithheld != second.Report.FilesWithheld {
		t.Errorf("a reused projection reported different counts: %+v vs %+v",
			first.Report, second.Report)
	}

	if second.Report.FilesWithheld != 1 {
		t.Errorf("FilesWithheld = %d, want the secret counted", second.Report.FilesWithheld)
	}

	// The count of withheld files survives the cache; the paths must not appear
	// anywhere, cached or not.
	if strings.Contains(second.Patch.Digest, "secrets") {
		t.Error("a withheld path leaked into the descriptor")
	}
}

// Different commit ranges are different projections, however identical the
// rights. Sharing across them would deliver a diff computed from somewhere the
// client has never been.
func TestPullCacheKeepsCommitRangesApart(t *testing.T) {
	h := newHarnessWithPolicy(t, sharingPolicy(t))

	base := h.upstreamHead("main")

	h.commitUpstream("main", "src/app.go", "package main\n\nfunc one() {}\n", "one")
	middle := h.upstreamHead("main")

	h.commitUpstream("main", "src/app.go", "package main\n\nfunc two() {}\n", "two")

	whole := h.pullAs(t, "dev", "req-whole", base)
	tail := h.pullAs(t, "dev2", "req-tail", middle)

	if whole.Patch == nil || tail.Patch == nil {
		t.Fatal("a pull produced no patch")
	}
	if whole.Patch.Digest == tail.Patch.Digest {
		t.Error("two different commit ranges produced the same projection")
	}
}

// reusedProjection reads back what the pull recorded about itself.
func (h *harness) reusedProjection(t *testing.T, requestID string) bool {
	t.Helper()

	records, err := h.store.Audit().Query(context.Background(), store.AuditQuery{
		Tenant:    policy.DefaultTenant,
		RequestID: requestID,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	for _, r := range records {
		if r.Action != "pull.delivered" {
			continue
		}

		var detail map[string]string
		if err := json.Unmarshal(r.Detail, &detail); err != nil {
			t.Fatalf("unmarshal detail: %v", err)
		}

		return detail["reused_projection"] == "true"
	}

	t.Fatalf("no pull.delivered record for request %q", requestID)

	return false
}
