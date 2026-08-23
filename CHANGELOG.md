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

Nothing has been released yet. The project works end to end and has not been
tagged; there is no upgrade path to promise until there is a version to upgrade
from.

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
- An OpenAPI 3.0.3 description served at `/openapi.yaml`, kept honest by tests
  asserting every route is described and every description has a route.
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
- `audit_log` is not partitioned and nothing prunes it. Because the table is
  append-only at the database level, a plain `DELETE` reports success and
  removes nothing.
- The blob store has only a filesystem backend, so `nitd` and every worker must
  share one volume.
- Multi-tenancy is present in the schema and the store API, but every process
  serves a single tenant.
- The claim path takes a deployment-wide advisory lock.

---

## Release notes will start here

The first tagged release will add sections in this shape:

```
## [0.1.0] - YYYY-MM-DD

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
