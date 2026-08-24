-- No equivalent, and none is possible.
--
-- The PostgreSQL migration of this version installs row-level security so that
-- a query on a connection with no tenant stamped on it returns nothing rather
-- than another tenant's rows. Neither MySQL nor MariaDB has row-level security:
-- the usual substitute is a view per tenant, which cannot be written for a
-- tenant set that changes at runtime, and neither engine has a session-scoped
-- policy mechanism to key one on.
--
-- So on this backend the tenant filter in each query is the only layer. Every
-- one of them is correct today — the conformance suite is what says so — but
-- there is no second net under them, and an operator choosing between backends
-- deserves to know which one they are on.
--
-- The version exists so the two dialects stay at the same numbers. A migration
-- present on one side and absent on the other would make
-- `nitctl migrate -status` describe two different schemas with one list.

SELECT 'no row-level security on this backend; see the comment above' AS note;
