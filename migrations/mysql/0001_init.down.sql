-- Foreign keys make the order matter: children before parents.
--
-- No DROP TYPE section, unlike PostgreSQL: MySQL enums are column types and
-- disappear with their tables.

DROP TRIGGER IF EXISTS audit_log_no_update;
DROP TRIGGER IF EXISTS audit_log_no_delete;

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS sync_points;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS workspaces;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
