package bootstrap

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/pkg/policy"
)

// Environment variable names. They are constants so a typo is a compile error
// in one place rather than a silently missing setting in three.
const (
	EnvAddr        = "NIT_ADDR"
	EnvDatabaseURL = "NIT_DATABASE_URL"
	EnvPolicyDir   = "NIT_POLICY_DIR"
	EnvBlobDir     = "NIT_BLOB_DIR"
	EnvSyncKey     = "NIT_SYNC_KEY"
	EnvWorkDir     = "NIT_WORK_DIR"
	EnvMaxPatch    = "NIT_MAX_PATCH_BYTES"
	EnvLogLevel    = "NIT_LOG_LEVEL"
	EnvAdminGroups = "NIT_ADMIN_GROUPS"
	EnvCORSOrigins = "NIT_CORS_ORIGINS"
	EnvForgeToken  = "NIT_FORGE_TOKEN"
	EnvGitSSHCmd   = "NIT_GIT_SSH_COMMAND"

	// The `_FILE` forms exist because that is how a container runtime delivers
	// a secret: Docker secrets, Kubernetes secrets and systemd's LoadCredential
	// all mount a file. Without them a deployment would have to interpolate the
	// value into the environment, where `docker inspect` and every crash dump
	// can read it.
	EnvDatabaseURLFile = "NIT_DATABASE_URL_FILE"
	EnvSyncKeyFile     = "NIT_SYNC_KEY_FILE"
	EnvForgeTokenFile  = "NIT_FORGE_TOKEN_FILE"

	EnvLeaseDuration = "NIT_LEASE_DURATION"
	EnvMaxAttempts   = "NIT_MAX_ATTEMPTS"
	EnvQueuePoll     = "NIT_QUEUE_POLL"
	EnvReapEvery     = "NIT_REAP_EVERY"
	EnvPullTTL       = "NIT_PULL_TTL"
	EnvEventMaxWait  = "NIT_EVENT_MAX_WAIT"
	EnvPolicyReload  = "NIT_POLICY_RELOAD"
)

// Origin says where a setting's value came from.
//
// It exists because "why is my lease 60 seconds?" is a question an operator
// asks at the worst possible moment, and the honest answer needs three places
// checked. `nitctl config show` prints this next to every value.
type Origin string

const (
	OriginDefault Origin = "default"
	OriginFile    Origin = "file"
	OriginEnv     Origin = "env"
)

// Config is the effective configuration of a process.
type Config struct {
	Addr        string
	DatabaseURL string
	PolicyDir   string
	BlobDir     string
	WorkDir     string

	// SyncKey signs sync tokens. Every nitd replica must share it: a token
	// minted by one and rejected by another would make a load-balanced
	// deployment fail at random.
	SyncKey []byte

	// ForgeToken is the credential workers push with. nit is the only writer of
	// the upstream, so it is a machine identity, not a person's.
	ForgeToken string

	// GitSSHCommand is passed to every git invocation as GIT_SSH_COMMAND.
	//
	// It is a passthrough, not an abstraction: nit has no SSH settings of its
	// own because a key path could only ever cover the simplest case, while
	// agents, ProxyJump, per-host keys and non-standard ports would still need
	// git's own configuration. Handing the whole command over keeps one place
	// to look without pretending to manage keys.
	//
	// Empty leaves the worker's inherited environment untouched, so a host
	// already configured through ~/.ssh/config keeps working.
	GitSSHCommand string

	MaxPatchBytes int64
	LogLevel      slog.Level

	// AdminGroups may read the operations API. Named here rather than in the
	// policy bundle so that a bad bundle cannot lock an operator out of the
	// tool for diagnosing it.
	AdminGroups []policy.GroupID

	// CORSOrigins are the browser origins allowed to call the API. Empty means
	// no cross-origin access, which is right for a console served from the same
	// host.
	CORSOrigins []string

	// LeaseDuration is how long a worker may hold a task without a heartbeat.
	//
	// It has to survive a clone: too short and a large repository loses its task
	// mid-flight to a competitor; too long and a crashed worker blocks its
	// branch for that long. This is the setting most likely to need changing per
	// deployment.
	LeaseDuration time.Duration

	// MaxAttempts caps retries of a failing task before it is failed for good.
	MaxAttempts int

	// QueuePoll is how often an idle worker asks for work.
	QueuePoll time.Duration

	// ReapEvery is how often abandoned tasks are returned to the queue.
	ReapEvery time.Duration

	// PullTTL is how long a generated pull patch stays fetchable.
	PullTTL time.Duration

	// EventMaxWait is how long the server holds a client's long poll open.
	EventMaxWait time.Duration

	// PolicyReload is how often the bundle directory is reread.
	PolicyReload time.Duration

	// ConfigFile is the file that was loaded, or "" when none was found.
	ConfigFile string

	// Origins records where each setting came from, keyed by the name used in
	// `nitctl config show`.
	Origins map[string]Origin
}

