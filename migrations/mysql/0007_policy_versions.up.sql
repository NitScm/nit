-- Which bundles have been in force, and when. See the PostgreSQL file at this
-- version for why this exists.
--
-- VARCHAR rather than TEXT on the key columns: MySQL cannot index a TEXT column
-- without a prefix length, and a primary key over a prefix would let two
-- different versions collide on their first 191 bytes. A version is
-- "sha256:" plus 16 hex characters, so the width is generous.
CREATE TABLE IF NOT EXISTS policy_versions (
    tenant_id       VARCHAR(64)  NOT NULL,
    version         VARCHAR(128) NOT NULL,

    first_loaded_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    last_loaded_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),

    source          TEXT         NOT NULL,
    ref             VARCHAR(255) NOT NULL DEFAULT '',
    commit_sha      VARCHAR(64)  NOT NULL DEFAULT '',

    PRIMARY KEY (tenant_id, version),

    -- Declared inside the table: MySQL has no IF NOT EXISTS for CREATE INDEX,
    -- and a migration runs once so it can simply say what it wants.
    KEY policy_versions_recent (tenant_id, first_loaded_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin ROW_FORMAT=DYNAMIC;
