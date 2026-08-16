-- Spec 0024. The authorization server's own rows: the clients a stranger may
-- register, the codes an approval issues, and the grant that turns one into an
-- ordinary api_tokens row.

-- name: CreateOAuthClient :one
INSERT INTO oauth_clients (id, name, redirect_uris, created_at)
VALUES (@id, @name, @redirect_uris, @now)
RETURNING *;

-- name: GetOAuthClient :one
SELECT * FROM oauth_clients WHERE id = @id;

-- Stamping a client approved is one conditional statement rather than a read
-- followed by a write, the same shape MarkAccountConnected uses. A second
-- approval by anyone matches no row, changes nothing, and is not an error:
-- approved_at records that some account once approved this client, never which.
-- name: ApproveOAuthClient :execrows
UPDATE oauth_clients SET approved_at = @now
WHERE id = @id AND approved_at IS NULL;

-- The daily sweep (AC-8). A client nobody ever approved is a row a stranger
-- created, so it dies once the resumable window passes. A stamped row is never
-- touched here. Deleting a client cascades its codes.
-- name: DeleteUnapprovedOAuthClients :execrows
DELETE FROM oauth_clients
WHERE approved_at IS NULL AND created_at < @cutoff;

-- name: CreateOAuthCode :one
INSERT INTO oauth_codes (code_hash, client_id, account_id, redirect_uri, code_challenge, resource, expires_at, created_at)
VALUES (@code_hash, @client_id, @account_id, @redirect_uri, @code_challenge, @resource, @expires_at, @now)
RETURNING *;

-- Read for the replay case only (AC-16a): a code that is already consumed still
-- has to be found, so the token it issued can be revoked. The exchange itself
-- never reads first, it consumes.
-- name: GetOAuthCode :one
SELECT * FROM oauth_codes WHERE code_hash = @code_hash;

-- Spending a code is one conditional statement and only a row count of one
-- proceeds to mint (AC-18a). A read followed by a write would let two token
-- requests arriving together both see the code unconsumed and both mint from
-- it. The expiry is part of the same condition, so an expired code is spent by
-- nobody.
-- name: ConsumeOAuthCode :one
UPDATE oauth_codes SET consumed_at = @now
WHERE code_hash = @code_hash AND consumed_at IS NULL AND expires_at > @now
RETURNING *;

-- name: SetOAuthCodeToken :execrows
UPDATE oauth_codes SET token_id = @token_id WHERE code_hash = @code_hash;

-- Revokes every live token this account already holds for this client, so the
-- partial unique index never sees two. Runs in the same transaction as the mint
-- that replaces it (AC-19a, AC-19b).
-- name: RevokeLiveClientTokens :execrows
UPDATE api_tokens
SET revoked_at = @now, updated_at = @now
WHERE account_id = @account_id AND oauth_client_id = @oauth_client_id AND revoked_at IS NULL;

-- name: CreateClientAPIToken :one
INSERT INTO api_tokens (id, account_id, name, token_hash, token_prefix, oauth_client_id, created_at, updated_at)
VALUES (@id, @account_id, @name, @token_hash, @token_prefix, @oauth_client_id, @now, @now)
RETURNING *;