// LoadConfig assembles the effective configuration.
//
// Three layers, lowest to highest: built-in defaults, the configuration file,
// then the environment. The environment wins because that is what a container
// orchestrator injects, and because an operator debugging a host expects
// `NIT_LOG_LEVEL=debug nitd` to work whatever the file says.
//
// A malformed value at any layer stops the process. Falling back to a default
// the operator did not choose is how a deployment ends up running something
// nobody intended.
func LoadConfig() (Config, error) {
	return LoadConfigFrom("")
}

// LoadConfigFrom is LoadConfig with an explicitly named configuration file,
// as passed to `-config`.
func LoadConfigFrom(explicitPath string) (Config, error) {
	// The configuration assembled so far is returned even when a later layer
	// fails. A broken configuration is exactly when `nitctl config show` is most
	// useful, and a screen of zeroes tells an operator nothing about what they
	// did have.
	cfg := defaults()

	path, err := FindConfigFile(explicitPath)
	if err != nil {
		return cfg, err
	}

	if path != "" {
		file, err := LoadFile(path)
		if err != nil {
			return cfg, err
		}

		cfg.ConfigFile = path

		if err := cfg.applyFile(file); err != nil {
			return cfg, err
		}
	}

	if err := cfg.applyEnv(); err != nil {
		return cfg, err
	}

	return cfg, cfg.validate()
}

func defaults() Config {
	return Config{
		Addr:          ":8080",
		PolicyDir:     "./configs/policy/example",
		BlobDir:       "./var/blobs",
		WorkDir:       "./var/work",
		MaxPatchBytes: 100 << 20,
		LogLevel:      slog.LevelInfo,

		LeaseDuration: 60 * time.Second,
		MaxAttempts:   3,
		QueuePoll:     time.Second,
		ReapEvery:     30 * time.Second,
		PullTTL:       24 * time.Hour,
		EventMaxWait:  30 * time.Second,
		PolicyReload:  30 * time.Second,

		Origins: map[string]Origin{},
	}
}

// mark records where a value came from.
func (c *Config) mark(name string, origin Origin) {
	c.Origins[name] = origin
}

// applyFile layers a configuration file over the defaults.
func (c *Config) applyFile(f *File) error {
	set := func(name string, target *string, value string) {
		if value != "" {
			*target = value
			c.mark(name, OriginFile)
		}
	}

	set("addr", &c.Addr, f.Server.Addr)
	set("policy.dir", &c.PolicyDir, f.Policy.Dir)
	set("storage.blob_dir", &c.BlobDir, f.Storage.BlobDir)
	set("storage.work_dir", &c.WorkDir, f.Storage.WorkDir)

	if len(f.Server.AdminGroups) > 0 {
		c.AdminGroups = toGroups(f.Server.AdminGroups)
		c.mark("server.admin_groups", OriginFile)
	}
	if len(f.Server.CORSOrigins) > 0 {
		c.CORSOrigins = f.Server.CORSOrigins
		c.mark("server.cors_origins", OriginFile)
	}
	if f.Storage.MaxPatchBytes > 0 {
		c.MaxPatchBytes = f.Storage.MaxPatchBytes
		c.mark("storage.max_patch_bytes", OriginFile)
	}
	if f.Queue.MaxAttempts > 0 {
		c.MaxAttempts = f.Queue.MaxAttempts
		c.mark("queue.max_attempts", OriginFile)
	}
	if f.Log.Level != "" {
		level, err := parseLevel(f.Log.Level)
		if err != nil {
			return err
		}
		c.LogLevel = level
		c.mark("log.level", OriginFile)
	}

	durations := []struct {
		name   string
		target *time.Duration
		raw    string
	}{
		{"queue.lease_duration", &c.LeaseDuration, f.Queue.LeaseDuration},
		{"queue.poll", &c.QueuePoll, f.Queue.Poll},
		{"queue.reap_every", &c.ReapEvery, f.Queue.ReapEvery},
		{"storage.pull_ttl", &c.PullTTL, f.Storage.PullTTL},
		{"server.event_max_wait", &c.EventMaxWait, f.Server.EventMaxWait},
		{"policy.reload", &c.PolicyReload, f.Policy.Reload},
	}

	for _, d := range durations {
		if d.raw == "" {
			continue
		}

		parsed, err := parseDuration(d.name, d.raw)
		if err != nil {
			return err
		}

		*d.target = parsed
		c.mark(d.name, OriginFile)
	}

	// Secrets, each of which may be inline or read from a file it names.
	database, err := readSecret(f.Database.URL, f.Database.URLFile, "database.url")
	if err != nil {
		return err
	}
	if database != "" {
		c.DatabaseURL = database
		c.mark("database.url", OriginFile)
	}

	key, err := readSecret(f.Security.SyncKey, f.Security.SyncKeyFile, "security.sync_key")
	if err != nil {
		return err
	}
	if key != "" {
		decoded, err := decodeSyncKey(key)
		if err != nil {
			return err
		}
		c.SyncKey = decoded
		c.mark("security.sync_key", OriginFile)
	}

	token, err := readSecret(f.Forge.Token, f.Forge.TokenFile, "forge.token")
	if err != nil {
		return err
	}
	if token != "" {
		c.ForgeToken = token
		c.mark("forge.token", OriginFile)
	}

	if f.Git.SSHCommand != "" {
		c.GitSSHCommand = f.Git.SSHCommand
		c.mark("git.ssh_command", OriginFile)
	}

	return nil
}

