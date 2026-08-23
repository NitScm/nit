package bootstrap_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/bootstrap"
)

// isolate points the search path at an empty directory, so a test never picks
// up a real /etc/nit/nit.yaml on the machine running it.
func isolate(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	t.Setenv("NIT_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Chdir(dir)

	for _, name := range []string{
		bootstrap.EnvSyncKey, bootstrap.EnvDatabaseURL, bootstrap.EnvPolicyDir,
		bootstrap.EnvBlobDir, bootstrap.EnvWorkDir, bootstrap.EnvAddr,
		bootstrap.EnvLogLevel, bootstrap.EnvAdminGroups, bootstrap.EnvCORSOrigins,
		bootstrap.EnvForgeToken, bootstrap.EnvLeaseDuration, bootstrap.EnvMaxAttempts,
		bootstrap.EnvQueuePoll, bootstrap.EnvReapEvery, bootstrap.EnvPullTTL,
		bootstrap.EnvEventMaxWait, bootstrap.EnvPolicyReload, bootstrap.EnvMaxPatch,
		bootstrap.EnvSyncKeyFile, bootstrap.EnvDatabaseURLFile, bootstrap.EnvForgeTokenFile,
		bootstrap.EnvGitSSHCmd, bootstrap.EnvMirrorBudget,
	} {
		t.Setenv(name, "")
	}

	return dir
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write: %v", err)
	}
}

const key = "0123456789abcdef0123456789abcdef"

func TestDefaultsWithoutAFile(t *testing.T) {
	isolate(t)
	t.Setenv(bootstrap.EnvSyncKey, key)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ConfigFile != "" {
		t.Errorf("ConfigFile = %q, want none found", cfg.ConfigFile)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.LeaseDuration != 60*time.Second {
		t.Errorf("LeaseDuration = %s", cfg.LeaseDuration)
	}
	if cfg.Origin("addr") != bootstrap.OriginDefault {
		t.Errorf("origin of addr = %q, want default", cfg.Origin("addr"))
	}
}

func TestFileIsFoundAndApplied(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), `
server:
  addr: "127.0.0.1:9000"
  admin_groups: [platform, sre]
queue:
  lease_duration: 5m
  max_attempts: 7
security:
  sync_key: "`+key+`"
log:
  level: debug
`, 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ConfigFile == "" {
		t.Fatal("the file in the working directory was not found")
	}
	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.LeaseDuration != 5*time.Minute {
		t.Errorf("LeaseDuration = %s", cfg.LeaseDuration)
	}
	if cfg.MaxAttempts != 7 {
		t.Errorf("MaxAttempts = %d", cfg.MaxAttempts)
	}
	if len(cfg.AdminGroups) != 2 {
		t.Errorf("AdminGroups = %v", cfg.AdminGroups)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %s", cfg.LogLevel)
	}
	if cfg.Origin("addr") != bootstrap.OriginFile {
		t.Errorf("origin of addr = %q, want file", cfg.Origin("addr"))
	}
}

// The environment wins over the file: that is what a container orchestrator
// injects, and what an operator expects when debugging a host by hand.
func TestEnvironmentOverridesFile(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), `
server:
  addr: "from-file:9000"
queue:
  lease_duration: 5m
security:
  sync_key: "`+key+`"
`, 0o600)

	t.Setenv(bootstrap.EnvAddr, "from-env:9999")
	t.Setenv(bootstrap.EnvLeaseDuration, "90s")

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Addr != "from-env:9999" {
		t.Errorf("Addr = %q, want the environment to win", cfg.Addr)
	}
	if cfg.LeaseDuration != 90*time.Second {
		t.Errorf("LeaseDuration = %s", cfg.LeaseDuration)
	}
	if cfg.Origin("addr") != bootstrap.OriginEnv {
		t.Errorf("origin of addr = %q, want env", cfg.Origin("addr"))
	}
	// A setting the environment did not touch keeps its file provenance.
	if cfg.Origin("security.sync_key") != bootstrap.OriginFile {
		t.Errorf("origin of sync_key = %q, want file", cfg.Origin("security.sync_key"))
	}
}

