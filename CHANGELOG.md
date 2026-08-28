# Changelog — nit

All notable changes to the Go module: engine, control plane, workers and CLIs.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project intends to follow [Semantic Versioning](https://semver.org/) from
1.0.0 onward.

Two things are versioned separately from the release number, because they are
contracts with other people's machines:

- **The wire protocol** (`protocol_version`, reported by `/healthz` and checked
  by `nit login`). A client and a server that disagree say so plainly instead of
  failing three commands later.
- **The policy bundle format.** A bundle that stops compiling after an upgrade
  is a breaking change and will be called one.

## [Unreleased]

Nothing since 0.2.0.

The sections below describe the module as a whole rather than one release —
what each package is and what it guarantees — and are kept current. The dated
sections underneath them are the changes.

### The engine (`pkg/`)

- **`pkg/patch`** — splits a patch at `diff --git` boundaries and re-emits the
  **original bytes** of surviving sections rather than round-tripping through a
  model. Keeping every section reproduces the input byte for byte, asserted by
  test.
- **`pkg/policy`** — bundle compilation and evaluation: total, order-independent,
  deny-wins, closed by default. Every decision names the rule that produced it
  and the bundle version in force. `except` for exemptions, subtree patterns and
  globs, refs, and the five actions `read | write | create | delete | admin`.
- **`pkg/enforce`** — fail-closed pushes, filtered pulls, and guards requiring
  `admin` for CI definitions, `.gitattributes`, `.gitmodules`, symlinks and
  submodules.
- **`pkg/gitx`**, **`pkg/forge`**, **`pkg/protocol`** — git execution, hosting
  drivers (HTTPS with a token, SSH with a key, local paths), and the wire types.
- **`pkg/audit`** — where a decision goes after it is made. `audit.Memory` is a
  working sink to test against, and `pkg/audit/audittest` is the conformance
  suite an implementation runs. A sink no longer has to buffer: `pkg/nitd` puts
  a bounded queue in front of whatever it is handed, so `Emit` runs off the
  request path with a context of its own — a developer cancelling their push no
  longer cancels the export of the decision already made about it. Drops at a
  full queue are counted, and the database still holds every record.
- **`pkg/store`** — `AuditQuery` pages forward with `AfterID` and `Oldest`, an
  ordering the three backends are now held to by `storetest` rather than
  agreeing on by accident. `nitctl audit export` walks a settled window with it
  and writes decision records as JSON Lines, which is how a deployment fills the
  gap left by a destination that was unreachable.

### The system (`internal/`, `cmd/`)

- **`nitd`** — authorization, the branch-partitioned queue with leases,
  heartbeats and fencing tokens, sync points, the audit trail and the operations
  API. It performs no git operation, so a slow or hostile repository cannot hold
  an API request open.
- **`nit-worker`** — clone, apply, rebase, push, and pull filtering.
- **`nit`** — `login`, `clone`, `pull`, `push`, `status`, `whoami`.
- **`nitctl`** — configuration, policy validation and explanation, migrations,
  tokens, queue and audit inspection.
- PostgreSQL storage, with a conformance suite shared by the in-memory and
  PostgreSQL backends.
- `pkg/protocol.Routes()` declares the API surface publicly. The OpenAPI
  description itself lives in the enterprise edition, which serves it alongside
  its own — and a test there compares it against this list in both directions,
  so a route added here and not described fails a build over there.
- Configuration from defaults, a file and the environment, with `_file`
  indirections for every secret and `nitctl config show` reporting where each
  value came from.
- Docker Compose stacks: a development environment with Gitea, and production
  overlays for Gitea, GitHub and GitLab.

### Provenance

- Commits published upstream are authored **and** committed as the authenticated
  user, never from the patch's `From:` line.
- They carry `Nit-User`, `Nit-Request`, `Nit-Task`, `Nit-Policy-Version`,
  `Nit-Base-Commit` and `Nit-Workspace` trailers, plus `Nit-Dropped` when
  `--drop-unauthorized` removed anything — so a commit can be traced from the
  forge alone, without database access. Counterfeit `Nit-*` lines are stripped
  from the author's message before the real trailers are appended.

### Documentation

- [`docs/`](docs) — architecture, protocol, policy language, configuration, a
  step-by-step validation walkthrough, scaling limits, and a numbered record of
  every design decision (D1–D30).
- [`examples/github-ssh/`](examples/github-ssh) — a worked deployment against
  GitHub over SSH, with an 18-case policy validation dataset that runs offline.

### Releases

Tagging `v*` publishes archives for Linux, macOS and Windows, `.deb` and `.rpm`
packages, and a checksums file, through `.goreleaser.yaml` and
`.github/workflows/release.yml`.

Two archives, split by who runs what: `nit_…` carries `nit` and `nitctl`,
`nit-server_…` carries `nitd` and `nit-worker`. Neither ships `docs/` — those
are engineering documents for people modifying nit, and a copy on disk drifts
from the site and from the binary it arrived with. README, SECURITY and the
licence travel with every artifact.

`nit`, `nitd`, `nit-worker` and `nitctl` all answer `version`, stamped at
release time and falling back to what the toolchain records — so a binary
installed with `go install` still identifies itself. A build that cannot say
which build it is turns every bug report into a guess.

Windows gets `nit` and `nitctl` only. The server components compile for it, but
a worker's job is clone, apply, rebase and push through a real git and that has
not been exercised there; a published binary is a claim, and this one is not
made yet.

### Known limits

Documented rather than hidden. The arithmetic is in
[`docs/SCALING.md`](docs/SCALING.md).

- Pushes serialize per branch and every task clones from scratch, so a single
  very busy branch is the case nit is worst at.
- A pull is generated per user, so read load scales with the number of
  developers rather than with the number of changes.
- `audit_log` is not partitioned. It is pruned — `nitctl audit prune` exists and
  counts before it deletes — but a very large trail still lives in one table.
- The blob store has a filesystem backend and an in-memory one. The filesystem
  backend is the only durable one, so `nitd` and every worker must share one
  volume.
- The claim path takes a deployment-wide advisory lock.
- **A hosted control plane is not possible yet.** Authorizing a push means
  decompressing the patch and reading the paths inside it, so a `nitd` somebody
  else runs is a `nitd` that sees your source. Splitting it into a customer-run
  edge and a hosted control plane is the remaining work; nothing in this module
  claims otherwise.

---

## [0.2.0] - 2026-08-28

Multi-tenancy stops being a column and becomes a thing the code resolves, and
five seams open so an edition can be built around this one rather than into it.

### Added

- **A request's tenant comes from its token.** `internal/server` reads it from
  the authenticated principal — which reads it from the session row, not from a
  field fixed at start-up — and puts it in the request context. The worker takes
  it from the task it is running. A process is no longer one customer.
- **PostgreSQL row-level security, as a second layer.** Every tenant-scoped
  table carries a policy with `USING` and `WITH CHECK`, under
  **`FORCE ROW LEVEL SECURITY`** — without which the table's owner, who is
  whoever ran the migrations, bypasses every policy and the whole thing is
  decoration. One pool hook stamps the connection from the request's context as
  it is acquired, so a forgotten tenant reads nothing instead of reading
  somebody else's rows. `Store.RowSecurityEnforced` asks the database whether
  any of it applies, because the failure mode of RLS is silence.

  ⚠️ MySQL and MariaDB have no row-level security. The context plus
  `WHERE tenant_id = ?` is the whole defence there; the empty tenant fails
  closed on both, but the second layer is PostgreSQL-only.
- **One policy bundle per tenant**, behind a registry with its own version and
  reload cadence per tenant.
- **Per-tenant signing keys.** Sync tokens are `st2`, signed with a key derived
  per tenant by HKDF, so a token minted for one tenant cannot verify for another
  even if a routing bug hands it over. `st1` still verifies, and only for the
  default tenant.
- **A per-tenant blob namespace**, with a fallback to the previous flat layout
  so an upgrade does not lose a patch that was already in flight.
- **Tenant-scoped operator access.**
- **MySQL 8.4 and MariaDB 11** as store backends, held to the same conformance
  suite as PostgreSQL rather than trusted to behave alike.
- **`pkg/nitd`** — a server can be assembled from outside the module. It takes
  the only exception to the no-IO rule in `pkg/`, and `pkg/nitd/boundary_test.go`
  fails if anything but `cmd/` imports it.
- **Seams, each with a conformance suite**: `pkg/store`, `pkg/blob`, the pull
  cache and the audit sink. An out-of-tree implementation runs the same suite the
  in-tree one does.
- **An in-memory blob store**, for tests and for an assembly that wants one.
- **Audit retention**, as an operator command that counts before it deletes —
  a tool that destroys evidence without being able to say how much is not one
  anybody should run.
- **Cursor paging and JSON Lines export** for the trail, over an ordering the
  three backends are now held to.
- **A bundle records which version was in force**, and a bundle can say what it
  is supposed to do and be checked against it.
- **The `idp:` group namespace** — a bundle may name a directory group without
  the deployment having a directory, so it compiles in CI with no credential.

### Changed

- A mirror is kept per repository instead of cloning per task, under an LRU disk
  budget that never evicts a mirror in use.
- A pull projection is shared between users with identical read rights.
- Branch exclusion uses a unique constraint rather than a global lock.
- A waiting client is woken rather than asking twice a second.
- `policy explain` says what a change does to people rather than to the file.

### Removed

- **The OpenAPI description and the Swagger viewer.** Both now live in the
  enterprise edition, which serves this module's description alongside its own.
  `pkg/protocol.Routes()` declares the surface publicly so the parity test stays
  real across two repositories — verified in both directions.

### Fixed

- A bare `-until` meant the start of that day rather than the end of it.
- A rebase was performed without a committer, and every failure was reported as
  a conflict.
- The server-side `nitctl` commands had no `-config` flag.
- The audit trail refused silently.

### Security

- **The tenant now comes from one place.** `server.tenantOf` fell back to the
  default tenant when no principal was in the context. Correct in the
  single-tenant deployment everybody runs, and a cross-tenant read on the first
  day a deployment has two. It reads the context the store reads, and an absent
  tenant is the empty tenant, which matches nothing on either backend.
- Row-level security, above, is the second layer that makes forgetting a failed
  query rather than a disclosure.

## [0.1.0-lts] - 2026-08-23

The community edition before the enterprise extraction began: per-file read and
write control, fail-closed pushes, filtered pulls, guards, an attributable audit
trail, commit provenance, policy as code, and releases for Linux, macOS and
Windows.

Marked LTS because it is the line the enterprise edition is built *around*
rather than *into*.

---

## How these sections are written

```
### Added
### Changed
### Deprecated
### Removed
### Fixed
### Security
```

**Security** entries name the class of problem and its impact, following
[SECURITY.md](SECURITY.md), and credit the reporter unless they prefer
otherwise.

Any change to the wire protocol or the policy bundle format gets its own
paragraph saying what breaks and what to do about it — not a bullet among
others.
