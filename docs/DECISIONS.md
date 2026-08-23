# Decisions

Short records of the choices that shape the system, and the reasoning behind
them. A decision that turns out to be wrong should be superseded here rather
than quietly changed in code.

---

## D1 — An unauthorized push is rejected, not stripped

**Decision.** If a patch touches any path its author may not write, the whole
push is refused with a report naming every offending path and the rule that
refused it. Stripping is available as `nit push --drop-unauthorized`, never by
default.

**Why.** Silently dropping files means the author believes their change landed,
upstream receives a partial commit that may not build, and the next pull
restores the upstream version of the dropped files — quietly reverting their
work. An authorization system that alters the user's intent without saying so
causes more damage than it prevents.

**Consequence.** Every section is evaluated before answering, so the developer
gets the complete list in one round trip.

---

## D2 — Read filtering ships in v1, with sync points

**Decision.** Both write control and read filtering are in the first production
release. Sync points and the opaque sync token exist from the first line of the
protocol.

**Why.** Read filtering — hiding files *inside* a repository — is the capability
no forge offers, and it is what the product is for. Shipping write control alone
would be a feature already approximated by a `pre-receive` hook.

**Consequence.** The developer's clone is a filtered projection, so local commit
hashes do not exist upstream and cannot be used as a diff base. Everything is
expressed relative to a server-recorded sync point (see `PROTOCOL.md` §1). This
is the single largest source of complexity in the system, and it was accepted
knowingly.

