# Scaling

What nit scales on, what it does not, and the order in which the ceilings
arrive.

**These figures are derived from the code, not measured.** Nothing here comes
from a benchmark. Treat the numbers as orders of magnitude to plan against, and
measure your own deployment before committing to them. Where a limit is
architectural rather than a tuning problem, it says so.

The short version: nit scales across **many repositories and many branches**. It
does not scale on **one very busy branch**, and it should not be adopted for a
monorepo where a large team pushes to a single trunk without reading §2 first.

---

## 1. What scales

| | Why |
| --- | --- |
| `nitd` replicas | Stateless apart from PostgreSQL. Add them behind a load balancer; they need only the same `security.sync_key`. |
| Repositories and branches in parallel | Each `repository:branch` is an independent queue partition. Workers scale linearly across them. |
| Refused pushes | Authorization runs **before** anything is queued, so a denial costs no clone, no worker, no disk. |
| Workspaces | A sync point is one row per `(workspace, repository, branch)`, read and written by primary key. |
| Policy size | The bundle is compiled once and reloaded every `policy.reload`; evaluation is in memory. |

---

## 2. The dominant ceiling: one branch is serial

This is the design (D11), not a defect. At most one task runs per
`repository:branch`, so the throughput of a branch is:

```
1 / (clone + apply + rebase + push)
```

and **every task clones from scratch** — there is no clone cache (§3). So:

| Repository | Plausible cycle | Pushes per hour, one branch |
| --- | --- | --- |
| Small service | 2–5 s | ~700–1800 |
| Medium, ~1 GB history | 20–40 s | ~90–180 |
| Large monorepo | 2–5 min | ~12–30 |

Two hundred developers pushing to a single `main` on a large monorepo is not
viable, and no amount of tuning changes it: adding workers buys throughput
*across* branches, never *within* one.

**What to do instead.** Feature branches partition perfectly — a hundred
developers on a hundred branches is a hundred parallel queues. A trunk-based
workflow where everyone commits directly to `main` is the one shape nit is worst
at, and that is worth knowing before adoption rather than after.

**What would move it.** Only §3. The serialization itself must stay: it is what
makes a push atomic with respect to other nit pushes.

---

## 3. Every task used to clone from scratch

**Fixed.** `internal/gitcache` keeps a bare mirror per repository, fetched
before each use, and cuts a `git worktree` per task from it. A task pays for the
delta rather than for a whole clone.

The numbers in §2 above predate it and are left as they were: they describe the
cost this removed, and the cycle now falls to fetch-plus-checkout. What follows
is the reasoning it was built against, kept because the risks did not go away —
they moved.

It was left out for a long time on purpose: a clone shared between tasks that
apply patches and rebase is also a way for one task's leftover state to corrupt
another's, and the failure mode is not a broken build — it is a wrong commit on
the forge under a developer's name.

**What that bought.** A worktree is never reused; each task gets a fresh one and
it is removed with `--force` afterwards, whatever state a failed task left. A
mirror that cannot be opened *or* fetched is rebuilt rather than nursed, because
a corrupt mirror poisons every later task for that repository. The credential is
never written to disk: the mirror is created empty with `git init --bare` and
filled by a fetch whose URL is passed per call, and the push goes to that URL
rather than to a configured remote.

**Sizing changes with it.** `storage.work_dir` now holds a persistent mirror per
repository plus one worktree per concurrent task, rather than a full clone per
task. Steady-state disk is closer to the sum of the repositories than to
repository × concurrency, and it does not return to zero between tasks.

**Which is why mirrors are evicted.** A clone returned its disk when the task
ended; a mirror does not, including the mirror of a repository pushed to once a
year. `storage.mirror_budget_bytes` (default 20 GiB) caps what the mirrors may
occupy. Past it, the least recently used ones are removed until the rest fit —
ordered by a marker file nit writes on each use, not by directory timestamps,
which say when git last happened to write rather than when nit last needed the
repository.

