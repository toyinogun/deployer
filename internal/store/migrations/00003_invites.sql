-- Spec 0015. Invite only registration: the single thing that authorises an
-- account now that reaching the register page no longer proves anything.
--
-- Purely additive. Nothing existing moves, every column here is nullable or
-- written at insert, and the previous binary reads the resulting schema without
-- harm because it never looks at this table (AC-12).

-- +goose Up

-- One invite, shaped like the single use secret email_tokens already is: the
-- code is stored as a hash and matched on that hash plus its state, never on the
-- hash alone. The raw value exists between minting and the one response that
-- shows it, and nowhere else (AC-14).
--
-- There is no status column. live, spent, revoked and expired are derived from
-- consumed_at, revoked_at and expires_at against the clock, so nothing sweeps
-- the table and a clock change cannot leave a stale stored state behind.
CREATE TABLE invites (
    id          TEXT PRIMARY KEY,
    code_hash   TEXT NOT NULL UNIQUE,
    -- The admin's own words about who this went to, bounded above this layer.
    -- The only record of that, since the invite is not bound to an address.
    note        TEXT,
    -- Null means the platform minted it at boot, the same way the bootstrap
    -- account carries a null email.
    created_by  TEXT REFERENCES accounts(id) ON DELETE RESTRICT,
    expires_at  TEXT NOT NULL,
    consumed_at TEXT,
    -- The account this invite created, stamped in the same transaction as the
    -- insert that created it.
    --
    -- Deferred, and that is load bearing rather than a style choice. The spend
    -- is a guarded update that runs before the account insert, so that a dead
    -- invite costs no account row at all, which means this column names a row
    -- that does not exist yet for the length of the transaction. An immediate
    -- check fails on the update itself. Deferred, it is checked at COMMIT, by
    -- which point the insert has run or the whole thing has rolled back.
    consumed_by TEXT REFERENCES accounts(id) ON DELETE RESTRICT
                DEFERRABLE INITIALLY DEFERRED,
    revoked_at  TEXT,
    created_at  TEXT NOT NULL
) STRICT;

-- The admin list is newest first and unpaginated: this grows with every
-- registration ever made rather than with headcount, which at the platform's
-- stated scale is tens of rows for years (spec 0015, Consequences).
CREATE INDEX invites_by_created ON invites(created_at DESC);

-- +goose Down
DROP TABLE invites;
