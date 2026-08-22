package enforce

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/policy"
)

const repo policy.RepoID = "backend-api"

// buildPolicy compiles a bundle where:
//   - everyone reads and writes src/ and docs/
//   - only the platform group may touch secrets/ or anything marked admin
func buildPolicy(t *testing.T) *policy.Policy {
	t.Helper()

	p, err := policy.Compile(policy.Spec{
		Version: "test-1",
		Users: []policy.User{
			{ID: "dev"},
			{ID: "platform"},
		},
		Groups: []policy.Group{
			{ID: "devs", Members: []policy.UserID{"dev"}},
			{ID: "platform", Members: []policy.UserID{"platform"}},
		},
		Repositories: []policy.Repository{
			{ID: repo, DefaultBranch: "main"},
		},
		Rules: map[policy.RepoID][]policy.Rule{
			repo: {
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
					ID:      "platform-owns-everything",
					Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "platform"},
					Paths:   []policy.Pattern{policy.MustParsePattern("**")},
					Actions: policy.AllActions,
					Effect:  policy.EffectAllow,
				},
				{
					ID:          "secrets-are-off-limits",
					Subject:     policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "devs"},
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

func optionsFor(t *testing.T, p *policy.Policy, user policy.UserID) Options {
	t.Helper()

	subject, err := p.Subject(user)
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	return Options{
		Engine:  p,
		Repo:    repo,
		Ref:     "refs/heads/feature/x",
		Subject: subject,
		Guards:  DefaultGuards(),
	}
}

func parse(t *testing.T, raw string) *patch.Set {
	t.Helper()

	set, err := patch.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("patch.Parse: %v", err)
	}
	return set
}

// section builds a minimal but valid modify section for a path.
func section(path string) string {
	return "diff --git a/" + path + " b/" + path + "\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/" + path + "\n" +
		"+++ b/" + path + "\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"
}

func TestPushAllAuthorized(t *testing.T) {
	p := buildPolicy(t)
	set := parse(t, section("src/app.go")+section("docs/readme.md"))

	res, err := Push(set, optionsFor(t, p, "dev"), ModeReject)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if !res.OK() {
		t.Fatalf("push refused: %s", res.Explain())
	}
	if len(res.Dropped) != 0 {
		t.Errorf("dropped %v", res.DroppedPaths())
	}
	if !bytes.Equal(res.Patch, set.Raw()) {
		t.Error("an entirely authorized push must be passed through unchanged")
	}
	if res.PolicyVersion != "test-1" {
		t.Errorf("PolicyVersion = %q", res.PolicyVersion)
	}
}

// The headline behaviour: one unauthorized file fails the whole push, and
// nothing is applied.
func TestPushRejectsWholePatch(t *testing.T) {
	p := buildPolicy(t)
	set := parse(t, section("src/app.go")+section("secrets/prod.env"))

	res, err := Push(set, optionsFor(t, p, "dev"), ModeReject)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if !res.Rejected {
		t.Fatal("push should have been rejected")
	}
	if res.OK() {
		t.Error("OK() must be false for a rejected push")
	}
	if res.Patch != nil {
		t.Error("a rejected push must produce no patch at all")
	}

	explanation := res.Explain()
	if !strings.Contains(explanation, "secrets/prod.env") {
		t.Errorf("explanation does not name the offending path:\n%s", explanation)
	}
	if !strings.Contains(explanation, "ask the platform team") {
		t.Errorf("explanation does not carry the rule description:\n%s", explanation)
	}
}

// Every section is evaluated even after the first denial, so the author sees
// the full list in one round trip.
func TestPushReportsAllDenials(t *testing.T) {
	p := buildPolicy(t)
	set := parse(t, section("secrets/a.env")+section("src/app.go")+section("secrets/b.env"))

	res, err := Push(set, optionsFor(t, p, "dev"), ModeReject)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if len(res.Dropped) != 2 {
		t.Errorf("got %d denied sections, want 2", len(res.Dropped))
	}
	if len(res.Verdicts) != 3 {
		t.Errorf("got %d verdicts, want one per section", len(res.Verdicts))
	}
}

func TestPushStripMode(t *testing.T) {
	p := buildPolicy(t)
	set := parse(t, section("src/app.go")+section("secrets/prod.env"))

	res, err := Push(set, optionsFor(t, p, "dev"), ModeStrip)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if res.Rejected {
		t.Fatal("strip mode must not reject")
	}
	if !res.OK() {
		t.Fatal("strip mode should have produced a patch")
	}
	if bytes.Contains(res.Patch, []byte("diff --git a/secrets/prod.env")) {
		t.Error("stripped section survived")
	}
	if !bytes.Contains(res.Patch, []byte("diff --git a/src/app.go")) {
		t.Error("authorized section was lost")
	}
}

func TestPushStripEverythingYieldsNoPatch(t *testing.T) {
	p := buildPolicy(t)
	set := parse(t, section("secrets/prod.env"))

	res, err := Push(set, optionsFor(t, p, "dev"), ModeStrip)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if res.Patch != nil {
		t.Error("expected no patch at all")
	}
	if res.OK() {
		t.Error("OK() must be false when nothing survives")
	}
}

func TestPushDistinguishesActions(t *testing.T) {
	// A group allowed to read and write, but not to delete or create.
	spec := policy.Spec{
		Version: "test-2",
		Users:   []policy.User{{ID: "dev"}},
		Groups:  []policy.Group{{ID: "devs", Members: []policy.UserID{"dev"}}},
		Repositories: []policy.Repository{
			{ID: repo},
		},
		Rules: map[policy.RepoID][]policy.Rule{
			repo: {{
				ID:      "modify-only",
				Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "devs"},
				Paths:   []policy.Pattern{policy.MustParsePattern("src/")},
				Actions: []policy.Action{policy.ActionRead, policy.ActionWrite},
				Effect:  policy.EffectAllow,
			}},
		},
	}

	limited, err := policy.Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	opts := optionsFor(t, limited, "dev")

	t.Run("modify allowed", func(t *testing.T) {
		res, err := Push(parse(t, section("src/app.go")), opts, ModeReject)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if res.Rejected {
			t.Errorf("modify should be allowed: %s", res.Explain())
		}
	})

	t.Run("delete refused", func(t *testing.T) {
		raw := "diff --git a/src/app.go b/src/app.go\n" +
			"deleted file mode 100644\n" +
			"index 1111111..0000000\n" +
			"--- a/src/app.go\n" +
			"+++ /dev/null\n" +
			"@@ -1 +0,0 @@\n" +
			"-old\n"

		res, err := Push(parse(t, raw), opts, ModeReject)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if !res.Rejected {
			t.Error("delete should be refused when only read and write are granted")
		}
	})

	t.Run("create refused", func(t *testing.T) {
		raw := "diff --git a/src/new.go b/src/new.go\n" +
			"new file mode 100644\n" +
			"index 0000000..1111111\n" +
			"--- /dev/null\n" +
			"+++ b/src/new.go\n" +
			"@@ -0,0 +1 @@\n" +
			"+new\n"

		res, err := Push(parse(t, raw), opts, ModeReject)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if !res.Rejected {
			t.Error("create should be refused when only read and write are granted")
		}
	})
}

// A rename must hold on both sides: otherwise it becomes a way to move a file
// out of a protected subtree.
func TestPushRenameRequiresBothSides(t *testing.T) {
	p := buildPolicy(t)

	raw := "diff --git a/secrets/prod.env b/src/prod.env\n" +
		"similarity index 100%\n" +
		"rename from secrets/prod.env\n" +
		"rename to src/prod.env\n"

	res, err := Push(parse(t, raw), optionsFor(t, p, "dev"), ModeReject)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if !res.Rejected {
		t.Fatal("moving a file out of a denied subtree must be refused")
	}
}

// The guard that matters most: write access to a harmless path plus a CI
// workflow edit is read access to the entire repository.
func TestPushProtectedPathRequiresAdmin(t *testing.T) {
	// The default bundle grants devs nothing on .github/, so this fails on the ordinary
	// path rule too. Grant devs write there to isolate the guard.
	spec := policy.Spec{
		Version: "test-3",
		Users:   []policy.User{{ID: "dev"}},
		Groups:  []policy.Group{{ID: "devs", Members: []policy.UserID{"dev"}}},
		Repositories: []policy.Repository{
			{ID: repo},
		},
		Rules: map[policy.RepoID][]policy.Rule{
			repo: {{
				ID:      "devs-write-everything-but-not-admin",
				Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "devs"},
				Paths:   []policy.Pattern{policy.MustParsePattern("**")},
				Actions: []policy.Action{
					policy.ActionRead, policy.ActionWrite,
					policy.ActionCreate, policy.ActionDelete,
				},
				Effect: policy.EffectAllow,
			}},
		},
	}

	permissive, err := policy.Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	opts := optionsFor(t, permissive, "dev")

	res, err := Push(parse(t, section(".github/workflows/ci.yml")), opts, ModeReject)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if !res.Rejected {
		t.Fatal("editing a CI workflow must require admin")
	}

	var sawGuard bool
	for _, c := range res.Denials() {
		if c.Guard == GuardProtectedPath && c.Action == policy.ActionAdmin {
			sawGuard = true
		}
	}
	if !sawGuard {
		t.Errorf("expected a protected-path guard denial, got %v", res.Denials())
	}

	t.Run("without guards it would pass", func(t *testing.T) {
		noGuards := opts
		noGuards.Guards = Guards{}

		res, err := Push(parse(t, section(".github/workflows/ci.yml")), noGuards, ModeReject)
		if err != nil {
			t.Fatalf("Push: %v", err)
		}
		if res.Rejected {
			t.Error("without guards the path rule alone allows it — this is the hole guards close")
		}
	})
}

