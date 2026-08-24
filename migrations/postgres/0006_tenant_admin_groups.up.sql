-- Give each tenant its own operators.
--
-- `server.admin_groups` is process configuration: one list, for whoever the
-- process serves. In a hosted service each customer needs their own
-- administrators, and the group names come from *their* bundle.
--
-- ============================================================================
-- WHY THIS IS NOT IN THE POLICY BUNDLE
-- ============================================================================
--
-- D28 decided that the permission to read the operations API is configuration
-- rather than policy, for a specific reason: the console is the tool for
-- diagnosing a broken bundle, so putting the permission to use it inside the
-- bundle would make that tool depend on the thing it exists to debug.
--
-- That reasoning still holds, so this is not "move it into the bundle". It is
-- the same configuration, scoped to a tenant and held by the control plane —
-- outside the customer's bundle, which is what keeps the console usable when
-- their rules are broken.
--
-- The group *names* still come from their bundle, because that is where groups
-- are defined. What lives here is which of those names may operate, and that
-- is ours to decide rather than theirs.
--
-- ============================================================================
-- WHAT THIS IS NOT
-- ============================================================================
--
-- It is not the account model. saas-thinking/03 gap 3 asks for "a customer
-- account, distinct from a policy user" — roles, invitations, ownership,
-- separate from the bundle's identities. That is a product, and it is gap 7.
--
-- This is the narrow half that makes a hosted control plane stop granting one
-- customer's administrators access to another's operations API. A deployment
-- with no rows here behaves exactly as before, on server.admin_groups.

CREATE TABLE tenant_admin_groups (
    tenant_id  TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,

    -- A group id from that tenant's policy bundle. Not a foreign key: groups
    -- live in files under version control, not in this database, and a row
    -- naming a group that has been renamed is a permission that stops applying
    -- rather than a constraint violation at reload time.
    group_id   TEXT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (tenant_id, group_id)
);

ALTER TABLE tenant_admin_groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_admin_groups FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON tenant_admin_groups
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());
