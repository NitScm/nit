# Configuration

Everything nit needs to run: the four binaries, every setting, and the
deployment shapes they fit into.

Three rules run through all of it.

**Configuration is a file and environment variables, policy is a bundle.**
Anything that decides *who may do what* lives in the policy bundle — authored in
YAML, reviewed like code, versioned and rollbackable (see `POLICY.md`). Anything
that decides *how this process runs* is configuration. Nothing crosses that
line: there is no setting that grants access, and no policy rule that sets a
port.

**Layers, lowest to highest: defaults, file, environment.** The environment wins
because that is what a container orchestrator injects, and because an operator
debugging a host expects `NIT_LOG_LEVEL=debug nitd` to work whatever the file
says. Use whichever fits: a file for what is stable, the environment for what is
injected.

**A malformed setting stops the process.** A bad duration, a short signing key,
a misspelled key in the file: the binary refuses to start and says which setting
is wrong and where. Silently falling back to a default the operator did not
choose is how a deployment ends up running something nobody intended.

---

## 1. The binaries

| Binary | Runs where | Needs |
| --- | --- | --- |
| `nitd` | Control plane, one or more replicas | PostgreSQL, policy bundle, blob storage |
| `nit-worker` | Anywhere with git and disk | The same, plus a forge credential |
| `nitctl` | Operator's machine or the server | Either PostgreSQL (admin) or the API (ops) |
| `nit` | Developer's machine | Nothing but the server URL and a token |

`nitd` performs no git operation. Everything that clones, applies or pushes runs
in `nit-worker`, so a slow or hostile repository can never hold an API request
open. That is why the two are separate processes and why they scale separately.

```
                    ┌────────────┐
   nit (developer)  │            │   PostgreSQL
        ───────────▶│    nitd    │◀────────────
                    │            │
                    └─────┬──────┘
                          │ queue
                    ┌─────▼──────┐
                    │ nit-worker │────────────▶ the forge (GitHub, GitLab, …)
                    └────────────┘
```

---

## 2. The configuration file

### Getting one

```sh
nitctl config init                    # writes /etc/nit/nit.yaml, mode 600
nitctl config init -config ./nit.yaml # or wherever you like
```

The generated file is fully commented and documents every setting in place.

### Where it is looked for

In order, first match wins:

1. `-config <path>` on `nitd` or `nit-worker`
2. `$NIT_CONFIG`
3. `./nit.yaml`, `./nit.yml`
4. `$XDG_CONFIG_HOME/nit/nit.yaml`
5. `$HOME/.config/nit/nit.yaml`
6. `/etc/nit/nit.yaml`, `/etc/nit/nit.yml`

Working directory first so a developer can run a checkout without touching the
system; `/etc/nit` last so a package's file is the fallback rather than the
override.

```sh
nitctl config path        # which file would be read
nitctl config show        # every effective value, and where it came from
```

A file named explicitly with `-config` or `$NIT_CONFIG` that does not exist is
an **error**, not a fallback: an operator who named a file meant that file.

### What it looks like

```yaml
server:
  addr: ":8080"
  admin_groups: [platform]
  cors_origins: []
  event_max_wait: 30s

database:
  url: "postgres://nit@localhost/nit?sslmode=require"
  # url_file: /run/secrets/nit-database-url

policy:
  dir: /etc/nit/policy
  reload: 30s

storage:
  blob_dir: /var/lib/nit/blobs
  work_dir: /var/lib/nit/work
  mirror_budget_bytes: 21474836480
  max_patch_bytes: 104857600
  pull_ttl: 24h

queue:
  lease_duration: 60s
  max_attempts: 3
  poll: 1s
  reap_every: 30s

security:
  sync_key_file: /etc/nit/sync.key

forge:
  token_file: /run/secrets/nit-forge-token

log:
  level: info
```

A misspelled key is refused, naming the field:

```
nitd: configuration file /etc/nit/nit.yaml: yaml: unmarshal errors:
  line 2: field adr not found in type bootstrap.fileServer
```

### Secrets

Every secret has an inline form and a `_file` twin:

| Inline | From a file | Environment |
| --- | --- | --- |
| `database.url` | `database.url_file` | `NIT_DATABASE_URL_FILE` |
| `security.sync_key` | `security.sync_key_file` | `NIT_SYNC_KEY_FILE` |
| `forge.token` | `forge.token_file` | `NIT_FORGE_TOKEN_FILE` |