// Secrets delivered as files is how every secret manager works, and it keeps
// the configuration file itself unprivileged.
func TestSecretsFromFiles(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "sync.key"), key+"\n", 0o600)
	write(t, filepath.Join(dir, "forge.token"), "ghp_secret\n", 0o600)
	write(t, filepath.Join(dir, "db.url"), "postgres://nit@db/nit\n", 0o600)

	write(t, filepath.Join(dir, "nit.yaml"), `
database:
  url_file: `+filepath.Join(dir, "db.url")+`
security:
  sync_key_file: `+filepath.Join(dir, "sync.key")+`
forge:
  token_file: `+filepath.Join(dir, "forge.token")+`
`, 0o644)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Trailing newlines are what every editor and secret mount adds; a key with
	// a stray "\n" would differ between replicas depending on how each was
	// written.
	if string(cfg.SyncKey) != key {
		t.Errorf("SyncKey = %q, want the trailing newline trimmed", cfg.SyncKey)
	}
	if cfg.ForgeToken != "ghp_secret" {
		t.Errorf("ForgeToken = %q", cfg.ForgeToken)
	}
	if cfg.DatabaseURL != "postgres://nit@db/nit" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

// A file holding secrets inline must not be readable by anyone else. Refusing
// is louder than warning, and a warning in a start-up log is one nobody sees.
func TestReadableFileWithInlineSecretsIsRefused(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), "security:\n  sync_key: \""+key+"\"\n", 0o644)

	_, err := bootstrap.LoadConfig()
	if err == nil {
		t.Fatal("a world-readable file with an inline secret was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error = %v; it must say how to fix it", err)
	}
}

// The same file without inline secrets is fine at any mode: it names paths, not
// credentials.
func TestReadableFileWithoutSecretsIsFine(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "sync.key"), key, 0o600)
	write(t, filepath.Join(dir, "nit.yaml"),
		"security:\n  sync_key_file: "+filepath.Join(dir, "sync.key")+"\n", 0o644)

	if _, err := bootstrap.LoadConfig(); err != nil {
		t.Errorf("LoadConfig: %v", err)
	}
}