A mirror with a live worktree is never a candidate, whichever process owns that
worktree: the sweep reads git's own `worktrees/` metadata, not just its own
bookkeeping. Eviction costs one slow task when the repository comes back; a full
disk fails every task on every repository and needs a human.

The sweep runs after a task releases its worktree, at most once a minute — it
walks the mirrors to measure them, and doing that after every task on a large
repository would cost more than it saves. Set the budget to `0` to disable
eviction, which is only sensible when something else watches the volume.

**One deployment constraint is new**: a `work_dir` belongs to one worker
process. The mirrors are locked per repository within a process; two processes
sharing a directory would race on the same mirror.

---

## 4. Pull was O(users)

**Fixed, within a worker.** A pull used to diff `sync_point..tip` and filter the
result per user: five hundred developers pulling after a release meant five
hundred diffs and five hundred filter passes for one upstream change, with no
sharing between users whose read rights are identical.

`internal/pullcache` shares the result. The key is
`(repository, from, to, rights profile)`, and a hit skips the diff as well as
the filtering — the key is built before either runs.

**What makes it safe** is `policy.Profile`, and it is worth being precise about
why, because a wrong answer here is not a slow pull but one developer receiving
another's files.

For a fixed repository, ref and action, `Evaluate` depends on the subject
through exactly two things: whether the user is disabled, and which rules pass
`HasAction`, `MatchesSubject` and `MatchesRef`. Everything after that reads the
rule and the path, never the subject. So two subjects agreeing on those two
things agree on every path, whatever the path is — and the profile is a hash of
precisely those, plus the policy version that pins which rules the positions
refer to.

It is a sufficient condition, not a necessary one. Two subjects who would decide
alike but whose rule sets differ get different profiles and the work is done
twice. That is the direction to be wrong in.

The property is tested rather than argued: `TestEqualProfilesDecideAlike` runs
every subject pair against a path corpus, and
`TestEqualProfilesDecideAlikeOnGeneratedPolicies` does the same for 200
generated policies with overlapping groups, exemptions and ref restrictions.
Both were checked by mutation — a constant fingerprint and one ignoring `except`
are caught.

**What is cached** is the artifact descriptor and the file counts, not the
bytes: the patch is already in the blob store. An entry is a few dozen bytes,
1024 of them are kept, and eviction is least-recently-used. The size is not
configurable because there is nothing to tune — a policy with a thousand
distinct rights profiles for one repository has a readability problem long
before it has a cache problem.

**Entries expire at a quarter of `storage.pull_ttl`**, and a hit verifies the
blob still exists before it is returned. An entry naming a swept patch would
hand a client a digest it cannot fetch; a missing blob is treated as a miss, so
the worst case is one recomputation.

**Per worker, and now replaceable.** A per-worker cache needs no table, no
migration and no invalidation story, and it still collapses the release-day
herd — which arrives within minutes on the same handful of workers. That is the
right default and it is what ships.

`pullcache.Store` is the seam for the deployment where "the same handful of
workers" is not true: a control plane whose workers come and go, or one that
would rather use a store it already runs. `worker.Deps.PullCache` takes a
replacement, `pkg/pullcache/pullcachetest` proves one correct, and the
commercial edition ships a fleet-wide implementation over the deployment's own
database.

Before deciding you need one: `nitctl audit` shows which pulls reused a
projection — `reused_projection` in the record's detail — so the hit rate is
measurable rather than assumed.

**Also mitigated.** Dedicate workers to read traffic so it does not compete with
pushes for disk:

```sh
nit-worker -queues=pull -concurrency=4
```

---

## 5. The claim path used to take a global lock

**Fixed.** Exclusion is a unique constraint now, not a lock. A row in
`partition_leases` is the right to run a task on one branch, its primary key is
`(tenant_id, partition_key)`, and two workers racing for the same branch both
try to insert it — the constraint decides, the loser picks another task.
Contention is per-branch, and two repositories no longer wait for each other at
all.

