package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvConfigFile names an explicit configuration file, overriding the search.
const EnvConfigFile = "NIT_CONFIG"

// searchPath is where a configuration file is looked for, in order, when none
// is named explicitly.
//
// Working directory first so a developer can run a checkout without touching
// the system, /etc/nit last so a package's file is the fallback rather than the
// override. The XDG location sits between them for a single-user install.
var searchPath = []string{
	"./nit.yaml",
	"./nit.yml",
	"$XDG_CONFIG_HOME/nit/nit.yaml",
	"$HOME/.config/nit/nit.yaml",
	"/etc/nit/nit.yaml",
	"/etc/nit/nit.yml",
}

// SearchPaths returns the candidate locations, expanded, with the ones whose
// variables are unset dropped.
//
// Expanding an unset $XDG_CONFIG_HOME leaves "/nit/nit.yaml" — a path in the
// filesystem root that nobody meant and that would be confusing to see listed.
func SearchPaths() []string {
	var out []string

	for _, candidate := range searchPath {
		if strings.Contains(candidate, "$") {
			variable := candidate[strings.Index(candidate, "$")+1 : strings.Index(candidate, "/")]
			if os.Getenv(variable) == "" {
				continue
			}
		}

		if expanded := os.ExpandEnv(candidate); expanded != "" {
			out = append(out, expanded)
		}
	}

	return out
}

// File is the on-disk configuration.
//
// It is grouped the way an operator thinks about a deployment rather than
// flattened to match the process's internal Config: "where is the database",
// "how does the queue behave", "what are the secrets".
//
// Every secret field has a `_file` twin. A secret manager, a Kubernetes secret
// and systemd's LoadCredential all deliver secrets as files; reading them
// directly means the value never has to be interpolated into a config file that
// something else might back up, log or commit.
type File struct {
	Server   fileServer   `yaml:"server"`
	Database fileDatabase `yaml:"database"`
	Policy   filePolicy   `yaml:"policy"`
	Storage  fileStorage  `yaml:"storage"`
	Queue    fileQueue    `yaml:"queue"`
	Security fileSecurity `yaml:"security"`
	Forge    fileForge    `yaml:"forge"`
	Git      fileGit      `yaml:"git"`
	Log      fileLog      `yaml:"log"`
}

type fileServer struct {
	Addr         string   `yaml:"addr"`
	AdminGroups  []string `yaml:"admin_groups"`
	CORSOrigins  []string `yaml:"cors_origins"`
	EventMaxWait string   `yaml:"event_max_wait"`
}

type fileDatabase struct {
	URL     string `yaml:"url"`
	URLFile string `yaml:"url_file"`
}

type filePolicy struct {
	Dir    string `yaml:"dir"`
	Reload string `yaml:"reload"`
}

type fileStorage struct {
	BlobDir       string `yaml:"blob_dir"`
	WorkDir       string `yaml:"work_dir"`
	MaxPatchBytes int64  `yaml:"max_patch_bytes"`
	PullTTL       string `yaml:"pull_ttl"`
}

type fileQueue struct {
	LeaseDuration string `yaml:"lease_duration"`
	MaxAttempts   int    `yaml:"max_attempts"`
	Poll          string `yaml:"poll"`
	ReapEvery     string `yaml:"reap_every"`
}

type fileSecurity struct {
	SyncKey     string `yaml:"sync_key"`
	SyncKeyFile string `yaml:"sync_key_file"`
}

type fileForge struct {
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
}

type fileGit struct {
	SSHCommand string `yaml:"ssh_command"`
}

type fileLog struct {
	Level string `yaml:"level"`
}

// FindConfigFile returns the configuration file to use, or "" when there is
// none.
//
// An explicitly named file that does not exist is an error: an operator who
// passed -config meant that file, and silently falling back to the search path
// would run a deployment on settings they did not choose.
func FindConfigFile(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv(EnvConfigFile)
	}

	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("configuration file %s: %w", explicit, err)
		}
		return explicit, nil
	}

	for _, path := range SearchPaths() {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}

	return "", nil
}

// LoadFile reads and validates a configuration file.
func LoadFile(path string) (*File, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}

	var file File

	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	decoder.KnownFields(true)

	if err := decoder.Decode(&file); err != nil && !errors.Is(err, errEOF(err)) {
		// A misspelled key is an error, not a shrug. A setting that is silently
		// ignored is how a deployment ends up running with a default nobody
		// chose — the exact failure this file exists to prevent.
		return nil, fmt.Errorf("configuration file %s: %w", path, err)
	}

	if err := file.checkPermissions(path); err != nil {
		return nil, err
	}

	return &file, nil
}

// errEOF recognizes the "empty document" error yaml.v3 returns, so a file that
// is entirely comments is treated as "no settings" rather than as broken.
func errEOF(err error) error {
	if err != nil && err.Error() == "EOF" {
		return err
	}
	return nil
}

// checkPermissions refuses a world- or group-readable file that carries a
// secret inline.
//
// A configuration file holding a database password and a sync token signing key
// is a credential. Refusing to read a readable one is deliberately louder than
// warning: a warning in a start-up log is a warning nobody sees.
//
// The `_file` indirections are exempt — they name a path, not a secret — which
// is the recommended way to keep the configuration file itself unprivileged.
func (f *File) checkPermissions(path string) error {
	if !f.hasInlineSecret() {
		return nil
	}

	// Windows has no POSIX permission bits. Go synthesises Mode().Perm() from
	// the read-only attribute alone, so every file reports 0666 and this check
	// would refuse every configuration carrying a secret — while telling the
	// operator to run chmod, which does not exist there.
	//
	// Access control on Windows is an ACL, and os.Stat cannot see one. Skipping
	// is therefore not a weakening of a check that worked: it is declining to
	// enforce something this API cannot observe. Deployments on Windows have to
	// secure the file themselves, or use the _file indirections, which carry no
	// secret at all.
	if runtime.GOOS == "windows" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf(
			"configuration file %s is mode %04o and contains secrets; "+
				"run: chmod 600 %s — or move the secrets to sync_key_file, url_file and token_file",
			path, mode, path)
	}

	return nil
}

