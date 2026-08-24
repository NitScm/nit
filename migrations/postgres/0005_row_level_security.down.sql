DO $$
DECLARE
    target TEXT;
BEGIN
    FOREACH target IN ARRAY ARRAY[
        'users', 'workspaces', 'repositories', 'sync_points',
        'tasks', 'artifacts', 'audit_log', 'partition_leases'
    ] LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', target);
        EXECUTE format('ALTER TABLE %I NO FORCE ROW LEVEL SECURITY', target);
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', target);
    END LOOP;
END
$$;

DROP FUNCTION IF EXISTS current_tenant();
