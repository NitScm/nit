-- Back to the silent rules of 0001.

DROP TRIGGER IF EXISTS audit_log_append_only_truncate ON audit_log;
DROP TRIGGER IF EXISTS audit_log_append_only ON audit_log;
DROP FUNCTION IF EXISTS audit_log_append_only();

CREATE RULE audit_log_no_update AS ON UPDATE TO audit_log DO INSTEAD NOTHING;
CREATE RULE audit_log_no_delete AS ON DELETE TO audit_log DO INSTEAD NOTHING;
