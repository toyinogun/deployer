-- Spec 0021. The public edge: the address an audited action was attributed to.
--
-- Purely additive, one nullable column on one existing table, so a previous
-- binary reads the schema it leaves behind unharmed and every current insert
-- still compiles against it.
--
-- No index. audit_log_occurred still serves every read the platform makes, and
-- an index here would cost a write on every audited action for a query that has
-- no caller. The daily sweep in AC-18 scans the whole table once a day, which is
-- the right cost for a table this size and is stated rather than discovered.

-- +goose Up

-- Null on a row the platform wrote itself: a suspension sweep, a reconcile
-- drive, a scheduled backup run. Null again on any row past
-- DEPLOYER_RETENTION_DAYS, which the sweep sets back in place without deleting
-- the row. After the window a nulled row and a platform written row are
-- indistinguishable, which is accepted (AC-17, AC-18, AC-18a).
ALTER TABLE audit_log ADD COLUMN client_address TEXT;

-- +goose Down
ALTER TABLE audit_log DROP COLUMN client_address;
