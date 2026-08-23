-- Make the audit trail's append-only guarantee fail loudly.
--
-- The PostgreSQL version of this migration replaced silent rewrite rules with a
-- trigger that raises. This backend has no such history — 0001 shipped without
-- enforcement and gets it here, so the two schemas arrive at the same guarantee
-- at the same version number.
--
-- Each trigger body is a single SIGNAL statement rather than a BEGIN ... END
-- block. That is deliberate: the migrator splits a file into statements on
-- semicolons, and a compound body would need a DELIMITER, which is a mysql(1)
-- client directive the server never sees.
--
-- SQLSTATE '45000' is the value the manual reserves for an unhandled
-- user-defined exception. The message names the reason, because the person
-- reading it is usually an operator who meant to purge and needs to be told
-- how.

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
    FOR EACH ROW SIGNAL SQLSTATE '45000'
    SET MESSAGE_TEXT = 'audit_log is append-only: UPDATE is not permitted; to purge, DROP TRIGGER audit_log_no_delete, delete, then recreate it';

CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
    FOR EACH ROW SIGNAL SQLSTATE '45000'
    SET MESSAGE_TEXT = 'audit_log is append-only: DELETE is not permitted; to purge, DROP TRIGGER audit_log_no_delete, delete, then recreate it';

-- TRUNCATE is not covered, and cannot be.
--
-- PostgreSQL has a statement-level BEFORE TRUNCATE trigger; neither MySQL nor
-- MariaDB fires any trigger on TRUNCATE, and neither has a statement-level
-- trigger to hang one on. So on this backend the strongest guarantee in the
-- schema is one word away from being bypassed, and no amount of SQL closes it.
--
-- The mitigation is a privilege, not a constraint: TRUNCATE requires the DROP
-- privilege, so an application account granted only SELECT, INSERT, UPDATE and
-- DELETE cannot issue one. docs/CONFIGURATION.md carries the GRANT. This is
-- recorded rather than papered over — an operator choosing between backends
-- deserves to know that this one holds the line with a permission where the
-- other holds it with a trigger.