The dequeue scan went with it: exclusion is a left join against that table
rather than a correlated subquery over `tasks`, so a queue whose head is full of
busy branches is no longer walked past on every claim.

**Two authorities, both single-row and both atomic.** The insert decides who
owns the branch; `AND state = 'queued'` on the update decides who owns the
*task*, which is what keeps two workers off one pull — a pull has no partition
and therefore no lease. Neither depends on a scan being right, which is what
made `FOR UPDATE SKIP LOCKED` insufficient in the first place.

**A losing worker retries**, up to five times, rather than reporting an empty
queue: it lost a race, not a search, and the retry sees the winner's lease.

**Every exit from running frees the branch**, in the same transaction as the
state change — complete, fail, requeue, lease expiry. A lease that outlived its
task would block that branch silently until somebody looked in the database, so
`storetest` walks all four exits and re-claims after each. Removing either
release makes it fail; that was checked by removing them.

**And the lock order is the same everywhere**: `partition_leases` first, then
`tasks`. The first version wrote them the other way round in the finishing
transitions, which gave InnoDB two transactions taking the same rows in opposite
orders — twelve workers draining thirty tasks lost three of them to a deadlock.

*What follows is the original note, kept because the reasoning is what mattered.*

### The original note

```go
// internal/store/postgres/tasks.go
SELECT pg_advisory_xact_lock($1)   // claimLockKey, one value for the deployment
```

Every claim, by every worker, for every repository, serializes on one advisory
lock. Each idle worker also polls every `queue.poll` (1 s by default), so N
workers take N lock acquisitions per second at rest.

The transaction is short — a lock, an expiry sweep, one `UPDATE … RETURNING` —
so this holds well into the tens of workers. It is nonetheless the throughput
ceiling of the whole dispatch layer, and it is global: a busy repository slows
the claim path for every other repository.

**Why it exists.** `FOR UPDATE SKIP LOCKED` alone does not give partition
exclusion. Two workers scanning concurrently lock *different rows of the same
branch*, both see no `running` task for that partition, and both claim it (D15).
A first attempt at per-partition `pg_try_advisory_xact_lock` during the scan was
flaky under coverage instrumentation; the global lock is correct and was
accepted knowingly.

**The work.** A `partition_leases` table with the partition key as primary key,
claimed by `INSERT … ON CONFLICT DO NOTHING`. Exclusion then comes from a unique
constraint rather than a lock, and contention is per-branch instead of global.

### Related: the dequeue scan

```sql
WHERE t.state = 'queued'
  AND NOT EXISTS (SELECT 1 FROM tasks busy
                  WHERE busy.partition_key = t.partition_key
                    AND busy.state = 'running')
ORDER BY t.created_at, t.id
LIMIT 1
```

`idx_tasks_dispatch` is `(state, created_at)` and knows nothing about
partitions, so when the head of the queue is full of tasks whose branches are
all busy, Postgres walks past every one of them on every claim. Cost grows with
queue depth × busy partitions — precisely the situation §2 produces. The
`partition_leases` table fixes this too, by making the exclusion a join instead
of a correlated subquery.

---

## 6. Storage and memory

**The blob store is a filesystem** (`internal/blob/filesystem`). The community
edition ships no object-storage backend, so `nitd` and every worker must share
one volume. Across hosts that means NFS or EFS: a single mount point, and its
latency on the write and read of every push.

*What exists.* `blob.Store` is a public seam in `pkg/blob`, documented in
[`EXTENSIONS.md`](EXTENSIONS.md) with the three obligations an implementation
carries. Anyone can write a backend against it; the commercial edition ships an
S3-compatible one.

*What is not solved by that.* A shared volume remains the answer here, and it is
a real answer rather than a placeholder — but it is the constraint that shapes
the topology, and an operator sizing a deployment should count its latency on
every push.

