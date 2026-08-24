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

---

## D34 — MySQL and MariaDB as a second backend, in the community edition

**Decision.** `internal/store/mysql` implements the store on MySQL 8.0.16+ and
MariaDB 10.6+, with its own migration set under `migrations/mysql/`. PostgreSQL
remains the recommended backend. The DSN selects the engine; nothing names it.

**Why at all.** "Which database do you support?" is an adoption question before
it is a technical one. A team already running MariaDB will not stand up
PostgreSQL to evaluate a tool, and the evaluation is where nit either gets
adopted or does not.

**Why community and not commercial.** It was briefly the other way. The
argument that changed it: a backend restriction is not a feature, it is an
obstacle placed in the way of trying the product. Nothing about running on
MariaDB costs more to support than running on PostgreSQL, so charging for it
would be charging for the absence of a limitation.

**What makes a second backend safe to offer.** `pkg/store/storetest`, and only
that. The queue semantics — partition exclusion, lease expiry, fencing tokens,
idempotent submission — are subtle enough that a prose description is not a
specification. One suite runs against all three implementations in CI. Without
it this decision would be irresponsible rather than merely expensive.

**Four things the schema could not carry across**, each recorded where it
occurs rather than smoothed over:

*Migrations are not transactional.* MySQL and MariaDB commit implicitly at each
DDL statement. A migration that fails halfway leaves the statements before it
applied and no record of the version. The migrator therefore executes one
statement at a time so the error names which — and `docs/CONFIGURATION.md` tells
an operator to back up before migrating this backend, which it does not say for
the other.

*TRUNCATE cannot be intercepted.* D31 gave `audit_log` a trigger that raises,
and PostgreSQL has a statement-level `BEFORE TRUNCATE` to cover the one word
that would otherwise bypass a row trigger. Neither engine here fires any
trigger on TRUNCATE. `UPDATE` and `DELETE` are refused; TRUNCATE is held off by
a privilege instead, since it requires `DROP`. A test asserts the gap so it
cannot be forgotten while it is open, and will fail loudly if an engine ever
closes it.

*There are no partial indexes.* PostgreSQL indexes only queued tasks and only
live sessions. Here every row ever written is indexed, so the dispatch index
grows with history rather than with the backlog. That is the strongest argument
for pruning finished tasks on this backend.

*Collations had to be pinned.* Every table is `utf8mb4_bin`. The defaults do not
match PostgreSQL's byte comparison and do not even agree with each other —
`utf8mb4_0900_ai_ci` on MySQL, `utf8mb4_uca1400_ai_ci` on recent MariaDB. Left
alone, "Maya" and "maya" would collide in `users_policy_id_unique` on one
backend and not on the other: a difference in an authorization join.

**And one thing found only by running it.** The conformance suite deadlocked
under concurrency: `Complete` locks a task by primary key, while the expired-
lease sweep scanned `idx_tasks_lease_expiry` and took the secondary index lock
first. Two orders, one cycle, three tasks never completed. The sweep now reads
ids and updates by primary key so both paths lock in the same order, and
mutating operations retry once InnoDB names them the victim — a deadlock rolls
the transaction back entirely, so repeating it is always safe. PostgreSQL never
exhibited this, which is the point: a second backend does not merely need
translating, it needs running.

---

## D35 — The blob store is a seam; object storage is the commercial implementation

**Decision.** `blob.Store` moves to `pkg/blob` and becomes a documented
extension point. `internal/blob/filesystem` is the implementation the community
edition ships. The S3-compatible one belongs to the commercial edition, in the
separate module described in `docs/EXTENSIONS.md`.

**Why it is a legitimate seam** and not an API frozen for commercial reasons —
the thing D-none-of-the-above warns about and `EXTENSIONS.md` sets a bar for:
the interface predates the commercial question, the reason to replace it is
deployment shape rather than performance, and an operator running Ceph, GCS or
Azure has the same reason to write one as we do. It sits at a storage boundary
rather than in a hot path, so it leaks no internals. A seam whose only possible
implementer is the vendor would fail all three tests; this passes them.