**Prior art to study before implementing the projection layer:**
[Josh](https://github.com/josh-project/josh) solves filtered views with
bidirectional commit mapping, in production.

---

## D3 — Subtrees first, scattered files supported

**Decision.** The pattern language supports both an explicit subtree form
(`secrets/`) and full globs (`**/*.env`), with no inference between them.

**Why.** Confidential areas are usually subtrees, and subtrees are what make a
future optimization possible (sparse-checkout and partial clone can serve a
subtree-shaped policy with native git). Scattered files still have to work —
credentials do not respect directory boundaries — so globs are first class.

**Consequence.** A trailing slash is the only marker for a subtree. Nothing is
guessed from a dot in the last segment.

---

## D4 — A push lands as a single squashed commit

**Decision.** A submission produces exactly one upstream commit, with the
message the client supplied.

**Why.** Preserving a series would require mapping every local commit hash to a
rewritten upstream one, and maintaining that mapping forever, for no benefit a
squashed commit with a good message does not already give. Local history stays
local, where it is useful.

**Consequence.** `git format-patch` / `git am` are not used; the client sends a
flat `git diff`. The patch parser still tolerates a preamble so a
`format-patch` payload does not break it.

---

## D5 — One workspace per machine, keyed from day one

**Decision.** nit ships assuming one workspace per developer per repository, but
`workspace_id` is part of the sync point key everywhere.

**Why.** Retrofitting the key later would mean migrating every stored sync point
without knowing which machine each one belonged to. Carrying an identifier that
happens to be constant costs nothing.

**Consequence.** A developer with a laptop and a desktop gets two workspaces with
independent sync points, rather than one corrupted one — supported the day the
CLI starts issuing distinct workspace ids.

---

## D6 — The forge is abstracted

**Decision.** Everything provider-specific lives behind `pkg/forge`. GitHub Cloud,
GitHub Enterprise, GitLab, Gitea and plain SSH remotes are all drivers.

**Why.** "Adapted to every cloud" was a product requirement, and a provider
leaking into the queue or the policy engine is very expensive to undo.

**Consequence.** The interface stays small on purpose: authenticate a clone, read
a branch tip without cloning, resolve the default branch. nit does not manage
issues, reviews or releases.

---

## D7 — Single tenant now, multi-tenant schema from the start

**Decision.** `tenant_id` is present in the domain types and will be present in
every table, defaulting to `default`.

**Why.** Threading a tenant through a schema, an API and a cache after the fact
is one of the most expensive migrations there is, and it is nearly impossible to
do safely once real data exists.

**Consequence.** No tenant resolution logic ships now; the column and the field
are simply always `default`.

---

## D8 — Patch filtering is byte-exact

**Decision.** `pkg/patch` splits the raw patch at `diff --git` boundaries and
re-emits the **original bytes** of surviving sections, rather than parsing into a
model and re-serializing.

**Why.** nit rewrites patches on users' behalf. A rewritten patch that differs
from the author's intent in any way other than the removal of whole files is a
bug. Round-tripping through a model loses information on binary chunks, unusual
line endings and non-UTF-8 paths.

**Consequence.** Keeping every section reproduces the input byte for byte — this
is asserted by a test. Metadata extraction still uses `go-gitdiff`; only the
rendering path is bytes.

**Safety of the split:** inside a hunk every line is prefixed by `' '`, `'+'`,
`'-'` or `'\'`, and binary chunks are base85 without spaces, so no content line
can be mistaken for a section header.

---

## D9 — Policy evaluation is total, order-independent and attributable

**Decision.** Bundles are compiled and fully validated at load time; `Evaluate`
returns a `Decision` and no error. All matching rules are considered, deny wins,
default is deny. Every decision names the rule and the bundle version.

**Why.** A malformed rule must not be able to fail open at request time. A policy
whose meaning changes when two lines are swapped cannot be reviewed. And "why did
this push pass on March 12?" is the question a permission system exists to
answer.

**Consequence.** `except` had to be added to the rule language, because with
deny-wins an exemption cannot be expressed as an allow (see `POLICY.md` §5).

---

## D10 — Guards, and the `admin` action

**Decision.** Changes to CI definitions, `.gitattributes`, `.gitmodules`, symlink
creation and submodule pointers require the `admin` action on top of ordinary
write permission. Enabled by default.

**Why.** Write access to a CI workflow is read access to the entire repository: a
job runs with a full checkout and can print anything. This is the largest hole in
a file-path-based permission model, and closing it is most of the security value
of the product.

**Consequence.** `pkg/patch` must expose file modes and entry kinds, not just
paths — which is why the model distinguishes blob, symlink and submodule.

---

## D11 — A branch-partitioned queue, not a held lock

**Decision.** Serialization comes from a queue with at most one task in flight
per `repo:branch`, plus worker leases with TTL and fencing tokens. The real
atomicity guarantee is `git push --force-with-lease`.

**Why.** A lock held across clone-apply-push blocks a branch for minutes and
strands it forever if a worker crashes. A queue serializes just as well, never
refuses a developer, and recovers from worker death by itself. And only the forge
can arbitrate against a change that did not come through nit at all.

**Consequence.** Pull tasks take no key and run fully in parallel. Postgres
(`FOR UPDATE SKIP LOCKED` + `LISTEN/NOTIFY`) is enough; no broker is needed.

---

## D12 — The client fetches; the server never calls back

**Decision.** When a pull is ready, the CLI is holding a long poll on the task's
event stream and then fetches the blob itself.

**Why.** A developer machine sits behind NAT and a firewall and is not
addressable. A design where "the server notifies the developer's machine" needs
tunnels or an agent, for no benefit.

**Consequence.** No inbound connectivity is ever required on developer machines.
Plain polling is the documented fallback.

---

## D13 — Policy lives in files, runtime state in PostgreSQL

**Decision.** The bundle is YAML in its own git repository, loaded and
hot-reloaded by `nitd`. The database holds tasks, sync points, artifacts, leases
and the audit log.

**Why.** Authorization rules should be reviewed like code — pull request,
history, blame, rollback, CI validation via `nitctl policy validate`. A database
row has none of that, and "who granted this access and when?" is the first
question after an incident.

**Consequence.** The admin UI edits rules by proposing changes to the bundle, not
by writing rows. That is a real constraint on the future web interface, and it is
accepted.

---

## D14 — One Go module, four binaries

**Decision.** A single module `github.com/NitScm/nit` with
`cmd/{nit,nitd,nit-worker,nitctl}`. One worker binary handling both queues.

**Why.** The CLI and the server must share the wire types and the policy engine;
separate modules make that impossible without publishing a third. Two worker
binaries would double the deployment surface to share 90% of their code.

**Consequence.** `nitctl` is a client of the API, not a second path to the
database — so the API is exercised from day one and the future Angular UI has no
capability `nitctl` lacks.

---

## D15 — Claims are serialized by an advisory lock

**Decision.** The PostgreSQL dequeue takes `pg_advisory_xact_lock` for the
duration of the claim transaction, in addition to `FOR UPDATE SKIP LOCKED`.

**Why.** `SKIP LOCKED` excludes workers competing for the *same row*. It does
nothing for two workers picking two *different* rows of the same branch: under
READ COMMITTED neither transaction sees the other's uncommitted UPDATE, both
pass the "is this branch busy?" test, and both start pushing to the same branch.

This was not theory. The first implementation looked correct, passed every
sequential test, and failed the concurrent one immediately: eight workers, four
branches, seven or eight claims succeeded instead of four, with two workers
holding the same branch. Only a concurrency test against real PostgreSQL could
find it — the in-memory store passes either way, because its mutex hides the
difference.

The obvious fix, one advisory lock per partition, was tried and rejected: taken
while the transaction scans candidate rows, it lets a worker hold a lock on a
branch it does not end up claiming, blocking a worker that would have. That
trades a correctness problem for an intermittent liveness one — which showed up
as a flaky test before it could have shown up in production.

**Consequence.** Claims serialize; the work they dispatch does not. A claim is
one indexed query and a short UPDATE, while the task it hands out runs for
minutes, fully in parallel with every other task. If the claim rate ever becomes
the bottleneck, per-partition locking is the escape hatch, and it requires
narrowing the candidate set to a single row *before* taking the lock.

## D16 — One conformance suite, every backend

**Decision.** `internal/store/storetest` holds the tests; the in-memory and
PostgreSQL stores both run them. Neither has meaningful tests of its own.

**Why.** Two implementations of a subtle contract drift silently. Writing the
contract once, as executable tests, is what keeps the in-memory store honest
enough to develop against and the SQL store honest enough to deploy.

**Consequence.** The unit test run needs no infrastructure — the PostgreSQL
suite skips unless `NIT_TEST_POSTGRES` is set — but CI must set it. A green
`make test` without `make test-postgres` proves less than it appears to.

---

## D17 — Migrations are a command, not a start-up side effect

**Decision.** `nitctl migrate` applies the schema. `nitd` never migrates on
boot. Migration files are embedded in the binary.

**Why.** A schema change is a deployment step an operator decides to take. A
server that migrates on boot will happily run half-rolled-out DDL from several
replicas at once, and leaves nobody able to say which version of the schema a
given release expects.

**Consequence.** Each migration runs in a transaction with the row recording it,
so a failure leaves neither partial schema nor a false record of success. A
session advisory lock serializes concurrent operators.

---

## D18 — Patches are fetched through their task, not from a blob endpoint

**Decision.** Uploads go to `POST /v1/blobs`; downloads go to
`GET /v1/tasks/{id}/patch`. There is no `GET /v1/blobs/{digest}`.

**Why.** Authorization for a download has to be a question with an answer.
"Does this task belong to you?" is one. "Do you know this digest?" is not: it
makes an unguessable identifier the only thing standing between a filtered patch
and the people it was filtered *for*, and a content-addressed store hands the
same digest to everyone whose filtered patch happens to be identical.

Uploading needs no such check — writing bytes into a content-addressed store
reveals nothing and grants nothing.

**Consequence.** A task carries the digest of the patch it produced, and the
download path checks ownership of the task. Tasks are 404 rather than 403 for
strangers: a caller has no business learning which task ids exist.

---

## D19 — Tokens are issued by an operator, and identity never comes from the patch

**Decision.** `nitctl token create -user alice` mints a token; the plaintext is
shown once and only its SHA-256 is stored. The commit author recorded upstream
comes from the authenticated session.

**Why.** The author field of a git commit is free text. A system that trusted it
would let anyone attribute a change to a colleague — including a colleague with
wider access — which would make the whole audit trail fiction.

Operator-issued tokens rather than a self-service login keep the trust chain
short enough to reason about while the rest of the system is being built. A
device flow against the forge is the obvious next step and changes nothing
below this line.

**Consequence.** Authentication failures are distinguished — expired, revoked,
disabled, not in the policy bundle — because the right action differs for each,
and "unauthorized" for all of them is an operational problem, not a security
feature. Nothing is leaked by the distinction: all of them require presenting a
valid token first, except the two that describe the token itself.

The rest of the provenance — which request, which bundle version, whether
anything was dropped — is recorded on the commit as trailers (D29).

---

## D20 — Sync tokens are signed

**Decision.** A sync token is `HMAC-SHA256` over its payload, with a key shared
by every `nitd` replica (`NIT_SYNC_KEY`, at least 32 bytes, no default and no
generated fallback).

**Why.** The token is the client's claim about which upstream commit its patch
was computed against, and the server applies the patch on top of whatever it
names. Unsigned, a client could name any base it liked — including one whose
projection it was never entitled to see.

Three checks run on every push, and each closes a different hole: the signature
proves the server minted the token; `Matches` proves it was minted for *this*
workspace, repository and branch, so one cannot be replayed on another; and the
comparison against the stored sync point proves it is still current.

**Consequence.** A generated-at-start-up key would differ between replicas and
across restarts, silently invalidating every client's token — which looks to a
developer like their workspace mysteriously needing a full resynchronization.
Hence: required, explicit, no fallback.

---

## D21 — A push advances the sync point only when the workspace stays faithful

**Decision.** After a push lands, the worker issues a new sync token only if
nothing was rebased onto and nothing was stripped. Otherwise it returns no
token, and the developer has to pull.

**Why.** The sync point means "the upstream commit whose filtered projection is
what this workspace holds". That statement survives a push that landed alone and
whole: the workspace is the old projection plus the author's own changes, which
is exactly the new projection.

It stops being true the moment something else is in the new commit. If the
branch moved and the change was rebased, upstream now contains a colleague's
work the workspace has never seen. If strip mode dropped sections, what landed
is not what the author committed. In both cases claiming the workspace is
projected from the new commit would make every later diff compute against a
base the developer does not have.

**Consequence.** "Your push landed, now pull" is a normal outcome, not an error.
The response says so by omitting the token rather than by failing.

---

## D22 — A pull derives its base from the client's token, not the stored record

**Decision.** `resolvePullBase` reads the commit out of the (signed) sync token
the client presents. The stored sync point is not consulted.

**Why.** Found while writing the worker, and it was a real deadlock. The two
diverge whenever a pull is delivered and the client fails to apply it — a crash,
a full disk, an interrupted command. Diffing from the stored point would then
hand the client a patch assuming changes it never received; refusing the request
as stale would leave it with *no* way to catch up, because every later pull
would be refused for the same reason.

Deriving the diff from where the client says it is makes the case
self-correcting. The signature is what makes trusting that claim safe: the
server minted the token, for this workspace, on this branch, at that commit.

**Consequence.** The stored sync point keeps one job — gating pushes, where
applying to the wrong base is dangerous — and loses the other. A push from a
stale workspace is still refused with `stale_sync_point`, and a pull is how the
client recovers.

---

## D23 — Bookkeeping never fails a push that already landed

**Decision.** Once `git push` has succeeded, no later error fails the task. A
sync point that cannot be advanced, an audit record that cannot be written: both
are logged loudly and the task still succeeds.

**Why.** The queue retries failed tasks. A task that failed *after* publishing
would be retried and would try to publish again — at best a no-op, at worst a
duplicate commit. The bookkeeping is recoverable; the push is not.

**Consequence.** Returning no sync token is the fallback for every bookkeeping
failure, which routes the client through a pull that reconciles whatever the
disagreement was.

---

## D24 — A pull replays local work instead of merging into it

**Decision.** `nit pull` applies the delivered patch to the *synchronization
commit*, not to HEAD, and then replays the developer's own commits onto the
result. That is `git pull --rebase`.

**Why.** A checkout's history is a synchronization commit followed by whatever
the developer has committed since. Applying a delivered patch on top of that
would bury the new sync point in the middle of local history, leaving nothing to
diff the next push from.

The rebase also fixes a case that would otherwise be unresolvable, and it is the
common one: after a push that the server rebased, the developer's own change is
already upstream. It comes back in the next pull, and applying it on top of the
local commit that already contains it conflicts — with itself. Replayed instead,
git recognizes it as already applied and drops it, exactly as it does after your
pull request is merged.

**Consequence.** A pull refuses to run against a dirty working tree, for the
same reason git does: a merge into uncommitted work cannot be untangled
afterwards. A genuine conflict between local commits and upstream aborts the
rebase and says so.

---

## D25 — The CLI accepts flags in any position

**Decision.** `cmd/nit` reorders arguments before handing them to the `flag`
package, so `nit clone backend-api -branch main` works.

**Why.** The standard library stops parsing at the first non-flag argument,
which means that command silently ignores `-branch`. Nobody writes commands the
other way round — git, gh and docker all accept flags after positionals — and an
option that is quietly dropped is far worse than one that errors: the command
appears to succeed while doing something else.

Found by running the CLI rather than by reading it, which is the argument for
having done so.

**Consequence.** `--` still ends flag parsing, so a path beginning with a dash
stays a path. The command layer remains stdlib `flag`; the surface is small and
stable, and Cobra would buy shell completion at the cost of a dependency tree —
a swap that touches one file if it ever becomes worth it.

---

## D26 — A workspace stores a token and a local base, not a commit map

**Decision.** `.nit/state.json` holds the opaque sync token plus `local_base`,
the local commit it corresponds to. Nothing else about the correspondence
between local and upstream history is kept.

**Why.** A push has to be diffed from somewhere, and the developer's commits are
not upstream commits — the local tree is a filtered projection, so its hashes
exist nowhere on the forge. Two values are enough: the token says where upstream
is, `local_base` says which local commit that is. A full mapping between the two
histories would have to be maintained forever and would be wrong after the first
rebase.

**Consequence.** The synchronization commit also carries the correspondence as
trailers, so a deleted state file is recoverable from `git log` alone. The state
file is written through a rename, because a truncated one leaves a checkout
unable to say where it is.

---

## D27 — The operations API is read-only

**Decision.** `/v1/admin/*` serves tasks, audit records, statistics and the
compiled policy. It changes nothing. `nitctl` and the web console are both
clients of it.

**Why.** Authorization rules are authored in files, reviewed through pull
requests, and rolled back like code (D13). A console that could edit rules would
be a second path to the same decisions with none of those properties — and it
would be the path people used, because it is the convenient one.

Making both clients read the same endpoints also keeps them honest: the API is
exercised by `nitctl` from day one, so the web UI can never need a capability
the command line lacks.

**Consequence.** Granting access is a pull request against the bundle, not a
click. That is slower on purpose. A future console can propose a change to the
bundle — a pull request opened on the policy repository — but it must not write
rules directly.

---

## D28 — Operator access is configured on the server, not in the bundle

**Decision.** `NIT_ADMIN_GROUPS` names the groups allowed to read the operations
API. It is server configuration, not a policy rule.

**Why.** The console is the tool for diagnosing a broken bundle. Putting the
permission to use it *inside* the bundle would make that tool depend on the
thing it exists to debug: a rule change that locked an operator out would also
remove their way of finding out why.

**Consequence.** A non-member gets 404, not 403 — the existence of an operations
API is not something an ordinary developer needs confirmed. And there is
deliberately no CORS wildcard: the API is bearer-authenticated, so `*` would let
any page a developer visits call it with their credentials.

---

## D29 — The upstream commit carries its own provenance, and the author's message is sanitized

**Decision.** The commit nit publishes carries git trailers — `Nit-User`,
`Nit-Request`, `Nit-Task`, `Nit-Policy-Version`, `Nit-Base-Commit`,
`Nit-Workspace`, and `Nit-Dropped` in strip mode. Before they are appended,
every line matching `^\s*Nit-[A-Za-z0-9-]*\s*:` is stripped from the author's
message.

**Why.** Identity already survived the trip: the commit is authored and
committed as the authenticated user (D19). Everything else about the decision
lived only in the audit table, so a commit seen on the forge could not be traced
back to the request that produced it, to the bundle version that authorized it,
or — worst — to the fact that files had been dropped from it. The forge is the
one record an auditor, a reviewer or a compliance export can read without
database access.

The sanitization is not decoration. A commit message is free text that lands in
the same commit as the real trailers, so `-m $'Fix it\n\nNit-User: bob'` would
attribute a change to a colleague in exactly the record that leaves the
database. The whole `Nit-` namespace is matched rather than the seven keys, so a
trailer added later is protected the day it is added.

**Consequence.** A commit body cannot contain a line beginning `Nit-…:`; the
handful of legitimate messages that would have is a price worth paying for an
attribution that cannot be forged. A message that was *only* forged trailers
gets a generated subject rather than a commit whose first line is a trailer.
When the author's message already ends in a trailer block, nit's lines join it —
git reads only the last block, and starting a new one would stop the forges
rendering their `Co-authored-by`.

`Nit-Dropped` is a count, not a list: the paths are queryable in the audit
trail, and an unbounded commit message is a poor place for them.

---

## D30 — SSH is configured by passing git a command, not by nit managing keys

**Decision.** `git.ssh_command` (`NIT_GIT_SSH_COMMAND`) is handed to every git
invocation as `GIT_SSH_COMMAND`. There is no `ssh_key` setting, and
`forge.Credentials` carries no key path.

**Why.** A driver's job is to return a URL, and a key cannot be expressed in
one — so a key path on `Credentials` could never have taken effect where it sat.
It also had the wrong cardinality: `Credentials` is one per worker process,
while a deploy key, the tightest scoping a forge offers, is valid for exactly
one repository. A single process-wide key would have pushed deployments toward
the looser machine-user pattern.

Beyond that, a key path covers only the simplest case. Agents, `ProxyJump`,
per-host keys, non-standard ports and certificate authentication all remain
git's business, so nit would own a thin slice and every deployment would still
need the escape hatch — two half-overlapping ways to configure one thing, with a
precedence nobody would remember.

The passthrough earns its place for a different reason: a deployment can keep
its configuration in one file and read it back with `nitctl config show`,
instead of splitting it between `nit.yaml` and a systemd `Environment=` line.

**Consequence.** A configured value **overrides** an inherited
`GIT_SSH_COMMAND`, because a setting that silently did nothing on a host that
exports one would be worse than no setting at all. Left empty, nothing is set
and a machine configured through `~/.ssh/config` keeps working.

nit does not expand variables inside configuration values, so a path only the
service manager knows — systemd's `%d` — has to arrive through the environment
layer rather than through the file.

If per-repository SSH keys ever matter, their home is the repository entry in
the policy bundle, not process configuration.

---

## D31 — The audit trail refuses out loud

**Decision.** `audit_log` is protected by a trigger that raises on `UPDATE`,
`DELETE` and `TRUNCATE`, replacing the `DO INSTEAD NOTHING` rewrite rules of
migration 0001.

**Why.** The rules did stop an application bug from rewriting history, and they
were silent about it. A purge answered `DELETE 0`, exited zero, and left every
row in place — so an operator running a retention job was told it had worked.
That was verified against a live database, not inferred.

A guarantee that lies about being enforced is worse than one that is merely
enforced, because the person it misleads is the one trying to do the right
thing.

**Consequence.** Emptying the table is now a deliberate act: disable the
trigger, delete, enable it. That is the behaviour a purge should have had all
along.

`TRUNCATE` needs its own statement-level trigger, because a row trigger does
not see one at all — without it the strongest guarantee in the schema would be
one word away from being bypassed by accident. The store conformance harness
disables exactly that trigger to reset a test database, which doubles as proof
that the escape hatch works.

It is also the portable form. PostgreSQL rewrite rules have no equivalent in
MySQL or MariaDB, while all three have triggers, so this removes an obstacle to
a second backend rather than adding one.

---

## D32 — A shared mirror per repository, a private worktree per task

**Decision.** `internal/gitcache` keeps a bare mirror per repository in
`storage.work_dir`, fetches it before each task, and cuts a detached
`git worktree` per task. Worktrees are never reused. Supersedes the
clone-per-task of D11's era.

**Why.** A task paid for a whole clone. On a large repository that dominated
every other cost, and because pushes to one branch are serialized, the clone
time *was* that branch's throughput — which is what put a big monorepo at a
dozen pushes an hour, and made it the customer profile nit was worst at while
being the one that needs it most.

**Why it waited.** Sharing a clone between tasks that apply patches and rebase
is also a way for one task's leftover state to corrupt another's, and the
failure is not a broken build — it is a wrong commit on the forge under a
developer's name. The trade was not worth taking before the apply path was
settled.

**Consequences**, each of which is a thing that can go wrong and is now a test:

A worktree is never reused. A task that dies mid-apply leaves a dirty one, and
removal is `--force` precisely because that is the case it exists for.

A mirror that cannot be *opened* or cannot be *fetched* is rebuilt. Both fail on
a corrupt mirror and the first is easy to miss: an interrupted pack write leaves
`objects/` in a state where `git init --bare` itself refuses, so a rebuild that
only triggered on a failed fetch would never run.

No credential reaches the disk. A mirror is created empty and filled by a fetch
whose URL is passed per call, and the push targets that URL rather than a
configured remote. The clone this replaced also wrote the authenticated URL into
a config — but deleted it seconds later, where a mirror would keep it for as
long as the worker lives.

Mirrors are locked per repository, not globally: git serializes ref updates per
repository, so two tasks fetching one mirror race while two tasks on different
mirrors do not.

**A new deployment constraint.** A `work_dir` belongs to one worker process.
The lock is in-process, so two processes sharing a directory would race on the
same mirror. Recorded in `docs/SCALING.md` §3.

---

## D33 — Mirrors are evicted on a disk budget, never while in use

**Decision.** `storage.mirror_budget_bytes` (default 20 GiB) caps what the
mirrors of D32 may occupy. Past it, `internal/gitcache` removes the least
recently used mirrors until the rest fit. A mirror with a live worktree is never
a candidate. Zero disables eviction.

**Why.** D32 traded a cost that was paid and released for one that is paid and
kept. A clone returned its disk when its task ended; a mirror does not, and a
worker that has seen enough repositories fills its volume — including with
mirrors of repositories nobody has pushed to in a year. Eviction costs one slow
task when a repository comes back. A full disk fails every task, on every
repository, and needs a human.

**Ordered by a marker file**, not by directory timestamps. Git rewrites parts of
a repository during a fetch and leaves others untouched for months, so a
directory's mtime records when git last happened to write rather than when nit
last needed the repository. A mirror with no marker — one left by a version
before this — sorts oldest, so it goes first rather than last.

**In use is decided twice.** An in-process counter covers this worker, including
the window between creating a task directory and git registering a worktree in
it. Git's own `worktrees/` metadata covers every other process: D32 said a
`work_dir` belongs to one worker, and a second worker that ignored the rule
would otherwise delete the objects the first is reading. Config is a place
people make mistakes; this one would have corrupted a running task rather than
failing it.

The cost of that choice: a task killed hard leaves both its worktree entry and
its directory, and pins one mirror until an operator clears the work directory.
That is the safe direction — the alternative is evicting a repository out from
under a task that is merely slow.

**Swept after a task releases its worktree, at most once a minute.** Measuring
the mirrors means walking them, and doing that after every task on a large
repository would cost more than eviction saves. Nothing runs on a timer, so
there is no background goroutine to own, shut down, or leak.

**What this is not.** It is not the answer to a volume too small for the
repositories in play. A budget below the size of one large repository evicts it
after every task and clones it again on the next; the setting bounds a working
set, it does not conjure disk.