func TestMalformedSettingsAreRefused(t *testing.T) {
	cases := map[string]struct {
		yaml string
		env  [2]string
		want string
	}{
		"unknown key": {
			yaml: "server:\n  adr: \":8080\"\n",
			want: "field adr not found",
		},
		"bad duration in the file": {
			yaml: "queue:\n  lease_duration: soon\n",
			want: "queue.lease_duration",
		},
		"bad duration in the environment": {
			env:  [2]string{bootstrap.EnvLeaseDuration, "soon"},
			want: bootstrap.EnvLeaseDuration,
		},
		"bad log level": {
			yaml: "log:\n  level: chatty\n",
			want: "expected debug, info, warn or error",
		},
		"short key": {
			env:  [2]string{bootstrap.EnvSyncKey, "tooshort"},
			want: "at least 32",
		},
		"both inline and file secret": {
			yaml: "security:\n  sync_key: \"" + key + "\"\n  sync_key_file: /nowhere\n",
			want: "use one",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := isolate(t)
			t.Setenv(bootstrap.EnvSyncKey, key)

			if tc.yaml != "" {
				write(t, filepath.Join(dir, "nit.yaml"), tc.yaml, 0o600)
			}
			if tc.env[0] != "" {
				t.Setenv(tc.env[0], tc.env[1])
			}

			_, err := bootstrap.LoadConfig()
			if err == nil {
				t.Fatal("a malformed setting was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A file named explicitly and missing is an error: the operator meant that
// file, and silently searching elsewhere would run settings they did not
// choose.
func TestExplicitMissingFileIsAnError(t *testing.T) {
	isolate(t)
	t.Setenv(bootstrap.EnvSyncKey, key)

	if _, err := bootstrap.LoadConfigFrom("/nowhere/nit.yaml"); err == nil {
		t.Error("a missing explicit configuration file was accepted")
	}
}

func TestNoSyncKeyIsRefused(t *testing.T) {
	isolate(t)

	_, err := bootstrap.LoadConfig()
	if err == nil {
		t.Fatal("a configuration without a signing key was accepted")
	}
	if !strings.Contains(err.Error(), "openssl rand") {
		t.Errorf("error = %v; it must say how to generate one", err)
	}
}

// `nitctl config show` must never print a secret, and must stay useful about
// where each value came from.
func TestRedactedHidesSecrets(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), `
database:
  url: "postgres://nit:hunter2@db:5432/nit"
security:
  sync_key: "`+key+`"
forge:
  token: "ghp_supersecret"
`, 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	rendered := strings.Join(cfg.Redacted(), "\n")

	for _, secret := range []string{key, "ghp_supersecret", "hunter2"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("a secret was printed: %q", secret)
		}
	}

	if !strings.Contains(rendered, "(set)") {
		t.Error("a set secret must still be reported as present")
	}
	// The DSN stays readable enough to identify the host and user.
	if !strings.Contains(rendered, "postgres://nit:***@db:5432/nit") {
		t.Errorf("the database URL was not redacted usefully:\n%s", rendered)
	}
}

func TestExampleFileCompiles(t *testing.T) {
	dir := isolate(t)

	// The file "nitctl config init" writes must itself be valid, or the first
	// thing an operator does produces an error.
	write(t, filepath.Join(dir, "sync.key"), key, 0o600)

	content := strings.ReplaceAll(bootstrap.ExampleFile,
		"/etc/nit/sync.key", filepath.Join(dir, "sync.key"))
	content = strings.ReplaceAll(content,
		"token_file: /run/secrets/nit-forge-token", "# token_file: unused")

	write(t, filepath.Join(dir, "nit.yaml"), content, 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("the example configuration file does not load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.LeaseDuration != 60*time.Second {
		t.Errorf("LeaseDuration = %s", cfg.LeaseDuration)
	}
}

// A container runtime delivers secrets as files, not as environment values.
// Without the `_FILE` forms a deployment would have to interpolate them into
// the environment, where `docker inspect` and every crash dump can read them.
func TestSecretsFromFilesInTheEnvironment(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "sync.key"), key+"\n", 0o600)
	write(t, filepath.Join(dir, "forge.token"), "ghp_secret\n", 0o600)
	write(t, filepath.Join(dir, "db.url"), "postgres://nit@db/nit\n", 0o600)

	t.Setenv(bootstrap.EnvSyncKeyFile, filepath.Join(dir, "sync.key"))
	t.Setenv(bootstrap.EnvForgeTokenFile, filepath.Join(dir, "forge.token"))
	t.Setenv(bootstrap.EnvDatabaseURLFile, filepath.Join(dir, "db.url"))

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if string(cfg.SyncKey) != key {
		t.Errorf("SyncKey = %q, want the trailing newline trimmed", cfg.SyncKey)
	}
	if cfg.ForgeToken != "ghp_secret" {
		t.Errorf("ForgeToken = %q", cfg.ForgeToken)
	}
	if cfg.DatabaseURL != "postgres://nit@db/nit" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

// A deployment that mounts a secret meant it, so the file wins over an inline
// value left over from an earlier configuration.
func TestSecretFileWinsOverInlineEnvironment(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "sync.key"), key, 0o600)

	t.Setenv(bootstrap.EnvSyncKey, "0000000000000000000000000000000000")
	t.Setenv(bootstrap.EnvSyncKeyFile, filepath.Join(dir, "sync.key"))

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if string(cfg.SyncKey) != key {
		t.Errorf("the inline value won over the mounted file")
	}
}

// A named secret file that is missing must stop the process: falling back to no
// key would mean a server that cannot mint a sync token starting anyway.
func TestMissingSecretFileIsRefused(t *testing.T) {
	isolate(t)
	t.Setenv(bootstrap.EnvSyncKeyFile, "/nowhere/sync.key")

	if _, err := bootstrap.LoadConfig(); err == nil {
		t.Error("a missing secret file was accepted")
	}
}

// git.ssh_command is a passthrough, so the value has to survive verbatim: an
// ssh command line is quoting-sensitive and a helpfully trimmed or re-split
// value would authenticate as the wrong key, or not at all.
func TestGitSSHCommandFromFile(t *testing.T) {
	dir := isolate(t)

	command := "ssh -i /run/secrets/nit-ssh-key -o IdentitiesOnly=yes -o BatchMode=yes"

	write(t, filepath.Join(dir, "nit.yaml"), `
security:
  sync_key: "`+key+`"
git:
  ssh_command: "`+command+`"
`, 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.GitSSHCommand != command {
		t.Errorf("GitSSHCommand = %q, want %q", cfg.GitSSHCommand, command)
	}
	if cfg.Origin("git.ssh_command") != bootstrap.OriginFile {
		t.Errorf("origin = %q, want file", cfg.Origin("git.ssh_command"))
	}
}

func TestGitSSHCommandFromEnvironmentWins(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), `
security:
  sync_key: "`+key+`"
git:
  ssh_command: "ssh -i /from/file"
`, 0o600)

	t.Setenv(bootstrap.EnvGitSSHCmd, "ssh -i /from/env")

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.GitSSHCommand != "ssh -i /from/env" {
		t.Errorf("GitSSHCommand = %q, want the environment to win", cfg.GitSSHCommand)
	}
	if cfg.Origin("git.ssh_command") != bootstrap.OriginEnv {
		t.Errorf("origin = %q, want env", cfg.Origin("git.ssh_command"))
	}
}

