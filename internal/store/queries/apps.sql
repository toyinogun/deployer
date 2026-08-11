-- name: CreateApp :one
INSERT INTO apps (id, account_id, name, slug, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetApp :one
SELECT * FROM apps WHERE id = @id AND deleted_at IS NULL;

-- name: GetAppBySlug :one
SELECT * FROM apps WHERE slug = @slug AND deleted_at IS NULL;

-- A page of an account's live apps, newest first. The cursor is the last id of
-- the previous page; an empty cursor starts at the top.
-- name: ListAppsByAccount :many
SELECT * FROM apps
WHERE account_id = @account_id
  AND deleted_at IS NULL
  AND (@cursor = '' OR id < @cursor)
ORDER BY id DESC
LIMIT @page_limit;

-- name: SoftDeleteApp :execrows
UPDATE apps
SET deleted_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- name: SetAppCurrentRelease :execrows
UPDATE apps SET current_release_id = @release_id, updated_at = @now WHERE id = @id;

-- name: CountInFlightDeploymentsForApp :one
SELECT COUNT(*) FROM deployments
WHERE app_id = @app_id AND state NOT IN ('healthy', 'failed', 'cancelled');

-- name: SlugExists :one
SELECT EXISTS (SELECT 1 FROM apps WHERE slug = @slug);

-- Tool responses read configuration through here and only here: a secret key is
-- listed with its flag but never with its value.
-- name: ListConfigForResponse :many
SELECT key, is_secret, CASE WHEN is_secret = 1 THEN NULL ELSE value END AS value
FROM app_config
WHERE app_id = @app_id
ORDER BY key;

-- The deploy path, and the release snapshot, are the only readers of secret
-- values.
-- name: ListConfigForDeploy :many
SELECT key, value, is_secret FROM app_config WHERE app_id = @app_id ORDER BY key;

-- name: SetConfig :exec
INSERT INTO app_config (app_id, key, value, is_secret, created_at, updated_at)
VALUES (@app_id, @key, @value, @is_secret, @now, @now)
ON CONFLICT (app_id, key) DO UPDATE
SET value = excluded.value, is_secret = excluded.is_secret, updated_at = excluded.updated_at;

-- name: UnsetConfig :execrows
DELETE FROM app_config WHERE app_id = @app_id AND key = @key;