**Patches are held in memory.** Both the control plane and the worker read a
patch with `io.ReadAll` bounded by `storage.max_patch_bytes` (100 MiB default),
and decompression is bounded by the same figure. Worst-case resident memory is
roughly `concurrency × max_patch_bytes` per process, twice that while
decompressing. Lower the ceiling if your patches are small; raise concurrency
only with the memory to match.

*The work.* Stream the patch through the splitter. `pkg/patch` re-emits original
bytes section by section and does not fundamentally need the whole input
resident.

---

## 7. PostgreSQL

One primary, holding the queue, sync points and the audit trail. Read replicas
would help the operations API and nothing else: the queue needs the primary.

### `audit_log` grows, and now has a way to stop

**Fixed, as far as it needed fixing.** `nitctl audit prune` removes records
older than a cutoff, on all three backends. It is the only thing that can: the
server holds a `store.Store` and cannot reach `store.AuditPruner` through it, so
no request path can delete evidence.

The volume was smaller than this section once implied. A successful push writes
two rows (`push.accepted` from the control plane, `push.applied` from the
worker); a refused one adds one `push.denied_path` per offending path, bounded
by the *submitted changeset*. A pull writes two rows regardless of how many
files were withheld — naming them would leak the structure the read rules exist
to hide, and it bounds the volume as a side effect. Growth is linear in activity
and modest.

So the reason to prune is a retention period someone is obliged to honour, not
usually disk. Each row is still written against four indexes.

Emptying it is deliberately awkward, and since migration 0002 it is at least
honest about being so. The table carries a trigger that raises:

```
ERROR:  audit_log is append-only: DELETE is not permitted
HINT:   to purge, disable trigger audit_log_append_only on audit_log, ...
```

It replaced rewrite rules that answered a `DELETE` with `DELETE 0`, exiting
zero and removing nothing — so an operator was told the cleanup had worked.
That was verified against a live database, which is also how the replacement
was checked.

The `TRUNCATE` trigger is separate because a row trigger does not see a
`TRUNCATE` at all: without it the strongest guarantee in the schema would be
one word away from being bypassed by accident.

Purging is a deliberate act rather than a command that appears to work:

```sh
nitctl audit prune -keep-days 365          # counts, deletes nothing
nitctl audit prune -keep-days 365 -yes     # deletes, in batches
```

It writes `audit.purge_started` before and `audit.purge_completed` after, both
naming the operator, so an interrupted purge leaves a visible trace instead of
an unexplained gap. On PostgreSQL the guard is lifted and restored inside each
batch's transaction — `ALTER TABLE` takes a lock no concurrent session passes,
so the guard is never observably off. On MySQL and MariaDB dropping a trigger is
DDL that commits immediately, so a window exists; the next prune reports it
through `GuardsWereMissing` and closes it.

*What is deliberately not done.* Range-partitioning `audit_log` by
`occurred_at` would make a purge O(1), and it is available to an operator who
wants it — but it is not the default and not the recommendation.

`DROP PARTITION` fires no trigger, on any of the three engines. Verified against
MariaDB 11.8 and MySQL 8.4: the rows go and nothing raises. Partitioning
therefore converts "removing audit records requires deliberately lifting a
guard" into "one `ALTER TABLE`" — it weakens exactly what it makes manageable.
On MySQL and MariaDB it costs more still: both refuse foreign keys on a
partitioned table (error 1506), so `audit_log` would give up all four of its
own.

A deployment keeping records for years and pruning monthly may well want it.
That is a choice for whoever carries the compliance risk, taken knowingly.

### Long polling was a poll

**Fixed on PostgreSQL.** A trigger calls `pg_notify` when a task's `state`
changes (migration 0003), `internal/store/postgres` holds one `LISTEN`
connection per process, and `internal/taskevents` fans notifications out to the
waiters. A developer watching their own push is woken when it moves rather than
up to `EventPollInterval` later.

**The poll stays**, and that is the design rather than a leftover. A
notification can be dropped — the listening connection can fail and reconnect
across a change — so it is what shortens the wait, never what guarantees one
arrives. A client that trusted it would hang the first time a connection
blipped.