**Prefer the `_file` form.** A secret manager, a Kubernetes secret and systemd's
`LoadCredential` all deliver secrets as files; reading them directly means the
value is never interpolated into a file that something else might back up, log
or commit. Trailing whitespace is trimmed, so the newline every editor and every
secret mount adds does not become part of your signing key.

Setting both forms of the same secret is an error rather than a precedence
question nobody should have to remember.

**A file carrying an inline secret must be mode 600.** nit refuses to read a
group- or world-readable one:

```
nitd: configuration file /etc/nit/nit.yaml is mode 0644 and contains secrets;
run: chmod 600 /etc/nit/nit.yaml — or move the secrets to sync_key_file, url_file and token_file
```

Refusing is deliberately louder than warning: a warning in a start-up log is a
warning nobody sees. A file using only the `_file` indirections holds no secret
and needs no special mode.

### Seeing what is in force

```sh
nitctl config show
```

```
file: /etc/nit/nit.yaml

SETTING                     FROM         VALUE
addr                        file         127.0.0.1:8080
database.url                file         postgres://postgres:***@localhost:5432/nit
forge.token                 file         (set)
log.level                   file         INFO
policy.reload               default      30s
queue.lease_duration        file         5m0s
queue.max_attempts          file         5
queue.poll                  default      1s
security.sync_key           file         (set)
server.admin_groups         file         platform
storage.pull_ttl            default      24h0m0s
```

The `FROM` column answers "why is this setting what it is?" — the question an
operator asks at the worst possible moment, whose honest answer otherwise needs
three places checked. Secrets are never printed, only whether they are set and
which layer supplied them; a database URL keeps its host and user and loses its
password.

`nitctl migrate` and `nitctl token` read the same configuration, so an operator
does not have to restate a DSN their file already holds.

---

## 3. Settings shared by `nitd` and `nit-worker`

Both read the same configuration, on purpose: two processes that disagree about
the database, the bundle or the signing key fail in ways that are very hard to
diagnose.

Every setting below has a file key and an environment variable. The file key is
what `nitctl config show` prints.

### Required

