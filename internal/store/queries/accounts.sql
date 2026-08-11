-- name: CreateAccount :one
INSERT INTO accounts (id, name, created_at, updated_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetAccount :one
SELECT * FROM accounts WHERE id = ?;

-- name: CreateAPIToken :one
INSERT INTO api_tokens (id, account_id, name, token_hash, token_prefix, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- A token resolves to an account only while it is live: not revoked, not past
-- its expiry, and belonging to an account that is not locked out. Unknown,
-- revoked, expired and disabled are all the same empty result to the caller.
-- name: ResolveToken :one
SELECT sqlc.embed(accounts), sqlc.embed(api_tokens)
FROM api_tokens
JOIN accounts ON accounts.id = api_tokens.account_id
WHERE api_tokens.token_hash = @token_hash
  AND api_tokens.revoked_at IS NULL
  AND (api_tokens.expires_at IS NULL OR api_tokens.expires_at > @now)
  AND accounts.disabled_at IS NULL;

-- name: TouchTokenLastUsed :exec
UPDATE api_tokens SET last_used_at = @now, updated_at = @now WHERE id = @id;

-- name: RevokeAPIToken :execrows
UPDATE api_tokens
SET revoked_at = @now, updated_at = @now
WHERE id = @id AND revoked_at IS NULL;

-- name: InsertAuditLog :exec
INSERT INTO audit_log (id, account_id, action, target_type, target_id, outcome, reason, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
