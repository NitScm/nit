-- nit initial schema.
--
-- Two principles run through this file.
--
-- Policy is not stored here. Users, groups, repositories and rules are authored
-- in a YAML bundle under version control (see docs/POLICY.md). The rows below
-- mirror the bundle only where a foreign key needs something to point at, and
-- they carry the bundle version that produced them.
--
-- tenant_id is present on every table even though nit ships single-tenant.
-- Threading a tenant through a schema after the fact is one of the most
-- expensive migrations there is, and nearly impossible to do safely once real
-- data exists.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ============================================================================
-- TENANTS
-- ============================================================================

CREATE TABLE tenants (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO tenants (id, name) VALUES ('default', 'Default tenant');

-- ============================================================================
-- IDENTITY
-- ============================================================================

-- A person. policy_user_id is the id used in the policy bundle; it is the join
-- between authorization decisions and stored state.
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,

    policy_user_id  TEXT NOT NULL,
    email           TEXT NOT NULL,
    display_name    TEXT NOT NULL DEFAULT '',

    disabled        BOOLEAN NOT NULL DEFAULT FALSE,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT users_policy_id_unique UNIQUE (tenant_id, policy_user_id),
    CONSTRAINT users_policy_id_not_empty CHECK (length(trim(policy_user_id)) > 0)
);

-- Authentication tokens issued to CLI installations.
--
-- Only the hash is stored: a leaked database dump must not yield usable
-- credentials.
CREATE TABLE sessions (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    token_hash   BYTEA NOT NULL,
    label        TEXT NOT NULL DEFAULT '',

    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,

    CONSTRAINT sessions_token_hash_unique UNIQUE (token_hash)
);

CREATE INDEX idx_sessions_user ON sessions(user_id) WHERE revoked_at IS NULL;

-- One checkout on one machine. Sync points are per workspace, which is what
-- lets a developer use a laptop and a desktop without corrupting either.
CREATE TABLE workspaces (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    label         TEXT NOT NULL DEFAULT '',

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at  TIMESTAMPTZ
);

CREATE INDEX idx_workspaces_user ON workspaces(user_id);

-- ============================================================================
-- REPOSITORIES
-- ============================================================================

-- Mirrors the policy bundle. Reconciled on every bundle reload; policy_version
-- records which bundle produced the current row.
CREATE TABLE repositories (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,

    policy_repo_id   TEXT NOT NULL,
    remote           TEXT NOT NULL,
    forge            TEXT NOT NULL,
    default_branch   TEXT NOT NULL DEFAULT 'main',

    policy_version   TEXT NOT NULL DEFAULT '',

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT repositories_policy_id_unique UNIQUE (tenant_id, policy_repo_id)
);

-- ============================================================================
-- SYNC POINTS
-- ============================================================================

-- The upstream commit whose filtered projection produced a workspace's current
-- state. See docs/PROTOCOL.md section 1: without this, a client's local commit
-- hash is meaningless to the server, because the local tree is a filtered
-- projection and its hashes exist nowhere upstream.
CREATE TABLE sync_points (
    tenant_id        TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    workspace_id     UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    repository_id    UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    branch           TEXT NOT NULL,

    upstream_commit  TEXT NOT NULL,

    -- The bundle that produced this projection. A policy change can widen or
    -- narrow what a workspace should contain, so a sync point taken under an
    -- older bundle may need a resynchronization rather than an incremental
    -- diff.
    policy_version   TEXT NOT NULL,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (workspace_id, repository_id, branch),

    CONSTRAINT sync_points_commit_not_empty CHECK (length(trim(upstream_commit)) > 0)
);

CREATE INDEX idx_sync_points_repo_branch ON sync_points(repository_id, branch);

-- ============================================================================
-- TASKS
-- ============================================================================

CREATE TYPE task_kind AS ENUM ('push', 'pull');

CREATE TYPE task_state AS ENUM (
    'queued',
    'running',
    'succeeded',
    'failed',
    'cancelled'
);

CREATE TABLE tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,

    -- Client-generated, makes submission idempotent. Networks fail mid-push;
    -- without this a retry creates a second upstream commit.
    request_id      TEXT NOT NULL,

    kind            task_kind NOT NULL,
    state           task_state NOT NULL DEFAULT 'queued',

    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id    UUID REFERENCES workspaces(id) ON DELETE SET NULL,
    repository_id   UUID NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    branch          TEXT NOT NULL,

    -- Serialization key. At most one task per partition runs at a time; pull
    -- tasks are read-only and leave this NULL so they run fully in parallel.
    partition_key   TEXT,

    payload         JSONB NOT NULL,
    result          JSONB,
    error           JSONB,

    attempts        INT NOT NULL DEFAULT 0,

    -- Lease held by the worker currently executing the task. lease_token is a
    -- fencing token: a worker whose lease expired must not be able to complete
    -- the task after another worker picked it up.
    lease_holder    TEXT,
    lease_token     TEXT,
    lease_expires_at TIMESTAMPTZ,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,

    CONSTRAINT tasks_request_id_unique UNIQUE (tenant_id, request_id),

    CONSTRAINT tasks_lease_consistency CHECK (
        (lease_token IS NULL AND lease_holder IS NULL AND lease_expires_at IS NULL)
        OR
        (lease_token IS NOT NULL AND lease_holder IS NOT NULL AND lease_expires_at IS NOT NULL)
    ),

    CONSTRAINT tasks_finished_after_started CHECK (
        finished_at IS NULL OR started_at IS NULL OR finished_at >= started_at
    )
);

