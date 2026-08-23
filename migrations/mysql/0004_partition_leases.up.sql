-- Make branch exclusion a unique constraint instead of a global lock.
--
-- The reasoning is in migrations/postgres/0004_partition_leases.up.sql and is
-- the same here. What differs is what it replaces: this backend serialized
-- claims on GET_LOCK, held by the session rather than the transaction, with the
-- release ordered by hand after the commit. That is gone with the lock.

CREATE TABLE partition_leases (
    tenant_id      VARCHAR(64) NOT NULL,

    -- "repository:branch" for a push. Pull tasks have no partition and take no
    -- row here, which is what lets them run fully in parallel.
    partition_key  VARCHAR(512) NOT NULL,

    -- The task holding it. UNIQUE because a task runs on one partition, and it
    -- is what makes releasing a lease a lookup by task id rather than a scan.
    task_id        CHAR(36) NOT NULL,

    acquired_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),

    PRIMARY KEY (tenant_id, partition_key),
    CONSTRAINT partition_leases_task_unique UNIQUE (task_id),
    CONSTRAINT partition_leases_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants(id),
    CONSTRAINT partition_leases_task_fk FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin ROW_FORMAT=DYNAMIC;

-- The same backfill, for the same reason: a task running across the migration
-- must keep its branch, or the first claim afterwards puts a second worker on
-- it.
--
-- No DISTINCT ON here — MySQL has none — so the grouped form does the same job:
-- one row per (tenant, partition), holding the task that started first.
INSERT IGNORE INTO partition_leases (tenant_id, partition_key, task_id, acquired_at)
SELECT t.tenant_id, t.partition_key, t.id, COALESCE(t.started_at, CURRENT_TIMESTAMP(6))
FROM tasks t
JOIN (
    SELECT tenant_id, partition_key, MIN(id) AS id
    FROM tasks
    WHERE state = 'running' AND partition_key IS NOT NULL
    GROUP BY tenant_id, partition_key
) first ON first.id = t.id;
