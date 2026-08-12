-- +goose Up
-- Spec 0007 turns an account into a person: an email, a password, a verified
-- state, and an admin flag. Every other table keeps pointing at accounts(id)
-- exactly as it did, so ownership does not change shape.
--
-- Purely additive: every added column is nullable or carries a constant default,
-- and both new tables are empty, so the previous binary reads this schema without
-- harm and no existing row needs a value.

ALTER TABLE accounts ADD COLUMN email TEXT;
ALTER TABLE accounts ADD COLUMN password_hash TEXT;
ALTER TABLE accounts ADD COLUMN email_verified_at TEXT;
ALTER TABLE accounts ADD COLUMN is_admin INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1));
ALTER TABLE accounts ADD COLUMN display_name TEXT;

-- A partial index rather than a column constraint, because SQLite cannot
-- ADD COLUMN ... UNIQUE. The predicate is also what lets every token only
-- account, the bootstrap one included, keep a null email.
CREATE UNIQUE INDEX accounts_email ON accounts(email) WHERE email IS NOT NULL;

-- Shaped like api_tokens on purpose: a hashed value, a revocation stamp, and a
-- last use. The raw cookie value never reaches this table.
CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    token_hash   TEXT NOT NULL UNIQUE,
    expires_at   TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

CREATE INDEX sessions_by_account ON sessions(account_id, created_at DESC);

-- One table serving both single use link kinds. Purpose is part of the lookup,
-- never just the hash, so a token minted to verify an address cannot be spent to
-- reset the password on it.
CREATE TABLE email_tokens (
    id          TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    purpose     TEXT NOT NULL CHECK (purpose IN ('verify_email', 'password_reset')),
    token_hash  TEXT NOT NULL UNIQUE,
    expires_at  TEXT NOT NULL,
    consumed_at TEXT,
    created_at  TEXT NOT NULL
) STRICT;

-- At most one live link per account per purpose: a resend stamps the previous
-- row consumed first, the same shape as api_tokens_live_name.
CREATE UNIQUE INDEX email_tokens_live ON email_tokens(account_id, purpose) WHERE consumed_at IS NULL;

-- +goose Down
DROP TABLE email_tokens;
DROP TABLE sessions;
DROP INDEX accounts_email;
ALTER TABLE accounts DROP COLUMN display_name;
ALTER TABLE accounts DROP COLUMN is_admin;
ALTER TABLE accounts DROP COLUMN email_verified_at;
ALTER TABLE accounts DROP COLUMN password_hash;
ALTER TABLE accounts DROP COLUMN email;