// Creating a symlink is an authorization decision, not a content edit: the link
// target may be a path the author cannot read.
func TestPushSymlinkRequiresAdmin(t *testing.T) {
	spec := policy.Spec{
		Version: "test-4",
		Users:   []policy.User{{ID: "dev"}},
		Groups:  []policy.Group{{ID: "devs", Members: []policy.UserID{"dev"}}},
		Repositories: []policy.Repository{
			{ID: repo},
		},
		Rules: map[policy.RepoID][]policy.Rule{
			repo: {{
				ID:      "devs-write-src",
				Subject: policy.RuleSubject{Type: policy.SubjectTypeGroup, ID: "devs"},
				Paths:   []policy.Pattern{policy.MustParsePattern("src/")},
				Actions: []policy.Action{
					policy.ActionRead, policy.ActionWrite,
					policy.ActionCreate, policy.ActionDelete,
				},
				Effect: policy.EffectAllow,
			}},
		},
	}

	p, err := policy.Compile(spec)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	raw := "diff --git a/src/leak.link b/src/leak.link\n" +
		"new file mode 120000\n" +
		"index 0000000..1111111\n" +
		"--- /dev/null\n" +
		"+++ b/src/leak.link\n" +
		"@@ -0,0 +1 @@\n" +
		"+../secrets/prod.env\n" +
		"\\ No newline at end of file\n"

	res, err := Push(parse(t, raw), optionsFor(t, p, "dev"), ModeReject)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if !res.Rejected {
		t.Fatal("creating a symlink must require admin")
	}

	var sawGuard bool
	for _, c := range res.Denials() {
		if c.Guard == GuardSymlink {
			sawGuard = true
		}
	}
	if !sawGuard {
		t.Errorf("expected a symlink guard denial, got %v", res.Denials())
	}
}