// hasInlineSecret reports whether the file carries a credential in its own
// text.
//
// A database URL only counts when it actually has a password. Treating every
// DSN as a secret would refuse a perfectly ordinary
// "postgres://localhost/nit" — no credential in it at all — and an operator who
// is told to lock down a file that holds nothing sensitive learns to ignore the
// message.
func (f *File) hasInlineSecret() bool {
	return f.Security.SyncKey != "" || f.Forge.Token != "" || hasPassword(f.Database.URL)
}

// hasPassword reports whether a DSN embeds a password.
func hasPassword(raw string) bool {
	scheme := strings.Index(raw, "://")
	at := strings.LastIndex(raw, "@")

	if scheme < 0 || at < scheme {
		// Not a URL, or no userinfo. A key/value DSN ("host=… password=…") is
		// the other form PostgreSQL accepts.
		return strings.Contains(raw, "password=")
	}

	return strings.Contains(raw[scheme+3:at], ":")
}

// readSecret resolves a value that may be given inline or as a file path.
func readSecret(inline, path, field string) (string, error) {
	if inline != "" && path != "" {
		return "", fmt.Errorf("configuration: %s and %s_file are both set; use one", field, field)
	}
	if inline != "" {
		return inline, nil
	}
	if path == "" {
		return "", nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("configuration: %s_file: %w", field, err)
	}

	// Trailing newlines are what every editor and every secret mount adds, and
	// a signing key with a stray "\n" is a key that differs between replicas
	// depending on how each one was written.
	return strings.TrimSpace(string(content)), nil
}

// ExampleFile is a fully commented configuration file, written by
// "nitctl config init".
const ExampleFile = `# nit configuration.
#
# Every setting here can also be given as an environment variable, and the
# environment wins: file first, environment on top. Use whichever fits your
# deployment — a file for what is stable, the environment for what a container
# orchestrator injects.
#
# Anything deciding *who may do what* belongs in the policy bundle, not here.

server:
  addr: ":8080"

  # Groups allowed to read the operations API (/v1/admin, nitctl stats|tasks|audit,
  # the web console). Empty means nobody. Configured here rather than in the
  # policy bundle so a bad bundle cannot lock an operator out of the tool for
  # diagnosing it.
  admin_groups: []

  # Browser origins allowed to call the API. Only needed when the console is
  # served from a different origin. There is no wildcard, on purpose.
  cors_origins: []

  # How long a client's long poll is held open before answering with no change.
  # Keep it under any proxy idle timeout in front of nitd.
  event_max_wait: 30s

database:
  url: "postgres://nit@localhost/nit?sslmode=require"
  # url_file: /run/secrets/nit-database-url

policy:
  dir: /etc/nit/policy

  # How often the bundle directory is reread. A bundle that does not compile is
  # not applied: the last good one stays in force.
  reload: 30s

storage:
  # Must be the same for nitd and every worker: nitd writes the authorized patch
  # here and the worker reads it back.
  blob_dir: /var/lib/nit/blobs

  # Scratch space for clones, local to each worker. Size it for the largest
  # repository times the worker's concurrency.
  work_dir: /var/lib/nit/work

  max_patch_bytes: 104857600   # 100 MiB
  pull_ttl: 24h

queue:
  # How long a worker may hold a task without a heartbeat. It has to survive a
  # clone: too short and a large repository loses its task mid-flight; too long
  # and a crashed worker blocks its branch for that long.
  lease_duration: 60s

  max_attempts: 3
  poll: 1s
  reap_every: 30s

security:
  # Signs sync tokens. At least 32 bytes, shared by every replica, never
  # rotated casually: rotating it makes every workspace resynchronize.
  #   openssl rand -base64 32
  #
  # Inline requires this file to be mode 600. Prefer the file form.
  # sync_key: "…"
  sync_key_file: /etc/nit/sync.key

forge:
  # The credential nit pushes with. nit is the only writer of the upstream, so
  # this is a machine identity, not a person's. Workers only.
  # token: "…"
  token_file: /run/secrets/nit-forge-token

git:
  # Passed to every git invocation as GIT_SSH_COMMAND. Workers only, and only
  # for ssh:// remotes — an HTTPS remote uses forge.token instead.
  #
  # A passthrough rather than an ssh_key setting: a key path would cover the
  # simplest case and leave agents, ProxyJump, per-host keys and non-standard
  # ports to git anyway. Leave it empty to keep whatever the process inherits.
  #
  # -i with IdentitiesOnly stops ssh offering every key it can find; the
  # known_hosts pair is what makes the host verifiable; BatchMode turns a
  # passphrase prompt into an error instead of a worker that hangs.
  # ssh_command: >-
  #   ssh -i /run/secrets/nit-ssh-key
  #   -o IdentitiesOnly=yes
  #   -o UserKnownHostsFile=/etc/nit/ssh/known_hosts
  #   -o StrictHostKeyChecking=yes
  #   -o BatchMode=yes

log:
  level: info   # debug | info | warn | error
`

// DefaultConfigPath is where "nitctl config init" writes, and the last entry of
// the search path.
var DefaultConfigPath = filepath.Join("/etc", "nit", "nit.yaml")