**What content addressing buys here.** A blob's name is derived from its bytes,
so an implementation coordinates nothing: writing the same bytes twice is the
same write. That is what makes a second backend cheap enough to be worth
offering as a seam rather than a rewrite.

**The obligation the signatures do not state**, and the reason it is written
down: `Put` must be atomic. A reader that sees a partially written blob under a
digest that looks complete does not get a corrupt file — it gets a patch that
passes verification and applies the wrong thing. The filesystem implementation
writes to a temporary file and renames.

**The concern this decision accepts.** The worker reads blobs directly, so a
community deployment across several hosts needs a shared volume. Making object
storage commercial therefore prices a *deployment topology* rather than an
integration, which is the failure mode `saas-thinking/09` argues against for
caching. It is accepted because the shared volume is a real, documented, working
answer — NFS or EFS, one mount point — where a missing pull cache would have
left no answer at all. `SCALING.md` §6 states the latency cost so nobody sizes a
deployment without it.

**If that stops being true** — if the shared-volume path turns out to block
evaluations rather than merely complicate them — the fix is not to move the S3
backend into the community edition. It is for `nitd` to serve blobs to workers
over the API it already exposes, which removes the shared volume for everyone
and costs the commercial edition nothing it was selling.

---

## D36 — A pull projection is shared by rights profile, not by user

**Decision.** `internal/pullcache` caches a filtered projection under
`(repository, from, to, rights profile)`, where the profile comes from
`policy.Profile`. Users whose read rights are identical are served the same
bytes. The cache is per worker and holds descriptors, not patches.

**Why.** A pull diffed and filtered per user. Five hundred developers pulling
after a release meant five hundred of each for one upstream change — the cost
that arrives on a release day rather than gradually, and the one a customer
notices first.

**The whole risk is the key**, and it is not a performance risk. If two subjects
with different rights ever share a profile, one developer receives another's
files, delivered by the component whose purpose is to prevent that. So the
profile is not "same groups" or "same user list"; it is derived from what
`Evaluate` actually reads.

For a fixed repository, ref and action, a decision depends on the subject
through exactly two things: whether the user is disabled, and which rules pass
`HasAction`, `MatchesSubject` and `MatchesRef`. Everything downstream —
`MatchesPath`, the deny-wins fold, the specificity tie-break — reads the rule
and the path and never the subject. Two subjects agreeing on those two things
therefore agree on every path, whatever the path is. The profile hashes exactly
those, plus the policy version, which pins which compilation the rule positions
refer to.

**Positions rather than rule ids.** A position is unique within a compiled
policy by construction. An id is author-supplied or derived, and while the
loader rejects duplicates today, the profile's soundness would then rest on a
validation rule rather than on arithmetic.

**Sufficient, not necessary.** Two subjects who would decide alike but whose
rule sets differ get different profiles, and the work is done twice. The other
direction is a leak, so this is where the imprecision belongs.

**Proved rather than argued.** `TestEqualProfilesDecideAlike` runs every subject
pair against a path corpus; the generated variant does the same for 200 policies
with overlapping groups, exemptions and ref restrictions. Both were verified by
mutation — a constant fingerprint and one ignoring `except` are caught, and a
third mutation (dropping the `MatchesRef` filter) is deliberately *not* a leak,
because the ref is hashed directly; a separate test pins that it costs sharing.

**Descriptors, not bytes.** The patch already lives in the blob store under its
digest, so an entry is a few dozen bytes. That is what makes the bound a
constant rather than a setting: 1024 entries cost less than one patch, and a
repository with a thousand distinct rights profiles has a readability problem
before it has a cache problem.

**An entry cannot outlive what it names.** Generated pull patches expire, and a
hit naming a swept one would hand a client a digest it cannot fetch. Entries
expire at a quarter of the artifact TTL *and* a hit verifies the blob exists;
a missing blob is a miss, so the worst case is one recomputation.

