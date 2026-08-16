-- Spec 0024. OAuth for clients that will not hold a token: the registered
-- clients, the short lived authorization codes, and the link from a token back
-- to the client it was granted to.
--
-- Purely additive (AC-29). Two new tables, one nullable column on api_tokens
-- with no default, and two indexes, so a previous binary reads the schema it
-- leaves behind unharmed and no existing row needs a value.

-- +goose Up

-- A client anyone on the internet may create. Registration grants nothing: the
-- row is inert until some signed in account approves it on the console, which
-- is what approved_at records. Nothing stored here is ever treated as a fact
-- about who the client is.
CREATE TABLE oauth_clients (
    -- The client_id handed back at registration.
    id            TEXT PRIMARY KEY,
    -- What the client called itself. Attacker supplied text, escaped wherever
    -- it is shown and bounded before it becomes a token name.
    name          TEXT NOT NULL,
    -- A JSON array, validated at registration (AC-5) and stored verbatim, so
    -- the exact registered string is what a redirect_uri is compared against
    -- (AC-10b).
    redirect_uris TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    -- Null until some account approves this client. Null and older than the
    -- window held in Go is what the daily sweep deletes; a stamped row is never
    -- swept (AC-8).
    approved_at   TEXT
) STRICT;

-- One authorization code. It lives 60 seconds and is spent once. The raw code
-- exists only in the redirect the browser follows; what is stored is its hash,
-- the same way api_tokens.token_hash is.
CREATE TABLE oauth_codes (
    code_hash      TEXT PRIMARY KEY,
    client_id      TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    -- The matched registered URI, compared again at the token endpoint (AC-18).
    redirect_uri   TEXT NOT NULL,
    -- S256 only. There is no path to a code without one (AC-11).
    code_challenge TEXT NOT NULL,
    -- The RFC 8707 value the authorize request carried, compared again at the
    -- token endpoint so a token is bound to the resource it was asked for.
    resource       TEXT NOT NULL,
    -- The token this code issued, so presenting the code a second time can
    -- revoke it (AC-16a).
    token_id       TEXT REFERENCES api_tokens(id) ON DELETE RESTRICT,
    expires_at     TEXT NOT NULL,
    -- Single use. Stamped by one conditional UPDATE, never by a read followed
    -- by a write, so two token requests arriving together cannot both mint
    -- (AC-18a).
    consumed_at    TEXT,
    created_at     TEXT NOT NULL
) STRICT;

-- Null for every token minted by hand, which is every token that exists today.
-- SQLite allows a REFERENCES clause on an added column because the default is
-- null.
ALTER TABLE api_tokens ADD COLUMN oauth_client_id TEXT
    REFERENCES oauth_clients(id) ON DELETE RESTRICT;

-- One live token per client per account, enforced here rather than by
-- remembering to check. A new grant revokes the previous one in the same
-- transaction, so this index never sees two (AC-19b).
CREATE UNIQUE INDEX api_tokens_live_client
    ON api_tokens(account_id, oauth_client_id)
    WHERE revoked_at IS NULL AND oauth_client_id IS NOT NULL;

-- So the sweep of dead codes is a range scan rather than a table scan.
CREATE INDEX oauth_codes_expiry ON oauth_codes(expires_at);

-- +goose Down
DROP INDEX oauth_codes_expiry;
DROP INDEX api_tokens_live_client;
DROP TABLE oauth_codes;
ALTER TABLE api_tokens DROP COLUMN oauth_client_id;
DROP TABLE oauth_clients;
