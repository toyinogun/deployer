-- Spec 0007. The identity half of the account: registration, sessions, single use
-- email links, and the token management surface a person drives from a session.

-- Registration writes the account's own id into name, so the NOT NULL UNIQUE
-- constraint that column was designed with is satisfied by construction and never
-- becomes a user visible collision. The human label lives in display_name.
--
-- The first admin rule is computed inside this one statement rather than by a
-- count and then an insert (AC-4). One statement is atomic on its own, so two
-- concurrent first registrations cannot both come out admin, and no transaction
-- has to read and then upgrade its lock to write.
-- name: CreateIdentityAccount :one
INSERT INTO accounts (id, name, email, password_hash, display_name, is_admin, created_at, updated_at)
SELECT @id, @id, @email, @password_hash, @display_name,
       CASE WHEN EXISTS (SELECT 1 FROM accounts WHERE email IS NOT NULL) THEN 0 ELSE 1 END,
       @now, @now
RETURNING *;

-- name: GetAccountByEmail :one
SELECT * FROM accounts WHERE email = @email;

-- name: ListAccounts :many
SELECT * FROM accounts ORDER BY created_at DESC;

-- name: MarkEmailVerified :execrows
UPDATE accounts SET email_verified_at = @now, updated_at = @now
WHERE id = @id AND email_verified_at IS NULL;

-- Stamping the account connected is one conditional statement rather than a read
-- followed by a write, so two GET /connect requests arriving at once leave
-- exactly one stamp and neither has to hold a transaction open to find out
-- (spec 0023, AC-4a). A second call matches no row and changes nothing, which is
-- the ordinary case on every visit after the first.
-- name: MarkAccountConnected :execrows
UPDATE accounts SET connected_at = @now, updated_at = @now
WHERE id = @id AND connected_at IS NULL;

-- name: SetPasswordHash :execrows
UPDATE accounts SET password_hash = @password_hash, updated_at = @now WHERE id = @id;

-- name: SetAccountDisabled :execrows
UPDATE accounts SET disabled_at = @disabled_at, updated_at = @now WHERE id = @id;

-- name: CreateSession :one
INSERT INTO sessions (id, account_id, token_hash, expires_at, created_at, updated_at)
VALUES (@id, @account_id, @token_hash, @expires_at, @now, @now)
RETURNING *;

-- A session resolves only while it is live: not revoked, not past its rolling
-- expiry, and belonging to an account that is not locked out. Every failing case
-- is the same empty result, exactly as ResolveToken is.
-- name: ResolveSession :one
SELECT sqlc.embed(accounts), sqlc.embed(sessions)
FROM sessions
JOIN accounts ON accounts.id = sessions.account_id
WHERE sessions.token_hash = @token_hash
  AND sessions.revoked_at IS NULL
  AND sessions.expires_at > @now
  AND accounts.disabled_at IS NULL;

-- Rolling expiry: every authenticated request pushes the session forward (AC-9).
-- name: TouchSession :exec
UPDATE sessions SET last_used_at = @now, expires_at = @expires_at, updated_at = @now WHERE id = @id;

-- name: RevokeSession :execrows
UPDATE sessions SET revoked_at = @now, updated_at = @now WHERE id = @id AND revoked_at IS NULL;

-- Used by disable and by a password change, both of which must leave no live
-- session behind, in the same transaction as the change itself (AC-10).
-- name: RevokeAccountSessions :execrows
UPDATE sessions SET revoked_at = @now, updated_at = @now
WHERE account_id = @account_id AND revoked_at IS NULL;

-- name: CreateEmailToken :one
INSERT INTO email_tokens (id, account_id, purpose, token_hash, expires_at, created_at)
VALUES (@id, @account_id, @purpose, @token_hash, @expires_at, @now)
RETURNING *;

-- Spending a link is one statement, matched and consumed together, so a link
-- presented twice at once is spent once without a transaction that reads and
-- then writes.
-- name: ConsumeEmailToken :one
UPDATE email_tokens SET consumed_at = @now
WHERE token_hash = @token_hash
  AND purpose = @purpose
  AND consumed_at IS NULL
  AND expires_at > @now
RETURNING *;

-- Supersedes whatever link the account holds for that purpose, so a resend leaves
-- exactly one live link rather than two working ones (AC-6).
-- name: ConsumeLiveEmailTokens :execrows
UPDATE email_tokens SET consumed_at = @now
WHERE account_id = @account_id AND purpose = @purpose AND consumed_at IS NULL;

-- name: RevokeAccountEmailTokens :execrows
UPDATE email_tokens SET consumed_at = @now
WHERE account_id = @account_id AND consumed_at IS NULL;

-- No pagination: one live name per account is already enforced, so a token list
-- is small by construction (AC-13).
-- name: ListLiveAPITokens :many
SELECT * FROM api_tokens
WHERE account_id = @account_id
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > @now)
ORDER BY created_at DESC;

-- name: GetAPIToken :one
SELECT * FROM api_tokens WHERE id = @id;