**Per worker, deliberately.** A shared cache needs a table, a migration on three
backends and an invalidation story. Each pull records `reused_projection` in its
audit detail, so the hit rate is observable before anyone pays for the shared
version — and so an operator asking why one pull took 40 ms and another 40
seconds is told rather than left to guess.

---

## D37 — Retention is an operator command, and partitioning is not the default

**Decision.** `nitctl audit prune` removes audit records older than a cutoff, on
all three backends. It is exposed through `store.AuditPruner`, which is
deliberately not part of `store.Store`. Range-partitioning `audit_log` remains
available to an operator and is not recommended.

**Why it had to exist.** Until now nothing could remove an audit record. D31
made that honest — a `DELETE` fails loudly instead of silently removing nothing
— but honest is not the same as usable: a deployment obliged to honour a
retention period had no way to honour it. That is a compliance problem, and it
was the last one on the list.

**Why not on the Store interface.** A server or a worker holds a `store.Store`.
If pruning were reachable from it, some code path eventually would, and the
component that records evidence would be able to remove it. Keeping the
capability behind a type assertion nothing on the request path performs is what
makes a purge an operator action rather than a possible one.

**Why it counts before it deletes.** Without `-yes` the command reports what
matches and stops. There is no undo, and a cutoff a year off is an ordinary
typo. A future cutoff — which would empty the table — is refused rather than
executed faithfully.

**Why the purge records itself, twice.** `audit.purge_started` before,
`audit.purge_completed` after, both naming the operator. A purge killed halfway
then leaves a `started` with no `completed`, which is exactly what an auditor
needs to see. The alternative is a gap in the trail that nothing explains, which
is indistinguishable from tampering.

**What the backends cannot do equally.** PostgreSQL lifts and restores the guard
inside each batch's transaction; `ALTER TABLE` takes a lock no concurrent
session passes, so the guard is never observably off. MySQL and MariaDB have no
`DISABLE TRIGGER`, only `DROP`, which is DDL and commits at once — so a window
exists there. Three things make that acceptable: only the DELETE guard comes off
(UPDATE stays refused throughout, so the window permits removal and never
rewriting), the window can only be opened by an account with DDL rights, which
could equally `DROP TABLE`, and the next prune reports a guard it finds already
missing instead of silently restoring it. The guarantee always was "an
application bug cannot rewrite history", never "an operator cannot".

**Why partitioning is not the default.** It would make a purge O(1), and it
weakens what it makes manageable. `DROP PARTITION` fires no trigger on any of
the three engines — verified against MariaDB 11.8 and MySQL 8.4 — so
partitioning converts "removing audit records requires deliberately lifting a
guard" into "one `ALTER TABLE`". On MySQL and MariaDB it costs more: both refuse
foreign keys on a partitioned table (error 1506), so `audit_log` would give up
all four of its own, and `ON DELETE SET NULL` would stop cleaning up after a
deleted user. It stays documented and available; whoever carries the compliance
risk decides.

**A correction this decision carries.** The schema comment claimed `audit_log`
answers "who could do what, when, and under which rules?". The policy bundle's
git history answers that, with blame and rollback, which is why rules live in
files. What only this table holds is refusals — which leave no trace on the
forge because they never reached it — and deliveries, which are the evidence a
read rule was applied. The comment is corrected in both dialects.

---

## D38 — `pkg/nitd`, the one package in `pkg/` that performs IO

**Decision.** `pkg/nitd` assembles and runs the control plane and the worker.
`cmd/nitd` and `cmd/nit-worker` are built on it, and so is anything outside this
module. It takes the only exception to the rule that `pkg/` performs no IO.

**Why it had to exist.** `internal/server`, `internal/worker`,
`internal/bootstrap` and `internal/store/connect` are internal, so nothing
outside the module could assemble a server. That is correct for the parts and
wrong for the whole: `store.Store`, `blob.Store`, `policy.Source` and
`audit.Sink` are all public extension points, and until now an implementation of
one could be written but never *run*.

Found by building `nit-enterprise` — the compiler said it plainly:
`use of internal package github.com/NitScm/nit/internal/server not allowed`.

