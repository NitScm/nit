DROP RULE IF EXISTS audit_log_no_delete ON audit_log;
DROP RULE IF EXISTS audit_log_no_update ON audit_log;

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS sync_points;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS workspaces;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;

DROP TYPE IF EXISTS audit_effect;
DROP TYPE IF EXISTS artifact_kind;
DROP TYPE IF EXISTS task_state;
DROP TYPE IF EXISTS task_kind;
