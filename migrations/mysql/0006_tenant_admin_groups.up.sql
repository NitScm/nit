-- Give each tenant its own operators. The reasoning is in
-- migrations/postgres/0006_tenant_admin_groups.up.sql and is the same here.
--
-- What differs is what protects the table: PostgreSQL adds a row-level security
-- policy, and this backend has none, so the tenant filter in each query is the
-- only layer — as it is for every other table here.

CREATE TABLE tenant_admin_groups (
    tenant_id  VARCHAR(64) NOT NULL,

    -- A group id from that tenant's policy bundle. Not a foreign key: groups
    -- live in files under version control, not in this database.
    group_id   VARCHAR(255) NOT NULL,

    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),

    PRIMARY KEY (tenant_id, group_id),
    CONSTRAINT tenant_admin_groups_tenant_fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin ROW_FORMAT=DYNAMIC;
