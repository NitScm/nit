package workspace_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/workspace"
	"github.com/NitScm/nit/pkg/gitx"
)

func requireGit(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

func newWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	requireGit(t)

	ws, err := workspace.Init(context.Background(), gitx.NewExecGit(),
		filepath.Join(t.TempDir(), "checkout"), workspace.State{
			Server:     "https://nit.example.com",
			Repository: "backend-api",
			Branch:     "main",
			Workspace:  "ws-1",
		})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	return ws
}

func TestInitAndOpen(t *testing.T) {
	ws := newWorkspace(t)

	reopened, err := workspace.Open(context.Background(), gitx.NewExecGit(), ws.Root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if reopened.State != ws.State {
		t.Errorf("state did not survive the round trip: %+v vs %+v", reopened.State, ws.State)
	}
}

// Commands must work from a subdirectory, as git's do.
func TestOpenSearchesUpwards(t *testing.T) {
	ws := newWorkspace(t)

	nested := filepath.Join(ws.Root, "src", "deep")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	found, err := workspace.Open(context.Background(), gitx.NewExecGit(), nested)
	if err != nil {
		t.Fatalf("Open from a subdirectory: %v", err)
	}
	if found.Root != ws.Root {
		t.Errorf("Root = %q, want %q", found.Root, ws.Root)
	}
}

func TestOpenOutsideAWorkspace(t *testing.T) {
	requireGit(t)

	_, err := workspace.Open(context.Background(), gitx.NewExecGit(), t.TempDir())
	if !errors.Is(err, workspace.ErrNotAWorkspace) {
		t.Errorf("got %v, want ErrNotAWorkspace", err)
	}
}

// nit's own directory must never end up in a patch: the server treats .nit/ as
// a protected path and would refuse the push.
func TestInitIgnoresItsOwnDirectory(t *testing.T) {
	ws := newWorkspace(t)

	content, err := os.ReadFile(filepath.Join(ws.Root, ".nit", ".gitignore"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(string(content)) != "*" {
		t.Errorf(".nit/.gitignore = %q", content)
	}
}

// The trailers are what makes a workspace recoverable when its state file is
// lost.
func TestSyncMessageCarriesTrailers(t *testing.T) {
	ws := newWorkspace(t)

	message := ws.SyncMessage("9f2c1ab4e5d6", "sha256:abcd")

	for _, want := range []string{
		"nit: sync backend-api@main",
		"Nit-Upstream-Commit: 9f2c1ab4e5d6",
		"Nit-Policy-Version: sha256:abcd",
		"Nit-Workspace: ws-1",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the message lacks %q:\n%s", want, message)
		}
	}
}

func TestDiffRequiresASyncPoint(t *testing.T) {
	ws := newWorkspace(t)

	_, err := ws.Diff(context.Background())
	if err == nil || !strings.Contains(err.Error(), "never synchronized") {
		t.Errorf("error = %v; it must say the workspace has no sync point", err)
	}
}

func TestEnsureCleanDetectsChanges(t *testing.T) {
	ws := newWorkspace(t)
	ctx := context.Background()

	if err := ws.EnsureClean(ctx); err != nil {
		t.Fatalf("a fresh workspace is not clean: %v", err)
	}

	if err := os.WriteFile(filepath.Join(ws.Root, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := ws.EnsureClean(ctx)
	if !errors.Is(err, workspace.ErrDirty) {
		t.Errorf("got %v, want ErrDirty", err)
	}
	if !strings.Contains(err.Error(), "new.txt") {
		t.Errorf("the error does not name the offending file: %v", err)
	}

	dirty, err := ws.Dirty(ctx)
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if !dirty {
		t.Error("Dirty disagrees with EnsureClean")
	}
}

// ---------------------------------------------------------------------------

func TestCredentialsRoundTrip(t *testing.T) {
	t.Setenv("NIT_CONFIG_DIR", t.TempDir())

	creds, err := workspace.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	if _, err := creds.Token("https://nit.example.com"); !errors.Is(err, workspace.ErrNoCredentials) {
		t.Errorf("got %v, want ErrNoCredentials", err)
	}

	creds.Set("https://nit.example.com", "nit_secret")

	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := workspace.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	token, err := reloaded.Token("https://nit.example.com")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "nit_secret" {
		t.Errorf("token = %q", token)
	}

	reloaded.Remove("https://nit.example.com")

	if _, err := reloaded.Token("https://nit.example.com"); err == nil {
		t.Error("Remove did not forget the token")
	}
}

// A trailing slash in one command must not lose the token stored by another.
func TestCredentialsNormalizeServerURLs(t *testing.T) {
	t.Setenv("NIT_CONFIG_DIR", t.TempDir())

	creds, err := workspace.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	creds.Set("https://nit.example.com/", "nit_secret")

	if _, err := creds.Token("https://nit.example.com"); err != nil {
		t.Errorf("a trailing slash lost the token: %v", err)
	}
}

// The file holds bearer tokens. It must never be readable by anyone else, and
// never even briefly.
func TestCredentialsFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NIT_CONFIG_DIR", dir)

	creds, err := workspace.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	creds.Set("https://nit.example.com", "nit_secret")

	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, err := workspace.CredentialsPath()
	if err != nil {
		t.Fatalf("CredentialsPath: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600: the file holds bearer tokens", mode)
	}

	// No temporary file left behind holding the same secret under looser
	// permissions.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary credentials file was left behind: %s", e.Name())
		}
	}
}

// Several deployments on one machine must not overwrite each other's tokens.
func TestCredentialsHoldSeveralServers(t *testing.T) {
	t.Setenv("NIT_CONFIG_DIR", t.TempDir())

	creds, err := workspace.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	creds.Set("https://staging.example.com", "nit_staging")
	creds.Set("https://prod.example.com", "nit_prod")

	if err := creds.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := workspace.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	staging, _ := reloaded.Token("https://staging.example.com")
	prod, _ := reloaded.Token("https://prod.example.com")

	if staging != "nit_staging" || prod != "nit_prod" {
		t.Errorf("tokens crossed: staging=%q prod=%q", staging, prod)
	}
}
