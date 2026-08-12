-- name: CreateUpload :one
INSERT INTO uploads (id, account_id, path, size_bytes, sha256, fetch_token_hash, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetUpload :one
SELECT * FROM uploads WHERE id = ?;

-- Single use, enforced by the update itself rather than a read then a write:
-- zero rows back means the token was already spent or has expired.
-- name: RedeemUpload :one
UPDATE uploads
SET redeemed_at = @now, updated_at = @now
WHERE fetch_token_hash = @fetch_token_hash
  AND redeemed_at IS NULL
  AND expires_at > @now
RETURNING *;

-- Replaces the token a build presents and clears any previous redemption, so a
-- resumed or retried build mints a fresh usable token rather than finding a
-- spent one (spec 0004, Value sourcing).
-- name: SetUploadFetchToken :execrows
UPDATE uploads
SET fetch_token_hash = @fetch_token_hash, redeemed_at = NULL, updated_at = @now
WHERE id = @id;

-- name: DeleteEventsOfSweptDeployments :execrows
DELETE FROM deployment_events
WHERE deployment_id IN (
    SELECT id FROM deployments
    WHERE state IN ('failed', 'cancelled')
      AND finished_at IS NOT NULL
      AND finished_at < @cutoff
);

-- name: DeleteSweptDeployments :execrows
DELETE FROM deployments
WHERE state IN ('failed', 'cancelled')
  AND finished_at IS NOT NULL
  AND finished_at < @cutoff;

-- name: DeleteAgedEvents :execrows
DELETE FROM deployment_events WHERE occurred_at < @cutoff;

-- An upload still named by a surviving deployment stays: nothing cascades, so
-- the sweep has to leave the parent's children intact.
-- name: DeleteStaleUploads :execrows
DELETE FROM uploads
WHERE uploads.created_at < @upload_cutoff
  AND (redeemed_at IS NOT NULL OR expires_at < @now)
  AND id NOT IN (SELECT upload_id FROM deployments WHERE upload_id IS NOT NULL);
