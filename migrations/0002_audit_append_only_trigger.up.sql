-- Make the audit trail's append-only guarantee fail loudly.
--
-- 0001 used rewrite rules: DO INSTEAD NOTHING. They do stop an application bug
-- from rewriting history, but they are silent. A purge reports "DELETE 0",
-- exits zero, and removes nothing, so an operator is told the cleanup worked
-- when it did not — verified against a live database before this migration was
-- written.
--
-- A trigger raises instead. Someone who tries to delete audit records now gets
-- an error naming the reason, which is the behaviour they needed in the first
-- place; and someone who genuinely means to purge disables the trigger, which
-- is a deliberate act rather than a command that appears to work.
--
-- It is also the portable form. PostgreSQL rewrite rules have no equivalent in
-- MySQL or MariaDB, while all three have triggers, so this removes one of the
-- obstacles to a second backend rather than adding one.

DROP RULE IF EXISTS audit_log_no_update ON audit_log;
DROP RULE IF EXISTS audit_log_no_delete ON audit_log;

CREATE OR REPLACE FUNCTION audit_log_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only: % is not permitted', TG_OP
        USING HINT = 'to purge, disable trigger audit_log_append_only on audit_log, delete, then re-enable it';
END;
$$;

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

-- TRUNCATE bypasses row triggers entirely, so it needs its own statement-level
-- one. Without it the strongest guarantee in the schema is one word away from
-- being bypassed by accident.
CREATE TRIGGER audit_log_append_only_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();
