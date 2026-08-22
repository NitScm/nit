# Architecture

nit is an authorization layer between developers and a git forge. Developers
never talk to the forge; they talk to nit, which decides what they may read and
write, file by file, and is the only writer of the upstream repository.

This document describes the shape of the system. `PROTOCOL.md` describes the
wire exchanges, `POLICY.md` the rule language, `CONFIGURATION.md` how to run it,
`VALIDATION.md` how to prove it works, and `DECISIONS.md` the choices that were
made and why.

---

## 1. Why the design looks like this

Two properties drive everything else.

**Read filtering means the developer's clone is not the upstream repository.**
If a developer may not read `secrets/`, those files are absent from their
workspace. Their trees differ from upstream, so their commit hashes differ, and
a local commit hash does not exist upstream. Any protocol that says "send me
your last commit and I will diff from it" is therefore broken by construction.
nit instead records a **sync point** per workspace: the upstream commit whose
filtered projection produced the developer's current state. Every exchange is
expressed relative to that point.

**An authorization decision must be explainable.** A permission system exists to
answer "who could do what, when, and why". Every decision carries the rule that
produced it and the version of the bundle it came from, and every decision is
recorded. This is why the policy engine returns a structured `Decision` rather
than a boolean, and why bundles are content-hashed.

---

## 2. Components

Four binaries, one Go module.

| Binary | Role |
| --- | --- |
| `nit` | Developer CLI: `login`, `clone`, `pull`, `push`, `status`, `whoami` |
| `nitd` | Control plane: API, identity, authorization, queue, sync points, blobs, audit |
| `nit-worker` | Executes queued tasks: clone, apply, filter, push |
| `nitctl` | Operator console: policy validation, tokens, migrations, tasks, audit, stats |
| `nit-console` | Angular web console over the same operations API (separate project) |

Three deliberate choices here:

- **One worker binary, not two.** Push and pull share the clone cache, the patch
  pipeline and the enforcement pass. Splitting them would double the deployment
  surface for no gain. Scaling is per queue (`--queues=pull`), not per binary.
- **The API comes before the console.** `nitctl` is a client of `nitd`'s HTTP
  API, not a second path to the database. The future Angular UI therefore has no
  capability `nitctl` lacks, and the API is exercised from day one.
- **One module.** The CLI and the server share `pkg/protocol` and `pkg/policy`,
  so they cannot drift, and the CLI can pre-validate a push locally before
  spending a round trip.

`nitd` performs no git operation itself. Anything that clones, applies or pushes
runs in a worker, so a slow or hostile repository can never block the API.

---

## 3. Layering

```
pkg/         pure domain — no IO, no clock, no logging, importable by anyone
  patch/     git patch model, byte-exact section splitting and filtering
  policy/    rules, patterns, compiled bundle, decision engine
    config/  YAML bundle loading, validation, content hashing
  enforce/   patch x policy -> filtered patch + report   <- the product's core
  protocol/  wire types shared by CLI, control plane and workers
  gitx/      git operations behind an interface (exec implementation)
  forge/     hosting provider abstraction (GitHub, GitLab, Gitea, plain SSH)

internal/    wiring and IO — not importable from outside the module
  server/    HTTP handlers, middleware, routing
  worker/     task handlers (push, pull)
  integration/ end-to-end tests over the whole loop
  store/     PostgreSQL repositories
  queue/     branch-partitioned queue and leases
  blob/      content-addressed blob store (filesystem, then S3)
  auth/      sessions and tokens
  synctoken/ signed, opaque sync tokens
  policyloader/ the compiled bundle, hot-reloaded
  compress/  bounded zstd for patch payloads
  taskspec/  what the control plane hands a worker
  bootstrap/ process configuration and policy reconciliation
  client/    the API client used by cmd/nit
  workspace/ a developer's local checkout: state, credentials, git
  flow/      clone, pull and push, testable without running the binary

cmd/         the four binaries
configs/     example policy bundle
testdata/    patch corpus
migrations/  SQL schema
```

The rule that keeps this honest: **`pkg/` never imports `internal/`, and `pkg/`
does no IO.** That is what makes the core fully testable without a database, a
network or even git, and it is why the bottom-up build order below is possible.

Status today: the product works end to end. A developer clones a filtered
projection, changes it, pushes it back and pulls colleagues' work, with
confidential subtrees that never reach their machine — all four binaries, over a
real git forge. Operators read the same system through `nitctl` or the web
console, both clients of one read-only operations API.

---

## 4. The two flows

### Push

