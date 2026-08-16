-- Spec 0015. The invite that authorises a registration.
--
-- Every lookup carries the live predicate in full, without exception:
-- consumed_at IS NULL AND revoked_at IS NULL AND expires_at > now. The spend
-- guard and the revoke guard both carry all three clauses, so neither can act on
-- a row the other has already ended (AC-4, AC-5, AC-7).

-- name: CreateInvite :one
INSERT INTO invites (id, code_hash, note, email, created_by, expires_at, created_at)
VALUES (@id, @code_hash, @note, @email, @created_by, @expires_at, @now)
RETURNING *;

-- Whether an address already has an account. Read inside the same transaction
-- that mints, so a registration landing between the check and the insert cannot
-- produce an invite bound to an address that now has an account (spec 0025,
-- AC-3).
-- name: AccountExistsByEmail :one
SELECT EXISTS (SELECT 1 FROM accounts WHERE email = @email);

-- The lookup registration makes before it does any other work. A code that is
-- unknown, spent, revoked or expired is the same empty result, so the caller
-- cannot tell which kind of bad code they hold (AC-2).
--
-- The bound address is part of that same predicate rather than a comparison
-- above it (spec 0025, AC-8): a live invite presented with the wrong address is
-- the same empty result as a dead code, costs the same work, and no ordering of
-- checks can tell the two apart. An unbound invite carries null here and matches
-- every candidate, which is what keeps every invite minted before spec 0025
-- working (AC-16).
-- name: LiveInviteByCodeHash :one
SELECT * FROM invites
WHERE code_hash = @code_hash
  AND (email IS NULL OR email = @candidate)
  AND consumed_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > @now;

-- Spending is a conditional update returning a row count, run inside the same
-- transaction as the account insert. The predicate is the full live one, so
-- nothing rests on the earlier lookup still being true by the time this runs:
-- two registrations racing on one code end with exactly one account (AC-4).
-- name: SpendInvite :execrows
UPDATE invites SET consumed_at = @now, consumed_by = @consumed_by
WHERE id = @id
  AND consumed_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > @now;

-- Revoking carries the same guard, so revoking an invite that is already spent
-- or expired touches nothing and is refused not_found (AC-7).
-- name: RevokeInvite :execrows
UPDATE invites SET revoked_at = @now
WHERE id = @id
  AND consumed_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > @now;

-- The admin list, newest first, with the two names it displays resolved here
-- rather than by a read per row. The code hash is in the row type and never
-- reaches a page: the layer above projects it away (AC-8).
-- name: ListInvites :many
SELECT i.id, i.note, i.email, i.expires_at, i.consumed_at, i.revoked_at, i.created_at,
       issuer.display_name AS issuer_name,
       spender.email       AS spender_email
FROM invites i
LEFT JOIN accounts issuer  ON issuer.id  = i.created_by
LEFT JOIN accounts spender ON spender.id = i.consumed_by
ORDER BY i.created_at DESC;

-- The two reads the startup bootstrap decides from. Either one being true means
-- the platform mints nothing and logs nothing (AC-13).
-- name: AnyAccountHasEmail :one
SELECT EXISTS (SELECT 1 FROM accounts WHERE email IS NOT NULL);

-- name: AnyLiveBootstrapInvite :one
SELECT EXISTS (
    SELECT 1 FROM invites
    WHERE created_by IS NULL
      AND consumed_at IS NULL
      AND revoked_at IS NULL
      AND expires_at > @now
);
