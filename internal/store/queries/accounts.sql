-- name: CreateAccount :one
INSERT INTO accounts (id, name, created_at, updated_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetAccount :one
SELECT * FROM accounts WHERE id = ?;

-- Names are unique, so this is how the bootstrap seeding tells "already seeded"
-- from "seed it now" without ever creating a second account (spec 0004, AC-1).
-- name: GetAccountByName :one
SELECT * FROM accounts WHERE name = ?;

-- name: CreateAPIToken :one
INSERT INTO api_tokens (id, account_id, name, token_hash, token_prefix, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- A token resolves to an account only while the token itself is live: not
-- revoked and not past its expiry. Unknown, revoked and expired are all the same
-- empty result to the caller.
--
-- A suspended account is deliberately NOT filtered here (spec 0018, AC-12). A
-- good token on a suspended account has to be told apart from a dead token so
-- the agent holding it hears account_suspended rather than a blank credential
-- error, and that decision is made once, in Go, by auth.Authenticate reading the
-- disabled_at this row carries.
-- name: ResolveToken :one
SELECT sqlc.embed(accounts), sqlc.embed(api_tokens)
FROM api_tokens
JOIN accounts ON accounts.id = api_tokens.account_id
WHERE api_tokens.token_hash = @token_hash
  AND api_tokens.revoked_at IS NULL
  AND (api_tokens.expires_at IS NULL OR api_tokens.expires_at > @now);

-- Revokes every live token an account holds under one name. The bootstrap
-- seeding uses it so rotating DEPLOYER_BOOTSTRAP_TOKEN leaves exactly one live
-- token rather than two working ones (spec 0004, AC-1).
-- name: RevokeTokensNamed :execrows
UPDATE api_tokens
SET revoked_at = @now, updated_at = @now
WHERE account_id = @account_id AND name = @name AND revoked_at IS NULL;

-- name: TouchTokenLastUsed :exec
UPDATE api_tokens SET last_used_at = @now, updated_at = @now WHERE id = @id;

-- name: RevokeAPIToken :execrows
UPDATE api_tokens
SET revoked_at = @now, updated_at = @now
WHERE id = @id AND revoked_at IS NULL;

-- name: InsertAuditLog :exec
INSERT INTO audit_log (id, account_id, action, target_type, target_id, outcome, reason, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);
