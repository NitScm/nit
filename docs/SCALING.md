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

## 3. Every task clones from scratch

`internal/worker` clones into `storage.work_dir` and removes the clone when the
task ends. On a large repository this dominates every other cost in §2, and it
is the single largest available win.

It was left out deliberately: a clone shared between tasks that apply patches
and rebase is also a way for one task's leftover state to corrupt another's, and
that trade was not worth taking before the correctness of the apply path was
settled. It now is.

**The work.** A cache keyed by repository, `git fetch` instead of `git clone`,
`git worktree` per task for isolation, and a hard reset between uses. Expect the
cycle in §2 to fall to fetch-plus-checkout — often an order of magnitude on a
large repository.

**Until then**, size `storage.work_dir` for largest repository × concurrency, and
set `queue.lease_duration` to comfortably exceed a full clone.

---

## 4. Pull is O(users)

Each pull clones, diffs `sync_point..tip`, and filters the result **per user**.
Five hundred developers pulling after a release is five hundred clones and five
hundred diffs for one upstream change, with no sharing between users whose read
rights are identical.

This is the second-worst scaling property, and the one that bites on a release
day rather than gradually.

**Mitigation today.** Dedicate workers to read traffic so it does not compete
with pushes for disk:

```sh
nit-worker -queues=pull -concurrency=4
```

**The work.** Two independent wins: the clone cache of §3 removes most of the
cost, and a filtered patch is cacheable by `(repository, from, to, read-rights
fingerprint)` — everyone in the same groups gets the same bytes, so the first
pull after a release could serve all the others.

---

## 5. The claim path takes a global lock

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

**The blob store is a filesystem** (`internal/blob`, `blob.NewFS`). There is no
object-storage backend, so `nitd` and every worker must share one volume. Across
hosts that means NFS or EFS: a single mount point, and its latency on the write
and read of every push.

*The work.* An S3-compatible backend behind the existing `blob.Store` interface.
It also removes the constraint that shapes the whole topology today.

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

### `audit_log` grows without bound

The table is **not partitioned** and nothing prunes it. A successful push writes
two rows (`push.accepted` from the control plane, `push.applied` from the
worker); a refused one writes `push.rejected` plus one `push.denied_path` per
offending path. Each row is written against four indexes.

The append-only rules make this sharper than it looks:

```sql
CREATE RULE audit_log_no_delete AS ON DELETE TO audit_log DO INSTEAD NOTHING;
```

`DO INSTEAD NOTHING` is **silent**. A purge reports `DELETE 0`, exits zero, and
leaves every row in place, so an operator is told the cleanup worked when nothing
happened. Both this and the procedure below were checked against a real
PostgreSQL, not inferred.

Purging today:

```sql
BEGIN;
DROP RULE audit_log_no_delete ON audit_log;
DELETE FROM audit_log WHERE occurred_at < now() - interval '2 years';
CREATE RULE audit_log_no_delete AS ON DELETE TO audit_log DO INSTEAD NOTHING;
COMMIT;
```

*The work.* Range-partition `audit_log` by `occurred_at`, monthly. `DROP TABLE`
on an old partition is not a `DELETE`, so the rule does not block it and
retention stops being a manual, rule-dropping procedure. This is the change that
should happen before any deployment that has to keep records for years.

### Long polling is a poll

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

1. **Clone cache** (§3) — largest single win; improves §2 and §4 at once.
2. **`audit_log` partitioning** (§7) — the only item that is a correctness and
   compliance problem rather than a performance one.
3. **`LISTEN`/`NOTIFY`** (§7) — cheapest, removes a per-client database load.
4. **`partition_leases`** (§5) — removes the global lock and the dequeue scan
   together.
5. **Pull result cache** (§4) — turns release-day load from O(users) into O(1).
6. **Object storage for blobs** (§6) — removes the shared-volume constraint on
   topology.

Items 1, 3 and 5 are performance. Items 2 and 4 change behaviour under load and
deserve tests written before the change, not after.

---

## 10. Sizing what exists today

| | |
| --- | --- |
| `queue.lease_duration` | Must exceed a full clone of your largest repository. Too short and tasks retry forever; too long and a crashed worker blocks its branch for that long. |
| `storage.work_dir` | Largest repository × worker concurrency, with room to spare. |
| `storage.blob_dir` | In-flight patches only, but shared by `nitd` and every worker. |
| Worker concurrency | Each runner holds one clone. Predict disk from concurrency, not from throughput. |
| `queue.poll` | Raising it lowers the §5 lock rate at the cost of latency on an idle queue. |
| `server.event_max_wait` | Below any proxy idle timeout in front of `nitd`. |

Two numbers say something is wrong *now*: `nitctl stats` showing **queued**
climbing, and **busy branches** staying high. Everything else is history.