// A plain rename of a regular file must not trip the symlink guard: the section
// carries no mode line at all.
func TestPushPlainRenameIsNotASymlink(t *testing.T) {
	p := buildPolicy(t)

	raw := "diff --git a/src/old.go b/src/new.go\n" +
		"similarity index 100%\n" +
		"rename from src/old.go\n" +
		"rename to src/new.go\n"

	res, err := Push(parse(t, raw), optionsFor(t, p, "dev"), ModeReject)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if res.Rejected {
		t.Errorf("an ordinary rename inside an authorized subtree must pass: %s", res.Explain())
	}
}

func TestPullFiltersUnreadable(t *testing.T) {
	p := buildPolicy(t)
	set := parse(t, section("src/app.go")+section("secrets/prod.env"))

	res, err := Pull(set, optionsFor(t, p, "dev"))
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if res.Rejected {
		t.Error("a pull is never rejected, only filtered")
	}
	if len(res.Dropped) != 1 || res.Dropped[0].DisplayPath() != "secrets/prod.env" {
		t.Errorf("dropped = %v, want [secrets/prod.env]", res.DroppedPaths())
	}
	if bytes.Contains(res.Patch, []byte("diff --git a/secrets/prod.env")) {
		t.Error("withheld section survived the filter")
	}
	if !bytes.Contains(res.Patch, []byte("diff --git a/src/app.go")) {
		t.Error("readable section was lost")
	}
}

func TestPullDropsHalfReadableRename(t *testing.T) {
	p := buildPolicy(t)

	raw := "diff --git a/secrets/old.env b/src/new.env\n" +
		"similarity index 100%\n" +
		"rename from secrets/old.env\n" +
		"rename to src/new.env\n"

	res, err := Pull(parse(t, raw), optionsFor(t, p, "dev"))
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if len(res.Kept) != 0 {
		t.Error("a rename with an unreadable side must be dropped whole")
	}
}

func TestPushOnTheMixedFixture(t *testing.T) {
	p := buildPolicy(t)

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "patches", "mixed.patch"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	set, err := patch.Parse(raw)
	if err != nil {
		t.Fatalf("patch.Parse: %v", err)
	}

	res, err := Push(set, optionsFor(t, p, "dev"), ModeStrip)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	dropped := map[string]bool{}
	for _, path := range res.DroppedPaths() {
		dropped[path] = true
	}

	// secrets/prod.env is denied outright; src/leak.link is a symlink and needs
	// admin. Everything else in the fixture sits in src/ or docs/.
	for _, want := range []string{"secrets/prod.env", "src/leak.link"} {
		if !dropped[want] {
			t.Errorf("%s should have been dropped; dropped = %v", want, res.DroppedPaths())
		}
	}
	for _, want := range []string{"src/app.go", "docs/logo.png", "docs/my file.txt"} {
		if dropped[want] {
			t.Errorf("%s should have been kept", want)
		}
	}

	if _, err := patch.Parse(res.Patch); err != nil {
		t.Fatalf("filtered patch does not re-parse: %v", err)
	}
}

func TestOptionsValidation(t *testing.T) {
	p := buildPolicy(t)
	set := parse(t, section("src/app.go"))

	if _, err := Push(set, Options{Repo: repo, Subject: policy.Subject{UserID: "dev"}}, ModeReject); err == nil {
		t.Error("expected an error when no engine is supplied")
	}
	if _, err := Push(set, optionsFor(t, p, "dev"), Mode("nonsense")); err == nil {
		t.Error("expected an error for an unknown mode")
	}
}