**Why the exception is safe.** The rule protects the authorization path from
acquiring IO dependencies. This package is a leaf: nothing else in the module
imports it, so no IO of its can reach anything. That reasoning decays the moment
somebody imports it for a helper, which is why `boundary_test.go` walks the
module's import graph and fails if any package other than the two binaries
imports it — verified to catch both a plain violation and one that would create
an import cycle.

**Why the binaries were rewritten on it** rather than left alone. A façade used
only by outsiders drifts from what the product does within a release. `cmd/nitd`
went from 202 lines to 84 and calls `nitd.Serve(ctx, cfg, nitd.Deps{})`; a zero
`Deps` is not a degraded mode, it is *the* mode. The two shipped binaries are
therefore the façade's most demanding test.

**Config is an alias, not a copy.** `type Config = bootstrap.Config`. The
configuration was already a public contract — one-to-one with `nit.yaml`, every
field documented in `docs/CONFIGURATION.md` — so the alias makes that visible
rather than creating it, and a parallel struct of twenty-five fields would drift
from the one the file maps to. An alias crosses the module boundary without the
caller importing anything internal; that was verified before the design was
settled, not assumed.

**What it deliberately does not do.** It traps no signals. A process embedding
nit alongside other things has its own idea of what `SIGTERM` means, so `cmd/`
wraps the context and the package only respects it.

**Ownership is explicit.** What a caller supplies, the caller closes; what the
package opens, it closes. A caller sharing one store between a server and a
worker in one process would otherwise have it closed underneath them.

---

## D39 — A conformance suite for `policy.Source`, and the spec it needs to be useful

**Decision.** `pkg/policy/policytest` holds every `policy.Source` to the same
assertions, and `policyconfig.LoadSpecFS` exposes the bundle before it is
compiled.

**Why the suite.** `policy.Source` was the third seam declared and the first
with nothing checking it. Its obligations are not in the signature: `Current`
returns a `*Policy`, and the interesting part is that it must be cheap enough
for the request path, never nil, safe under concurrent readers, immutable once
handed out, and — the one that matters — unchanged by a bundle that fails to
compile.

That last property is the difference between a typo and an incident. Failing
open grants access nobody authorized; failing closed takes an outage on every
edit. The suite asserts the third answer, and a mutation confirmed it: making
the loader clear its bundle on a compile error fails
`ABrokenBundleChangesNothing`, while a harmless edit beside it does not.

**A defect in the suite's own design, found by implementing it.** The first
version had `Publish(spec)` and asserted the served version equalled the
published one. The directory loader computes its version as a *hash of the
bundle*, so it can never honour a version chosen by a caller — the suite was
unimplementable by the implementation it was written for. It now asks for a
*behaviour* ("this user may read, or may not") and asserts the decision changed
and the version moved, which both kinds of source can satisfy.

**Why `LoadSpecFS`.** `policy.Source`'s own documentation names composition as
the case it was extracted for: rules from files, group membership resolved
against a company directory. The loader made that impossible — it read the files
and compiled in one step — so the only way to compose was to re-implement the
YAML reader, which would drift from this one on the next format change. The
repository documented a use case it did not permit.

**What a composer must respect**, both stated where the function is and tested:
`Compile` is not optional, because it is what resolves group inclusion and
refuses a rule naming an undeclared subject; and a composed bundle sets its own
`Version`, because the loader's is a hash of the files and everything keyed on a
version — a sync point, an audit record, the pull cache's rights profile — would
otherwise conflate two bundles that decide differently.

---

## D40 — A notification shortens a wait; the poll still guarantees it

**Decision.** On PostgreSQL, a trigger calls `pg_notify` when a task's `state`
changes, the store exposes `store.TaskNotifier`, and `internal/taskevents` wakes
the long poll. **The poll is not removed.** MySQL and MariaDB implement none of
this and keep polling.

**Why.** `GET /v1/tasks/{id}/events` re-read the task every 500 ms per waiting
client — one query per developer per half second, for a row that changes twice
in a task's life, on a wait somebody is watching.