-- The dequeue path: oldest queued task first, restricted to partitions with
-- nothing running.
CREATE INDEX idx_tasks_dispatch
    ON tasks(state, created_at)
    WHERE state = 'queued';

-- Used to test whether a partition is busy, and to compute queue position.
CREATE INDEX idx_tasks_partition
    ON tasks(partition_key, state)
    WHERE partition_key IS NOT NULL AND state IN ('queued', 'running');

-- Reaping expired leases.
CREATE INDEX idx_tasks_lease_expiry
    ON tasks(lease_expires_at)
    WHERE state = 'running';

CREATE INDEX idx_tasks_user ON tasks(user_id, created_at DESC);
CREATE INDEX idx_tasks_repo ON tasks(repository_id, branch, created_at DESC);

-- ============================================================================
-- ARTIFACTS
-- ============================================================================

CREATE TYPE artifact_kind AS ENUM ('push_patch', 'pull_patch');

-- Content-addressed blobs: uploaded patches and generated filtered patches.
--
-- Addressing by digest gives deduplication, resumable transfer and integrity
-- checking the client can perform itself.
CREATE TABLE artifacts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,

    digest             TEXT NOT NULL,
    kind               artifact_kind NOT NULL,

    size_bytes         BIGINT NOT NULL,
    uncompressed_bytes BIGINT NOT NULL,
    encoding           TEXT NOT NULL,

    -- Backend-specific locator: a path for the filesystem store, a key for S3.
    locator            TEXT NOT NULL,

    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at         TIMESTAMPTZ,

    CONSTRAINT artifacts_digest_unique UNIQUE (tenant_id, digest),
    CONSTRAINT artifacts_size_positive CHECK (size_bytes >= 0),
    CONSTRAINT artifacts_expiry_after_creation CHECK (
        expires_at IS NULL OR expires_at > created_at
    )
);

CREATE INDEX idx_artifacts_expiry ON artifacts(expires_at) WHERE expires_at IS NOT NULL;

-- ============================================================================
-- AUDIT LOG
-- ============================================================================

CREATE TYPE audit_effect AS ENUM ('allow', 'deny');

-- Append-only. This table is the product's regulatory deliverable: it is what
-- answers "who could do what, when, and under which rules?".
--
-- One row per denied path, plus one summary row per operation. Recording every
-- allowed path of every push would multiply volume by the size of a changeset
-- for information the summary already carries.
CREATE TABLE audit_log (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,

    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    actor_user_id   UUID REFERENCES users(id) ON DELETE SET NULL,
    actor_label     TEXT NOT NULL,

    action          TEXT NOT NULL,
    repository_id   UUID REFERENCES repositories(id) ON DELETE SET NULL,
    branch          TEXT,
    path            TEXT,

    effect          audit_effect,
    reason          TEXT,
    rule_id         TEXT,
    guard           TEXT,

    policy_version  TEXT NOT NULL,
    request_id      TEXT,
    task_id         UUID REFERENCES tasks(id) ON DELETE SET NULL,

    detail          JSONB
);

CREATE INDEX idx_audit_occurred ON audit_log(occurred_at DESC);
CREATE INDEX idx_audit_actor ON audit_log(actor_user_id, occurred_at DESC);
CREATE INDEX idx_audit_repo ON audit_log(repository_id, occurred_at DESC);
CREATE INDEX idx_audit_request ON audit_log(request_id);

-- Append-only enforcement, so an application bug cannot rewrite history.
--
-- Note what this costs: DO INSTEAD NOTHING is *silent*. A DELETE reports
-- "DELETE 0" and succeeds, so an operator purging old records is told it worked
-- and nothing happened. This table is not partitioned and has no retention
-- mechanism; it grows without bound. Purging today means dropping the rule,
-- deleting, and recreating it — see docs/SCALING.md, which also describes the
-- migration to a partitioned table that would make DROP PARTITION the answer.
CREATE RULE audit_log_no_update AS ON UPDATE TO audit_log DO INSTEAD NOTHING;
CREATE RULE audit_log_no_delete AS ON DELETE TO audit_log DO INSTEAD NOTHING;