```
 developer                nitd                        worker              forge
     |                      |                            |                  |
  git diff base..HEAD       |                            |                  |
  (--binary --full-index)   |                            |                  |
     |---- upload blob ---->|                            |                  |
     |---- PushRequest ---->|                            |                  |
     |                 verify sync point                 |                  |
     |                 parse patch                       |                  |
     |                 enforce.Push (fail-closed)        |                  |
     |<--- 403 + denials ---|  (if unauthorized)         |                  |
     |                 enqueue on key repo:branch        |                  |
     |<--- task id ---------|                            |                  |
     |                      |------ lease + task ------->|                  |
     |                      |                       clone at sync point     |
     |                      |                       apply filtered patch    |
     |                      |                       commit (squashed)       |
     |                      |                       rebase onto HEAD        |
     |                      |                            |--- push -------->|
     |                      |                            |  (--force-with-  |
     |                      |                            |    lease)        |
     |                      |<----- result + new sync ---|                  |
     |<--- event: done -----|                            |                  |
   record new sync point    |                            |                  |
```

Authorization happens in `nitd`, before anything is queued: an unauthorized push
costs no clone. The worker re-checks nothing about permissions — it applies a
patch that has already been through `enforce`.

### Pull

```
 developer                nitd                        worker              forge
     |---- PullRequest ---->|                            |                  |
     |                 resolve sync point                |                  |
     |<--- task id ---------|                            |                  |
     |                      |------ task -------------->|                   |
     |                      |                       clone / fetch           |
     |                      |                       diff sync..HEAD         |
     |                      |                       enforce.Pull (filter)   |
     |                      |                       store blob              |
     |                      |<----- blob + next sync ----|                  |
     |---- GET events ----->|  (long poll, held open)    |                  |
     |<--- event: ready ----|                            |                  |
     |---- GET blob ------->|                            |                  |
     |<--- filtered patch --|                            |                  |
  apply, commit sync marker |                            |                  |
  record next sync point    |                            |                  |
```

Note the direction of the last exchange. The server **never** connects to a
developer machine: laptops sit behind NAT and firewalls and are not addressable.
The CLI holds a long poll open on the task and fetches the blob itself. No
tunnels, no open ports, no agent on developer machines.

---

## 5. Concurrency: a queue, not a lock

A lock held across the whole clone-apply-push cycle is a throughput killer and a
liveness hazard: a worker that crashes leaves the branch blocked forever.

nit instead uses a **queue partitioned by `repo:branch`, with at most one task in
flight per key**:

- Serialization is a property of the queue, so no distributed lock is needed.
- A push on a busy branch is **queued, never refused** — the developer does not
  have to retry by hand.
- Workers hold a **lease with a TTL and a heartbeat**, plus a fencing token. A
  dead worker's task is re-dispatched when the lease expires; the token stops a
  zombie worker from pushing after the fact.
- **Pull tasks take no key at all** and run fully in parallel: they are read-only.
- The final `git push --force-with-lease=<branch>:<expected>` is the real
  atomicity guarantee. Only the forge can arbitrate against a change that did not
  come through nit; the queue merely avoids wasted work.
- Every request carries a client-generated `request_id`. Networks fail
  mid-push; without idempotency a retry creates a second upstream commit.

Implementation for v1: PostgreSQL with `SELECT ... FOR UPDATE SKIP LOCKED` and
`LISTEN/NOTIFY`. No Redis, no broker — Postgres is already a dependency.

---

## 6. Data

Policy lives in **files**, runtime state lives in **PostgreSQL**.

The bundle is versioned in a git repository of its own and reviewed through pull
requests: rules get history, blame and rollback, which a database row does not
have. `nitd` loads it, hot-reloads it, and stamps every decision with its
content hash.

Tables (beyond the obvious `users`, `tasks`, `notifications`):

| Table | Why it exists |
| --- | --- |
| `repositories` | Repositories under nit control, with their forge and credentials reference |
| `sync_points` | `(tenant, workspace, repo, branch) -> upstream_commit`. The heart of section 1 |
| `workspaces` | One checkout on one machine, owned by a user |
| `leases` | `(resource_key, holder, token, expires_at)` for queue serialization |
| `artifacts` | Content-addressed blobs (patches in and out), with TTL and a GC job |
| `audit_log` | Append-only: actor, action, repo, branch, path, effect, rule id, policy version, request id. `DO INSTEAD NOTHING` rules block UPDATE and DELETE at the database, so an application bug cannot rewrite history |

Two identifiers are carried from day one even though they are constant today:

- `tenant_id`, defaulting to `default`. Threading a tenant through a schema
  after the fact is one of the most expensive migrations there is.
