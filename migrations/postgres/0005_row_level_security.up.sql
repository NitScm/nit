-- Make forgetting the tenant a failed read instead of somebody else's rows.
--
-- Every query already filters on tenant_id, and every one of them is correct
-- today. This is the second layer, for the day one of them is not: a
-- context-carried tenant is easy to forget to pass, and a forgotten one is not
-- an error — it is a silent cross-tenant read, which is the single failure this
-- product cannot survive.
--
-- The application stamps every connection with the tenant of the request about
-- to use it (internal/store/postgres, PrepareConn). A context with no tenant
-- stamps the empty string, which matches nothing, so a caller that forgot reads
-- zero rows.
--
-- ============================================================================
-- READ THIS BEFORE BELIEVING IT WORKS
-- ============================================================================
--
-- Row-level security fails *silently*. Two roles bypass it entirely:
--
--   * a superuser, always;
--   * a table's owner, unless the table is FORCEd — and even FORCE does not
--     stop a superuser.
--
-- Verified on PostgreSQL 17 while writing this: connected as the superuser that
-- created the tables, a policy restricting rows to one tenant returned every
-- row, with ENABLE and with FORCE alike. Under an ordinary role it returned one
-- row with the setting present and **none** with it absent.
--
-- So nit must connect as a role that neither owns these tables nor is a
-- superuser. docs/CONFIGURATION.md carries the grants. A deployment that skips
-- that is not half-protected, it is unprotected — which is why the server
-- reports on start-up whether the policies actually apply to it, rather than
-- assuming that installing them was enough.
--
-- ============================================================================
-- WHY sessions IS NOT HERE
-- ============================================================================
--
-- sessions is the table that resolves the tenant: authentication looks a token
-- up before anybody knows whose it is. A policy on it would make every
-- authentication return zero rows, and nobody could log in — the bootstrap
-- cannot be protected by the thing it bootstraps.
--
-- What protects it is unchanged and is enough: a token hash is 32 unguessable
-- bytes under a unique constraint, so finding another tenant's session means
-- guessing their token, which is the same barrier as impersonating them
-- directly.
--
-- tenants is absent for the same shape of reason: it is the registry, not a
-- tenant's data.

CREATE OR REPLACE FUNCTION current_tenant() RETURNS TEXT
LANGUAGE sql STABLE AS $$
    -- The second argument makes a missing setting NULL rather than an error,
    -- so a query on an unstamped connection returns nothing instead of failing
    -- with something an operator would have to decode.
    SELECT current_setting('nit.tenant', true)
$$;

DO $$
DECLARE
    target TEXT;
BEGIN
    FOREACH target IN ARRAY ARRAY[
        'users', 'workspaces', 'repositories', 'sync_points',
        'tasks', 'artifacts', 'audit_log', 'partition_leases'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', target);

        -- FORCE, or the owner — which is whoever ran the migrations — reads
        -- everything and the policies are decoration.
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', target);

        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I USING (tenant_id = current_tenant()) '
            'WITH CHECK (tenant_id = current_tenant())', target);
    END LOOP;
END
$$;
