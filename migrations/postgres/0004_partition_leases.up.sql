-- Make branch exclusion a unique constraint instead of a global lock.
--
-- Until now every claim, by every worker, for every repository, serialized on
-- one advisory lock (D15). It was correct and it was the throughput ceiling of
-- the whole dispatch layer: a busy repository slowed the claim path for every
-- other repository.
--
-- The lock existed because FOR UPDATE SKIP LOCKED does not give partition
-- exclusion. Two workers scanning concurrently lock *different rows of the same
-- branch*, both see no running task for that partition, and both claim it.
--
-- A row here is the right to run a task on one partition. Two workers racing
-- for the same branch both try to insert it; the unique constraint decides, one
-- of them loses and picks another task. Contention is per-branch, and two
-- repositories no longer wait for each other at all.
--
-- It also fixes the dequeue scan. The old query asked
-- "NOT EXISTS (SELECT 1 FROM tasks busy WHERE busy.partition_key = …)" for
-- every candidate, so a queue whose head is full of busy branches was walked
-- past on every claim. Exclusion is now a left join against this table.

CREATE TABLE partition_leases (
    tenant_id      TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,

    -- "repository:branch" for a push. Pull tasks have no partition and take no
    -- row here, which is what lets them run fully in parallel.
    partition_key  TEXT NOT NULL,

    -- The task holding it. UNIQUE because a task runs on one partition, and it
    -- is what makes releasing a lease a lookup by task id rather than a scan.
    --
    -- ON DELETE CASCADE so that removing a task cannot strand its branch. That
    -- is not a normal path — tasks are not deleted — but a stranded partition
    -- blocks a branch until a human notices, which is the worst kind of bug to
    -- leave available.
    task_id        UUID NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,

    acquired_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (tenant_id, partition_key)
);

-- Existing deployments may have tasks running right now. Without this backfill
-- their branches would look free to the first claim after the migration, and
-- two workers would push to the same branch — the exact failure this table
-- exists to prevent, caused by the change that prevents it.
INSERT INTO partition_leases (tenant_id, partition_key, task_id, acquired_at)
SELECT DISTINCT ON (tenant_id, partition_key)
       tenant_id, partition_key, id, COALESCE(started_at, NOW())
FROM tasks
WHERE state = 'running' AND partition_key IS NOT NULL
ORDER BY tenant_id, partition_key, started_at
ON CONFLICT DO NOTHING;