| File key | Variable | What it is |
| --- | --- | --- |
| `database.url` | `NIT_DATABASE_URL` | Database DSN — see [Which database](#which-database) |
| `security.sync_key` | `NIT_SYNC_KEY` | Sync token signing key, **at least 32 bytes**, raw or base64 |
| `policy.dir` | `NIT_POLICY_DIR` | Directory holding the policy bundle |

#### Which database

Three backends are supported, and the DSN is what selects one. There is no
setting that names the engine: a DSN that names neither shape is refused at
start-up rather than guessed at.

| Engine | DSN shape |
| --- | --- |
| PostgreSQL | `postgres://nit:secret@db:5432/nit?sslmode=require` |
| MySQL 8.0.16+ | `nit:secret@tcp(db:3306)/nit?tls=true` |
| MariaDB 10.6+ | `nit:secret@tcp(db:3306)/nit?tls=true` |

MySQL and MariaDB share one driver and one schema; nothing distinguishes them
in configuration, and the conformance suite runs against both.

**PostgreSQL is the recommended backend**, and the reasons are specific rather
than a preference:

- **Migrations are transactional.** A migration that fails halfway rolls back.
  MySQL and MariaDB commit implicitly at every DDL statement, so a failure
  there leaves a partly-applied schema and no record of the version — the error
  names the statement that failed, and fixing it is manual. **Back up before
  migrating those two.**
- **The audit trail cannot be truncated.** PostgreSQL refuses `TRUNCATE` on
  `audit_log` with a trigger. Neither MySQL nor MariaDB fires a trigger for
  `TRUNCATE`, so on those the guarantee rests on a privilege instead — see
  below. `UPDATE` and `DELETE` are refused on all three.
- **The dispatch indexes are partial.** PostgreSQL indexes only queued tasks
  and only live sessions; the other two index every row ever written, so those
  indexes grow with history rather than with the backlog.

Everything the application does behaves identically across the three. That is
not an assertion of intent: `pkg/store/storetest` is one suite, and it runs
against each backend in CI.

##### The grant MySQL and MariaDB need

`TRUNCATE` requires the `DROP` privilege. Withholding it is what stops the
audit trail from being erased in one word, so the application account must not
have it:

```sql
CREATE USER 'nit'@'%' IDENTIFIED BY 'secret';
GRANT SELECT, INSERT, UPDATE, DELETE ON nit.* TO 'nit'@'%';
```

Migrations need more, and are run by a different account on purpose — a
deployment step an operator takes, not something the server can do to itself:

```sql
GRANT ALL PRIVILEGES ON nit.* TO 'nit_migrate'@'%';
```

Each of these can come from a file it names instead, in either layer:

| Secret | File key | Variable | Reads a path from |
| --- | --- | --- | --- |
| Database URL | `database.url_file` | `NIT_DATABASE_URL_FILE` | that path |
| Signing key | `security.sync_key_file` | `NIT_SYNC_KEY_FILE` | that path |
| Forge token | `forge.token_file` | `NIT_FORGE_TOKEN_FILE` | that path |

This is the recommended form, and the one the Compose stacks in `deploy/` use.
Docker secrets, Kubernetes secrets and systemd's `LoadCredential` all deliver a
secret as a file; reading it directly means the value never appears in
`docker inspect`, in a crash dump, or in a configuration file something else
might back up. Trailing whitespace is trimmed, so the newline every mount adds
does not become part of your signing key.

When both an inline value and a file are set in the environment, **the file
wins**: a deployment that mounts a secret meant it.

#### The signing key deserves its own paragraph

Generate it once, share it across every replica, and never rotate it casually:

```sh
openssl rand -base64 32
```

The sync token is a client's claim about which upstream commit its patch was
computed against, and the server applies that patch on top of whatever the token
names. The signature is what stops a client naming a base of its choosing.

There is deliberately **no default and no generated fallback**. A key generated
at start-up would differ between replicas and across restarts, silently
invalidating every client's token — which, to a developer, looks like their
workspace mysteriously demanding a full resynchronization.

Rotating it invalidates every sync token in existence. Every workspace will need
one `nit pull` to recover. That is survivable but it is not free; treat the key
like a database password.

### Storage

| File key | Variable | Default | What it is |
| --- | --- | --- | --- |
| `storage.blob_dir` | `NIT_BLOB_DIR` | `./var/blobs` | Where patch payloads are stored |
| `storage.work_dir` | `NIT_WORK_DIR` | `./var/work` | Where a worker keeps its git mirrors and task worktrees (worker only) |
| `storage.mirror_budget_bytes` | `NIT_MIRROR_BUDGET_BYTES` | `21474836480` (20 GiB) | Disk the mirrors may occupy before the least recently used are evicted; `0` disables eviction |

**`NIT_BLOB_DIR` must be shared between `nitd` and every worker.** `nitd` writes
the authorized patch there and the worker reads it back; two processes with two
separate directories produce `missing_patch` on every push. On one host that
means the same path; across hosts it means a shared volume, until the blob store
grows an object-storage backend.

`NIT_WORK_DIR` is scratch space, local to each worker, and does not need to be
shared or backed up. **It belongs to one worker process**: mirrors are locked
per repository within a process, and two workers pointed at one directory would
race on the same mirror.

It holds two things. A bare mirror per repository, kept between tasks so a task
fetches a delta instead of cloning — and one worktree per concurrent task, which
is removed when the task ends.

Only the worktrees return their disk on their own, so size the volume as
`NIT_MIRROR_BUDGET_BYTES` plus the largest repository times the worker's
concurrency, with room to spare. When the mirrors exceed the budget the least
recently used are removed until the rest fit; one whose worktree is still in use
is never among them. Setting the budget below the size of a single large
repository is worse than useless: that repository is evicted after every task
and cloned again on the next.

### Behaviour

| File key | Variable | Default | What it is |
| --- | --- | --- | --- |
| `log.level` | `NIT_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `storage.max_patch_bytes` | `NIT_MAX_PATCH_BYTES` | `104857600` (100 MiB) | Ceiling on a patch, compressed and decompressed |
| `queue.lease_duration` | `NIT_LEASE_DURATION` | `60s` | How long a worker may hold a task without a heartbeat |
| `queue.max_attempts` | `NIT_MAX_ATTEMPTS` | `3` | Retries before a task is failed for good |
| `queue.poll` | `NIT_QUEUE_POLL` | `1s` | How often an idle worker asks for work |
| `queue.reap_every` | `NIT_REAP_EVERY` | `30s` | How often abandoned tasks are returned to the queue |
| `storage.pull_ttl` | `NIT_PULL_TTL` | `24h` | How long a generated pull patch stays fetchable |
| `policy.reload` | `NIT_POLICY_RELOAD` | `30s` | How often the bundle directory is reread |

Durations accept Go syntax: `90s`, `5m`, `2h`.

#### Sizing `NIT_LEASE_DURATION`

This is the setting most likely to need changing. The lease has to survive the
longest gap a worker can go without heartbeating, which in practice means it has
to survive a clone.

- **Too short** and a large repository loses its task mid-flight: the lease
  lapses, another worker claims the same task, and the first one is cut off by
  its own fencing check. The symptom is tasks that retry forever without ever
  failing.
- **Too long** and a crashed worker blocks its branch for that long, because
  nothing else may claim a partition that is still leased.

Start at `60s`. If your clones take minutes, raise it to comfortably exceed
them — `5m` for a large monorepo is reasonable — and accept the matching
recovery delay after a crash.

---

## 4. `nitd` only

| File key | Variable | Default | What it is |
| --- | --- | --- | --- |
| `server.addr` | `NIT_ADDR` | `:8080` | Listen address |
| `server.admin_groups` | `NIT_ADMIN_GROUPS` | *(empty)* | Groups allowed to read the operations API |
| `server.cors_origins` | `NIT_CORS_ORIGINS` | *(empty)* | Browser origins allowed to call the API |
| `server.event_max_wait` | `NIT_EVENT_MAX_WAIT` | `30s` | How long a client's long poll is held open |

In the file these two are lists; in the environment they are comma-separated.

### Who may read the operations API

Empty means the operations API — `/v1/admin/*`, `nitctl stats|tasks|audit`, the
web console — is reachable by nobody. Non-members get **404**, not 403: the
existence of an operations API is not something an ordinary developer needs
confirmed.

It is server configuration rather than a policy rule on purpose. The console is
the tool for diagnosing a broken bundle; putting the permission to use it
*inside* the bundle would make that tool depend on the thing it exists to debug.

```sh
NIT_ADMIN_GROUPS=platform,sre
```

### Browser origins

Only needed when the web console is served from a different origin than the API.
Serve them from the same host and leave this empty.

There is **no wildcard**. The API is bearer-authenticated, so `*` would let any
page a developer happens to visit call it with their credentials.

```sh
NIT_CORS_ORIGINS=http://localhost:4200        # development only
```

### Long-poll duration

The CLI holds a long poll open while it waits for a task; the server answers
after this long even if nothing changed, and the client reconnects. Shorter
means more reconnections. Longer risks an intermediate proxy closing the
connection first — if you sit behind a load balancer with a 30-second idle
timeout, set this below it.

---

## 5. `nit-worker` only

| File key | Variable | Default | What it is |
| --- | --- | --- | --- |
| `forge.token` | `NIT_FORGE_TOKEN` | *(empty)* | Credential nit pushes to the forge with, over HTTPS |
| `git.ssh_command` | `NIT_GIT_SSH_COMMAND` | *(empty)* | Passed to every git invocation as `GIT_SSH_COMMAND` |

| Flag | Default | What it is |
| --- | --- | --- |
| `-queues` | `push,pull` | Which task kinds this worker takes |
| `-concurrency` | `1` | Runners in this process |
| `-name` | hostname | Identifier recorded on leases and in logs |

### The forge credential

nit is the **only writer** of the upstream repository, so this is a machine
identity, not a person's. Give it write access to the repositories in the bundle
and nothing else.

For HTTPS remotes it is injected into the URL at clone time. For SSH remotes,
leave it empty: the key does this job, and an unused secret is one more thing to
rotate.

The authenticated URL is a credential: it never appears in logs or in error
messages, which is why a clone failure reports only the branch it was cloning.

### SSH remotes

```yaml
git:
  ssh_command: >-
    ssh -i /run/secrets/nit-ssh-key
    -o IdentitiesOnly=yes
    -o UserKnownHostsFile=/etc/nit/ssh/known_hosts
    -o StrictHostKeyChecking=yes
    -o BatchMode=yes
```

`git.ssh_command` is passed to every git invocation as `GIT_SSH_COMMAND`. Left
empty, nothing is set and whatever the process inherits — `~/.ssh/config`, an
agent, a systemd `Environment=` line — stands untouched.

It is a **passthrough, not an abstraction.** There is deliberately no `ssh_key`
setting: a key path would cover only the simplest case, and agents, `ProxyJump`,
per-host keys, non-standard ports and certificate authentication would still be
git's business. Two half-overlapping ways to configure the same thing is worse
than one. The setting exists so that a deployment can keep everything in one
file and see it in `nitctl config show`, not so that nit manages keys.

A configured value **overrides** an inherited `GIT_SSH_COMMAND`. A setting that
silently did nothing on a host that happens to export one would be worse than no
setting at all: the wrong key gets offered and the failure reads like a
permissions problem on the forge.

Three options earn their place in that command:

| | |
| --- | --- |
| `-i` with `IdentitiesOnly=yes` | Otherwise ssh offers every key it can find, and on a host with an agent the wrong one goes first — the forge then answers as whatever account that key belongs to |
| `UserKnownHostsFile` + `StrictHostKeyChecking=yes` | A worker that accepts any host key will push the repository to whoever answers |
| `BatchMode=yes` | Workers run git with `GIT_TERMINAL_PROMPT=0`; this makes a passphrase prompt an error rather than a worker hanging with a branch leased |

An SSH key with a passphrase cannot work unattended. Use an agent and pass
`SSH_AUTH_SOCK` through the environment, or an unencrypted key readable only by
the worker's user.

### Scaling

Concurrency comes from running several runners, not from fanning out inside one:
each runner holds one clone at a time, which keeps a worker's disk budget
something an operator can predict.

```sh
nit-worker                          # takes both queues
nit-worker -queues=pull             # a machine dedicated to read traffic
nit-worker -concurrency=4           # four runners, four concurrent clones
```

Pushes serialize per branch whatever you do here — that is the queue's job — so
adding workers increases throughput across branches and repositories, never
within one branch.

---

## 6. `nitctl`

Two kinds of command, with two ways of reaching the system.

**Server-side commands** talk to the database directly and are run where the
database is reachable:

```sh
nitctl migrate -dsn "$NIT_DATABASE_URL"
nitctl migrate -status                      # list migrations without applying
nitctl token create -user alice -label laptop -ttl 720h
nitctl token list   -user alice
nitctl token revoke -id <session-id>

nitctl audit prune -keep-days 365           # counts what would go, deletes nothing
nitctl audit prune -keep-days 365 -yes      # deletes, in batches
```

They read `NIT_DATABASE_URL` and `NIT_POLICY_DIR` from the environment, or take
`-dsn` and `-policy`.

`audit prune` is the only way to remove audit records, and it is here rather
than among the operations commands on purpose: the server holds a `store.Store`
and cannot reach the pruning interface through it, so no request can delete
evidence. Without `-yes` it reports and stops — there is no undo. It writes
`audit.purge_started` and `audit.purge_completed` into the trail it is emptying,
naming whoever ran it, so an interrupted purge leaves a trace rather than an
unexplained gap.

**Operations commands** read the API, exactly as the web console does, so the
API is exercised from day one and the console can never need a capability the
command line lacks:

```sh
export NIT_SERVER=https://nit.example.com
export NIT_TOKEN=nit_…                      # or reuse the credential nit login stored

nitctl stats
nitctl tasks -state queued
nitctl audit -user alice -since 24h
nitctl audit -request <request-id> -json
```

**Policy commands** need no server at all, which is what makes them usable in
CI:

```sh
nitctl policy validate ./policy
nitctl policy show     ./policy
nitctl policy explain  ./policy -repo backend-api -user bob -path secrets/prod.key
```

Run `nitctl policy validate` in the policy repository's CI. A bundle that does
not compile must never reach production: `nitd` refuses to start on one, and a
running server keeps serving the last good bundle rather than reloading a broken
one.

---

## 7. `nit`, on a developer's machine

| Variable | Default | What it is |
| --- | --- | --- |
| `NIT_SERVER` | *(empty)* | Server URL, so `clone` and `whoami` need no flag |
| `NIT_CONFIG_DIR` | `~/.nit` | Where the credentials file lives |

Nothing else. A developer machine holds no forge credential, no database
password and no signing key.

```sh
nit login https://nit.example.com     # paste the token from nitctl token create
nit clone backend-api
cd backend-api
nit pull
nit push -m "my change"
nit push --check                       # authorize without submitting
nit status
```

The token is stored in `~/.nit/credentials.json` with mode `600`, keyed by
server so one machine can talk to a staging and a production deployment without
the tokens overwriting each other.

Inside a checkout, `.nit/state.json` holds the workspace id, the sync token and
the local commit it corresponds to. It is written through a rename, and the same
correspondence is repeated as trailers on every synchronization commit, so a
deleted state file is recoverable from `git log` alone.

---

## 8. Putting it together

### One host, with a configuration file

```sh
createdb nit

nitctl config init                       # /etc/nit/nit.yaml, mode 600
openssl rand -base64 32 > /etc/nit/sync.key && chmod 600 /etc/nit/sync.key
$EDITOR /etc/nit/nit.yaml                # database.url, policy.dir, admin_groups

nitctl config show                       # check before starting anything
nitctl migrate
nitd &
nit-worker &
```

### One host, environment only

```sh
createdb nit
export NIT_DATABASE_URL=postgres://localhost/nit
export NIT_POLICY_DIR=/etc/nit/policy
export NIT_BLOB_DIR=/var/lib/nit/blobs
export NIT_WORK_DIR=/var/lib/nit/work
export NIT_SYNC_KEY=$(openssl rand -base64 32)
export NIT_ADMIN_GROUPS=platform
export NIT_FORGE_TOKEN=ghp_…

nitctl migrate
nitd &
nit-worker &
```

### Several hosts

- **`nitd`**: any number of replicas behind a load balancer. They share the
  database, the bundle and `NIT_SYNC_KEY`. They are stateless otherwise.
- **`nit-worker`**: any number, on machines with git, disk and network access to
  the forge. They need the same database, bundle, key and **the same blob
  storage as `nitd`**.
- **The policy bundle** is deployed to every host — a git checkout updated by
  your deployment, or a mounted volume. `NIT_POLICY_RELOAD` picks up changes
  without a restart.

Migrations are **not** applied at start-up. A schema change is a deployment step
an operator decides to take: run `nitctl migrate` first, then roll out. A server
that migrates on boot will happily run half-rolled-out DDL from several replicas
at once.

### systemd

```ini
# /etc/systemd/system/nitd.service
[Unit]
Description=nit control plane
After=network-online.target postgresql.service

[Service]
Type=exec
User=nit
ExecStart=/usr/local/bin/nitd -config /etc/nit/nit.yaml
Restart=always
RestartSec=5

# nit needs its blob directory and nothing else on the filesystem.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/nit

[Install]
WantedBy=multi-user.target
```

The worker unit is the same with `ExecStart=/usr/local/bin/nit-worker` and
`ReadWritePaths=/var/lib/nit`, which covers both the shared blob directory and
its own scratch space.

Keep `/etc/nit/nit.yaml` and anything it points at readable only by the `nit`
user. With the `_file` indirections the configuration file itself holds no
secret, and the credentials can come from systemd:

```ini
LoadCredential=sync-key:/etc/nit/sync.key
```

with `security.sync_key_file: ${CREDENTIALS_DIRECTORY}/sync-key` — or from
`EnvironmentFile=` if you prefer the environment layer.

---

## 9. What is not configurable, and why

- **The policy.** Rules, groups, repositories and paths are the bundle's job.
  There is no environment variable that grants anyone access to anything.
- **The guards.** Protection of CI definitions, `.gitattributes`, `.gitmodules`,
  symlink creation and submodule pointers is on, always. Write access to a CI
  workflow is read access to the whole repository; making that switchable would
  make it switched off. Granting `admin` on those paths to a team is how you
  express the exception, in the bundle, where it is reviewed.
- **Fail-closed pushes.** A patch touching an unauthorized path is refused. The
  client can ask for stripping per push (`nit push --drop-unauthorized`); there
  is no server setting that makes stripping the default, because a developer
  must know when what landed differs from what they committed.
- **The tenant.** nit ships single-tenant; `tenant_id` is carried through the
  schema and the domain types so multi-tenancy can be added without a migration
  of everything, but nothing configures it yet.

---

## 10. Containers

`deploy/` carries a Dockerfile and Compose stacks: one for development with a
Gitea forge included, and a forge-agnostic base plus an overlay per forge for
real use. See `deploy/README.md`.

Two things matter more in a container than anywhere else:

- **The blob directory must be a volume shared between `nitd` and every
  worker.** `nitd` writes the authorized patch there and a worker reads it back;
  separate volumes produce `missing_patch` on every push.
- **Deliver secrets as files**, with `NIT_SYNC_KEY_FILE` and friends, so they
  stay out of `docker inspect`.

---

## 11. Checking a configuration

```sh
nitctl config show                  # every value, and which layer supplied it
nitctl config path                  # which file would be read
nitctl policy validate ./policy     # the bundle compiles
nitctl migrate -status              # migrations the binary carries
curl -s localhost:8080/healthz      # protocol and policy version in force
```

`/healthz` is unauthenticated and reports the policy version, which is what makes
a rolling deploy diagnosable: two replicas serving different bundles is
otherwise invisible.

`docs/VALIDATION.md` walks a fresh installation end to end and proves each
property in turn.