**Why the poll stays.** A notification can be lost: the listening connection can
fail and reconnect across a change, a buffer can overflow, and a backend may
have no mechanism at all. A client that treated a notification as its liveness
guarantee would hang on the first blip — replacing a small predictable latency
with a rare unbounded one. So the contract is stated where an implementer will
read it: *a notification is a hint, never a substitute for reading.*

**Subscribe before reading.** The first version of the hub subscribed and waited
in one call, which left a window: read the state, the task changes, then
subscribe — and the waiter sits on the ticker anyway. A test caught it, and the
API was split into `Subscribe` then `Wait` so the order is expressible. The
window was the whole feature, silently.

**Only on a change of state.** A heartbeat rewrites `lease_expires_at` every few
seconds; a trigger on any update would wake every waiter for it and cost more
than the poll it replaces.

**A trigger, not `pg_notify` in the store.** The notification then covers every
path that changes a task, including one nobody has written yet and one an
operator runs by hand. In the application it would have to be repeated in
`Claim`, `Complete`, `Fail`, `Cancel` and `ReleaseExpired`, and forgetting one
produces a client that hangs for a full interval on exactly one transition.

**The MySQL migration is a no-op that exists anyway.** Both dialects keep the
same version numbers, so `nitctl migrate -status` describes one schema rather
than two. The file says what cannot be done and why, instead of being absent and
leaving a reader to wonder.

---

## D41 — Branch exclusion is a unique constraint, not a lock. Supersedes D15.

**Decision.** A row in `partition_leases`, keyed `(tenant_id, partition_key)`,
is the right to run a task on one branch. A claim inserts it; every exit from
running deletes it. The global advisory lock of D15 — and MySQL's `GET_LOCK` —
are gone.

**Why D15 existed, and why it can go.** `FOR UPDATE SKIP LOCKED` does not give
partition exclusion: two workers scanning concurrently lock *different rows of
the same branch*, both see nothing running on it, and both claim it. D15 took a
global lock instead, knowingly, and it was the throughput ceiling of the whole
dispatch layer — a busy repository slowed the claim path for every other
repository.

A unique constraint answers the same question without serializing anything else.
Two workers racing for one branch both insert; one wins, and the loser retries
with another task rather than waiting behind it.

**Two authorities, and the second is not decoration.** The insert decides who
owns the branch. `AND state = 'queued'` on the update decides who owns the
*task* — which is what keeps two workers off a single pull, since pulls have no
partition and take no lease. Neither depends on a scan being right.

**A losing worker retries, five times, then reports no task.** It lost a race,
not a search, and each retry sees the winner's lease and skips that partition.
Bounded because a worker that keeps losing is burning a connection on a queue
others are draining faster than it can; polling again in a second is cheaper.

**The dequeue scan went with the lock.** Exclusion is a left join against
`partition_leases` instead of `NOT EXISTS (SELECT 1 FROM tasks busy …)`, so a
queue whose head is full of busy branches is no longer walked past on every
claim — the cost that grew with queue depth × busy partitions, which is exactly
what a serialized branch produces.

**Every exit from running frees the branch, transactionally.** Complete, fail,
requeue and lease expiry. A lease outliving its task blocks that branch silently
until a human looks in the database, so this is a conformance assertion — the
suite walks all four exits and re-claims after each. Removing either release
makes it fail, which was verified by removing them.

**One lock order everywhere: `partition_leases`, then `tasks`.** The first
version wrote them the other way round in the finishing transitions, and InnoDB
found it immediately — twelve workers draining thirty tasks lost three to
"Deadlock found when trying to get lock". This is the second deadlock in this
codebase caused by two paths agreeing on what to lock and not on the order, and
the lesson is now written where both implementations can see it.

**Migrating a live deployment backfills.** Tasks already running when the
migration lands would otherwise look like free branches to the first claim
afterwards — two workers on one branch, caused by the change that exists to
prevent that.


---

## D42 — A sync token is signed with a key derived per tenant