// applyEnv layers the environment over whatever came before.
func (c *Config) applyEnv() error {
	plain := []struct {
		name   string
		env    string
		target *string
	}{
		{"addr", EnvAddr, &c.Addr},
		{"policy.dir", EnvPolicyDir, &c.PolicyDir},
		{"storage.blob_dir", EnvBlobDir, &c.BlobDir},
		{"storage.work_dir", EnvWorkDir, &c.WorkDir},
		{"git.ssh_command", EnvGitSSHCmd, &c.GitSSHCommand},
	}

	for _, setting := range plain {
		if value := os.Getenv(setting.env); value != "" {
			*setting.target = value
			c.mark(setting.name, OriginEnv)
		}
	}

	if raw := os.Getenv(EnvAdminGroups); raw != "" {
		c.AdminGroups = toGroups(splitList(raw))
		c.mark("server.admin_groups", OriginEnv)
	}
	if raw := os.Getenv(EnvCORSOrigins); raw != "" {
		c.CORSOrigins = splitList(raw)
		c.mark("server.cors_origins", OriginEnv)
	}
	if raw := os.Getenv(EnvLogLevel); raw != "" {
		level, err := parseLevel(raw)
		if err != nil {
			return err
		}
		c.LogLevel = level
		c.mark("log.level", OriginEnv)
	}
	// Secrets, inline or from a file the variable names. The file form wins if
	// both are set, because a deployment that mounts a secret meant it.
	secrets := []struct {
		name     string
		inlineOf string
		fileOf   string
		apply    func(string) error
	}{
		{
			name: "database.url", inlineOf: EnvDatabaseURL, fileOf: EnvDatabaseURLFile,
			apply: func(v string) error { c.DatabaseURL = v; return nil },
		},
		{
			name: "forge.token", inlineOf: EnvForgeToken, fileOf: EnvForgeTokenFile,
			apply: func(v string) error { c.ForgeToken = v; return nil },
		},
		{
			name: "security.sync_key", inlineOf: EnvSyncKey, fileOf: EnvSyncKeyFile,
			apply: func(v string) error {
				decoded, err := decodeSyncKey(v)
				if err != nil {
					return err
				}
				c.SyncKey = decoded
				return nil
			},
		},
	}

	for _, secret := range secrets {
		value := os.Getenv(secret.inlineOf)

		if path := os.Getenv(secret.fileOf); path != "" {
			content, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("%s: %w", secret.fileOf, err)
			}
			// Trailing whitespace is what every secret mount adds; a signing key
			// with a stray newline differs between replicas depending on how
			// each one was written.
			value = strings.TrimSpace(string(content))
		}

		if value == "" {
			continue
		}

		if err := secret.apply(value); err != nil {
			return err
		}

		c.mark(secret.name, OriginEnv)
	}

	if raw := os.Getenv(EnvMaxPatch); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("%s: must be a positive number of bytes", EnvMaxPatch)
		}
		c.MaxPatchBytes = parsed
		c.mark("storage.max_patch_bytes", OriginEnv)
	}
	if raw := os.Getenv(EnvMaxAttempts); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("%s: must be a positive integer", EnvMaxAttempts)
		}
		c.MaxAttempts = parsed
		c.mark("queue.max_attempts", OriginEnv)
	}

	durations := []struct {
		name   string
		env    string
		target *time.Duration
	}{
		{"queue.lease_duration", EnvLeaseDuration, &c.LeaseDuration},
		{"queue.poll", EnvQueuePoll, &c.QueuePoll},
		{"queue.reap_every", EnvReapEvery, &c.ReapEvery},
		{"storage.pull_ttl", EnvPullTTL, &c.PullTTL},
		{"server.event_max_wait", EnvEventMaxWait, &c.EventMaxWait},
		{"policy.reload", EnvPolicyReload, &c.PolicyReload},
	}

	for _, d := range durations {
		raw := os.Getenv(d.env)
		if raw == "" {
			continue
		}

		parsed, err := parseDuration(d.env, raw)
		if err != nil {
			return err
		}

		*d.target = parsed
		c.mark(d.name, OriginEnv)
	}

	return nil
}