// Unset means the process's inherited environment stands, so a host already
// configured through ~/.ssh/config keeps working.
func TestGitSSHCommandDefaultsToEmpty(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), "security:\n  sync_key: \""+key+"\"\n", 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.GitSSHCommand != "" {
		t.Errorf("GitSSHCommand = %q, want empty", cfg.GitSSHCommand)
	}
}

// It is a command line with paths, not secret material, and being able to read
// it back is the whole reason for preferring it to an environment variable set
// three layers away.
func TestGitSSHCommandIsShownInFull(t *testing.T) {
	dir := isolate(t)

	command := "ssh -i /run/secrets/nit-ssh-key -o IdentitiesOnly=yes"

	write(t, filepath.Join(dir, "nit.yaml"), `
security:
  sync_key: "`+key+`"
git:
  ssh_command: "`+command+`"
`, 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	var line string
	for _, row := range cfg.Redacted() {
		if strings.HasPrefix(row, "git.ssh_command") {
			line = row
		}
	}

	if line == "" {
		t.Fatal("git.ssh_command is missing from config show")
	}
	if !strings.Contains(line, command) {
		t.Errorf("config show line = %q, want the command in full", line)
	}
}

// Zero is how eviction is turned off, so it has to survive the load. An int64
// field would read an explicit 0 as an absent key and silently restore the
// default — leaving a deployment that asked for no eviction with 20 GiB of it.
func TestZeroMirrorBudgetIsKept(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), `
security:
  sync_key: "`+key+`"
storage:
  mirror_budget_bytes: 0
`, 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.MirrorBudgetBytes != 0 {
		t.Errorf("MirrorBudgetBytes = %d, want the 0 the file asked for", cfg.MirrorBudgetBytes)
	}
	if cfg.Origin("storage.mirror_budget_bytes") != bootstrap.OriginFile {
		t.Errorf("origin = %q, want file", cfg.Origin("storage.mirror_budget_bytes"))
	}
}

func TestMirrorBudgetFromEnvironmentWins(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), `
security:
  sync_key: "`+key+`"
storage:
  mirror_budget_bytes: 1073741824
`, 0o600)

	t.Setenv(bootstrap.EnvMirrorBudget, "2147483648")

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.MirrorBudgetBytes != 2147483648 {
		t.Errorf("MirrorBudgetBytes = %d, want the environment's 2 GiB", cfg.MirrorBudgetBytes)
	}
}

func TestNegativeMirrorBudgetIsRefused(t *testing.T) {
	isolate(t)
	t.Setenv(bootstrap.EnvSyncKey, key)
	t.Setenv(bootstrap.EnvMirrorBudget, "-1")

	if _, err := bootstrap.LoadConfig(); err == nil {
		t.Fatal("a negative mirror budget was accepted")
	}
}

func TestMirrorBudgetDefaults(t *testing.T) {
	isolate(t)
	t.Setenv(bootstrap.EnvSyncKey, key)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.MirrorBudgetBytes != 20<<30 {
		t.Errorf("MirrorBudgetBytes = %d, want 20 GiB", cfg.MirrorBudgetBytes)
	}
}

// A MySQL DSN carries its password too, and carries it before any scheme.
// Redaction that keyed on "://" printed it in full.
func TestRedactedHidesAMySQLPassword(t *testing.T) {
	dir := isolate(t)

	write(t, filepath.Join(dir, "nit.yaml"), `
database:
  url: "nit:hunter2@tcp(db:3306)/nit"
security:
  sync_key: "`+key+`"
`, 0o600)

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	rendered := strings.Join(cfg.Redacted(), "\n")

	if strings.Contains(rendered, "hunter2") {
		t.Errorf("the MySQL password was printed:\n%s", rendered)
	}
	if !strings.Contains(rendered, "nit:***@tcp(db:3306)/nit") {
		t.Errorf("the DSN was not redacted usefully:\n%s", rendered)
	}
}