**Decision.** `synctoken.Root` holds the deployment secret and cannot sign
anything. A `Signer` is obtained `For` a tenant, with an HKDF-SHA256 subkey.
Tokens carry `st2`; `st1` tokens still verify, but only for the default tenant.

**Why.** A sync token is the client's claim about which upstream commit its
patch was computed against, and the server applies the patch on top of whatever
the token names. One key per deployment means a token minted for one tenant
verifies for every other, so a bug in whatever resolves the tenant stops being a
rejected request and becomes a patch applied on somebody else's repository, on a
base its author was never entitled to see.

**Why a type and not a convention.** The root cannot sign. Asking for a signer
means naming a tenant, and there is no value that means "all of them", so the
compiler asks the question rather than a reviewer. A derivation that a caller
could forget is a derivation that will be forgotten.

**Rotation is unchanged.** Nothing is stored; rotating the root rotates every
tenant at once, which is what an operator already expects.

**The legacy format is scoped to the world it came from.** `st1` tokens verify
only under the default tenant's signer, so the moment a deployment has a second
tenant they are refused rather than accepted everywhere. Without that scoping
the transition would quietly become the hole it exists to close. It costs a
release: every developer's stored token is replaced by their next pull.

---

## D43 — Blobs are stored under the tenant that owns them

**Decision.** The filesystem blob store is rooted at
`storage.blob_dir/<tenant>`. `blob.Fallback` reads the old flat location when
the new one misses.

**Why, given the leak is currently unreachable.** A patch is fetched through its
task, and `handleTaskPatch` guards on ownership; there is no endpoint that takes
a bare digest. So today a shared namespace leaks nothing. It is a deduplication
side channel waiting for the first endpoint that takes a digest — and
cross-tenant dedup is worth nothing, so there is no argument on the other side.

**Why no interface change.** A tenant is a different *root*, not a different
method signature. `blob.Store` is untouched, the S3 implementation in the
commercial edition uses its existing prefix, and the conformance suite did not
move.

**Why the fallback.** The blob store holds the authorized patch between the
control plane and a worker, so a patch that becomes unreachable mid-flight is a
push failing with `missing_patch` — the error operators already associate with a
misconfigured deployment. Handing them that error *because of an upgrade* would
teach exactly the wrong lesson.

It is transitional and should be removed once no blob predates the move, which
is one artifact TTL — a day by default. Deletes reach both stores so the old
location empties itself rather than being kept alive by the thing meant to
retire it.

---

## D44 — A request's tenant comes from its token, not from the process

**Decision.** `store.SessionStore.ByTokenHash` no longer takes a tenant,
`auth.Principal` carries the one resolved from the session, and every handler
reads it from the request rather than from `server.Config.Tenant`.

**Why the lookup lost its tenant argument.** A token is what *resolves* a
tenant, so asking the caller to supply one is asking it for the answer to the
question it is asking. The hash is unique across the deployment by constraint
(`sessions_token_hash_unique`), so the lookup is unambiguous without it, and the
tenant then comes off the session — which is authoritative.

That is not a weakening. The old filter was redundant against a correct store:
matching a 32-byte hash already identifies one row, and the tenant it carried
was assumed rather than checked.

**What this finishes, and what it does not.** It is the first half of gap 1 in
`saas-thinking/03`. A process no longer serves exactly one customer by
construction. What remains is the half that makes forgetting *impossible* rather
than merely unnecessary: an absent principal still falls back to the default
tenant, which is right today and becomes "read the first tenant's data" the day
there is a second one. PostgreSQL row-level security is the second layer, and it
is not here.

**Issuance still takes a tenant, deliberately.** `Service.Issue` provisions a
credential *into* a tenant; that is an operator naming a customer, not a request
discovering one. Resolving it from context there would mean a token could only
be issued for whoever happened to be asking.

**A test that passed for the wrong reason, and how it was found.** The first
version queried from the default tenant and asserted another tenant's repository
was absent — which held whether the resolution worked or not, because the right
answer and the wrong one coincide there. Removing the resolution did not fail
it. It now queries from the *second* tenant, where the two answers differ, and
the mutation fails it.