// validate rejects a configuration that cannot work, naming the setting and
// where it would have to be fixed.
func (c *Config) validate() error {
	if len(c.SyncKey) == 0 {
		return fmt.Errorf(
			"no sync token signing key: set security.sync_key_file in the configuration file, "+
				"or %s; generate one with: openssl rand -base64 32", EnvSyncKey)
	}
	if len(c.SyncKey) < synctoken.MinKeyBytes {
		return fmt.Errorf("the sync token signing key is %d bytes, need at least %d",
			len(c.SyncKey), synctoken.MinKeyBytes)
	}

	return nil
}

// Origin reports where a setting's value came from.
func (c Config) Origin(name string) Origin {
	if origin, ok := c.Origins[name]; ok {
		return origin
	}
	return OriginDefault
}

// Redacted renders the effective configuration for `nitctl config show`.
//
// Secrets are never printed, only whether they are set and where they came
// from. An operator diagnosing a deployment needs to know the key is present
// and which layer supplied it; nobody needs it echoed to a terminal that
// scrolls into a support ticket.
func (c Config) Redacted() []string {
	rows := [][2]string{
		{"addr", c.Addr},
		{"database.url", redactURL(c.DatabaseURL)},
		{"policy.dir", c.PolicyDir},
		{"policy.reload", c.PolicyReload.String()},
		{"storage.blob_dir", c.BlobDir},
		{"storage.work_dir", c.WorkDir},
		{"storage.max_patch_bytes", strconv.FormatInt(c.MaxPatchBytes, 10)},
		{"storage.pull_ttl", c.PullTTL.String()},
		{"queue.lease_duration", c.LeaseDuration.String()},
		{"queue.max_attempts", strconv.Itoa(c.MaxAttempts)},
		{"queue.poll", c.QueuePoll.String()},
		{"queue.reap_every", c.ReapEvery.String()},
		{"server.admin_groups", joinGroups(c.AdminGroups)},
		{"server.cors_origins", strings.Join(c.CORSOrigins, ",")},
		{"server.event_max_wait", c.EventMaxWait.String()},
		{"security.sync_key", present(len(c.SyncKey) > 0)},
		{"forge.token", present(c.ForgeToken != "")},

		// Printed in full, unlike the secrets above: it is a command line with
		// paths in it, and being able to read it back is the entire reason for
		// preferring it to an environment variable set three layers away.
		{"git.ssh_command", c.GitSSHCommand},

		{"log.level", c.LogLevel.String()},
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows {
		value := row[1]
		if value == "" {
			value = "-"
		}

		out = append(out, fmt.Sprintf("%-26s %-12s %s", row[0], c.Origin(row[0]), value))
	}

	sort.Strings(out)

	return out
}

func present(set bool) string {
	if set {
		return "(set)"
	}
	return "(not set)"
}

// redactURL keeps a DSN readable without printing its password.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}

	at := strings.LastIndex(raw, "@")
	scheme := strings.Index(raw, "://")

	if at < 0 || scheme < 0 || at < scheme {
		return raw
	}

	credentials := raw[scheme+3 : at]
	if colon := strings.Index(credentials, ":"); colon >= 0 {
		credentials = credentials[:colon] + ":***"
	}

	return raw[:scheme+3] + credentials + raw[at:]
}

// decodeSyncKey accepts a base64 key or a raw one.
func decodeSyncKey(raw string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) >= synctoken.MinKeyBytes {
		return decoded, nil
	}

	if len(raw) < synctoken.MinKeyBytes {
		return nil, fmt.Errorf("the sync token signing key must be at least %d bytes", synctoken.MinKeyBytes)
	}

	return []byte(raw), nil
}

func parseDuration(name, raw string) (time.Duration, error) {
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w (expected a duration such as 90s, 5m, 2h)", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}

	return parsed, nil
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log level %q: expected debug, info, warn or error", raw)
	}
}

func toGroups(names []string) []policy.GroupID {
	out := make([]policy.GroupID, 0, len(names))
	for _, name := range names {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			out = append(out, policy.GroupID(trimmed))
		}
	}
	return out
}

func joinGroups(groups []policy.GroupID) string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, string(g))
	}
	return strings.Join(out, ",")
}

// splitList parses a comma-separated environment variable.
func splitList(raw string) []string {
	var out []string

	for part := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

// NewLogger returns the process logger.
func NewLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