Two details worth keeping: the trigger fires only on a change of `state`,
because a heartbeat rewrites `lease_expires_at` every few seconds and waking
every waiter for that would cost more than the poll it replaces; and a caller
subscribes *before* reading the task, or a change landing in between leaves it
on the ticker for no reason — the window that made the first version of this
pointless, found by a test.

**MySQL and MariaDB keep polling.** They have no `LISTEN`/`NOTIFY` and no way
for a trigger to reach another connection, so `internal/store/mysql` does not
implement `store.TaskNotifier`, the server says so on its start-up line, and the
only difference is up to one interval of latency.

### The original note

`GET /v1/tasks/{id}/events` queries `tasks` every `EventPollInterval` (500 ms)
for up to `server.event_max_wait` (30 s). Five hundred clients waiting is roughly
a thousand `SELECT`s per second on the busiest table in the schema.

*The work.* `LISTEN`/`NOTIFY` on task state changes. Small, self-contained, and
the cheapest item on this page.

---

## 8. Policy evaluation

Compiled once, evaluated in memory. For each changed path, `Evaluate` walks
every rule of the repository, discarding non-candidates on three cheap tests —
action, subject, ref — before matching any pattern. So the cost is
`paths × rules` in guard checks and `paths × candidate rules × patterns` in
doublestar matching.

Hundreds of rules against patches of hundreds of files is not a concern. Bundles
with thousands of rules, evaluated against patches touching thousands of files —
a monorepo-wide refactor — are worth measuring before assuming.

*The work, if it ever matters.* Index rules by path prefix so a path only tests
the rules that could match it. Nothing about the current structure prevents it;
`Evaluate` stays total and order-independent either way.

---

## 9. In what order to fix it

Done, in the order they were taken:

- ~~**`partition_leases`** (§5)~~ — branch exclusion is a unique constraint, so
  two repositories no longer serialize against each other.
- ~~**`LISTEN`/`NOTIFY`** (§7)~~ — PostgreSQL wakes a waiting client instead of
  being asked twice a second. MySQL keeps polling, and says so.
- ~~**Audit retention** (§7)~~ — `nitctl audit prune`, on all three backends,
  recording itself as it goes. Partitioning is available to an operator and is
  not the default, for the reason §7 gives.
- ~~**Clone cache** (§3)~~ — a mirror per repository, a worktree per task, and
  an LRU disk budget so the mirrors cannot fill the volume.
- ~~**Pull result cache** (§4)~~ — release-day load falls from O(users) to
  O(distinct rights profiles), per worker.

What remains, in priority order:

Nothing on this list is load-bearing today. What remains in the document is
either done, deliberately declined (partitioning `audit_log`, see D37), or
waiting on evidence rather than on effort.

Object storage for blobs (§6) is not on this list: the seam exists, and the
implementation belongs to the commercial edition.

---

## 10. Sizing what exists today

| | |
| --- | --- |
| `queue.lease_duration` | Must exceed a full clone of your largest repository. Too short and tasks retry forever; too long and a crashed worker blocks its branch for that long. |
| `storage.work_dir` | The mirrors (bounded by `storage.mirror_budget_bytes`) plus one worktree per concurrent task. Give the volume room above the budget: the budget covers mirrors only. |
| `storage.mirror_budget_bytes` | The repositories worth keeping warm, summed. Below the size of one large repository it degenerates into cloning on every task. |
| `storage.blob_dir` | In-flight patches only, but shared by `nitd` and every worker. |
| Worker concurrency | Each runner holds one clone. Predict disk from concurrency, not from throughput. |
| `queue.poll` | Raising it lowers the §5 lock rate at the cost of latency on an idle queue. |
| `server.event_max_wait` | Below any proxy idle timeout in front of `nitd`. |

Two numbers say something is wrong *now*: `nitctl stats` showing **queued**
climbing, and **busy branches** staying high. Everything else is history.