- `workspace_id`, so a developer with a laptop and a desktop simply gets two
  sync points instead of a corrupted one.

**Identity is never taken from the patch.** The author field of a commit is free
text; anyone can forge it. Identity comes from the authenticated session, and
the commit author is verified against it or rewritten.

---

## 7. Transport

- HTTPS, JSON for metadata, binary bodies for patches. Being able to debug with
  `curl` is worth more than the milliseconds gRPC would save; gRPC stays an
  option once the contract stops moving.
- Patches are compressed with **zstd** — better ratio and faster than gzip on
  this shape of data — uploaded in chunks and addressed by the SHA-256 of their
  compressed bytes. Content addressing gives deduplication, resumability and
  client-verifiable integrity for free.
- Patches are produced with `--binary --full-index --find-renames` and with
  `--no-ext-diff --no-textconv`: a developer's diff configuration must never
  change what nit authorizes.
- A size ceiling is enforced, and `uncompressed_size` is declared so a receiver
  can refuse a decompression bomb before allocating.

---

## 8. Guards: the holes that path rules cannot see

Path rules answer "may this subject change this path?". They do not answer "does
this change hand the subject a capability the policy withholds?". A patch that
only ever touches authorized paths can still:

- edit `.github/workflows/*` — CI runs with a full checkout and can print
  anything, so write access to CI is read access to the entire repository;
- edit `.gitattributes` (clean/smudge filters run code) or `.gitmodules`;
- create a **symlink** pointing into an unreadable subtree;
- flip a file mode to `120000` (symlink) or `160000` (submodule).

`pkg/enforce` therefore requires the `admin` action for those changes, on top of
the ordinary write requirements, and `pkg/patch` exposes modes and entry kinds
precisely so this check is possible. This is the single most important security
property of the system and it is enabled by default.

A related invariant is enforced at bundle load time: **an allow rule granting
write, create or delete must also grant read.** Writing a file you cannot see
means overwriting content blind, and you cannot produce a diff against a file
absent from your workspace.

---

## 9. Build order

Bottom-up, because the valuable part is testable without infrastructure.

1. **`pkg/patch`** — the patch model. Done. Filtering is byte-exact: sections are
   re-emitted from the original bytes rather than re-serialized, so a rewritten
   patch differs from the author's only by the removal of whole files.
2. **`pkg/policy`** (+ `config`) — the decision engine. Done. Evaluation is total
   (validation happens at compile time), order-independent (deny wins, default
   deny), and attributable.
3. **`pkg/enforce`** — patch x policy. Done. Fail-closed on push, filtering on
   pull, guards on top.
4. **`pkg/gitx`, `pkg/forge`, `pkg/protocol`** — interfaces and wire types. Done.
5. **`internal/store`, `internal/queue`, `internal/blob`** — persistence, queue,
   leases, artifacts. Done. Two store backends (in-memory and PostgreSQL) share
   one conformance suite; the queue implements branch partitioning, lease expiry
   and fencing, verified against real PostgreSQL under concurrency.
6. **`internal/server`, `internal/auth`** — the API and sessions. Done. Tokens,
   sync token signing, the push authorization path, the pull queueing path, long
   polling, and the audit trail.
7. **`internal/worker`** — push and pull handlers. Done, with an end-to-end test
   that drives the HTTP API, runs a real worker, and asserts against a real git
   repository.
8. **`cmd/nit`** — the developer CLI. Done: `login`, `clone`, `pull`, `push`,
   `push --check`, `status`, `whoami`, driven end to end in the integration
   tests through the same code the binary uses.
9. **Operations API and Angular console.** Done: `/v1/admin/*` serves tasks,
   audit, statistics and the compiled policy, read-only; `nitctl stats|tasks|audit`
   and the web console in `../nit-console` are both clients of it.

---

## 10. Known open questions

- **Conflict handling on push.** A patch that no longer applies onto upstream
  HEAD needs a policy: reject and ask the developer to pull, or attempt a
  three-way merge. Rejecting is the safe default; merging silently would produce
  content the author never reviewed.
- **CLI framework.** Currently stdlib `flag`. Cobra is the obvious candidate once
  the command surface stabilizes.
- **Workspace bootstrap.** `nit clone` needs a filtered snapshot, not a diff from
  an empty tree, for repositories of any size.
- **Prior art worth studying before implementing the projection layer:**
  [Josh](https://github.com/josh-project/josh) solves filtered views with
  bidirectional commit mapping and is battle-tested. Even if none of its code is
  reused, its data model is validated by production use.
