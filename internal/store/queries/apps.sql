-- name: CreateApp :one
INSERT INTO apps (id, account_id, name, slug, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- How many live apps an account holds, which is the whole of the per account
-- cap: the predicate is the one every other app read already uses, so a soft
-- deleted app frees a slot with no extra column and no reaper involvement
-- (spec 0016, Data model sketch).
-- name: CountLiveAppsByAccount :one
SELECT COUNT(*) FROM apps WHERE account_id = @account_id AND deleted_at IS NULL;

-- The same count for every account at once, for the admin listing. One grouped
-- statement rather than a count per row (spec 0016, AC-12).
-- name: CountLiveAppsPerAccount :many
SELECT account_id, COUNT(*) AS app_count FROM apps
WHERE deleted_at IS NULL
GROUP BY account_id;

-- name: GetApp :one
SELECT * FROM apps WHERE id = @id AND deleted_at IS NULL;

-- The get or create lookup every deploy starts with: an app is identified by
-- its account and the name that account gave it, and the same pair must always
-- resolve to the same row so the hostname never moves (spec 0004, AC-4).
-- name: GetAppByName :one
SELECT * FROM apps WHERE account_id = @account_id AND name = @name AND deleted_at IS NULL;

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

-- The whole app listing in one statement: no loop reading each app's newest
-- deployment, and no Kubernetes call anywhere near it (spec 0012, AC-8). The
-- projection is deliberate the way the release listing's is: no app_config and
-- no config_snapshot column is named, so no configuration value enters the
-- process at all (AC-7).
--
-- serving and last_deployment are read independently, because an app whose last
-- deploy failed is usually still serving its previous release (AC-5).
-- name: ListAppSummariesByAccount :many
SELECT
    a.id,
    a.name,
    a.slug,
    a.created_at,
    r.release_number AS serving_release_number,
    d.id AS last_deployment_id,
    d.state AS last_deployment_state,
    d.failure_reason AS last_deployment_reason,
    -- The newest finish, not the newest deployment's finish: a deploy running
    -- right now has no finished_at, and that must not blank out when the app
    -- last actually deployed (AC-6, Value sourcing).
    -- Coalesced rather than cast: a NULL here is "nothing has finished", and an
    -- empty string carries that without a nullable column to unwrap.
    CAST(COALESCE(f.last_finished, '') AS TEXT) AS last_deployed_at
FROM apps a
LEFT JOIN releases r ON r.id = a.current_release_id
LEFT JOIN deployments d ON d.id = (
    SELECT n.id FROM deployments n
    WHERE n.app_id = a.id
    ORDER BY n.created_at DESC, n.id DESC
    LIMIT 1
)
LEFT JOIN (
    SELECT app_id, MAX(finished_at) AS last_finished FROM deployments GROUP BY app_id
) f ON f.app_id = a.id
WHERE a.account_id = @account_id AND a.deleted_at IS NULL
ORDER BY a.id DESC
LIMIT @page_limit;

-- The same projection as ListAppSummariesByAccount, over the keyset cursor
-- ListAppsByAccount already carries. It exists because the web app list needs
-- the serving release and the last deploy state on a page that also pages, and
-- neither existing statement has both: one has the state without a cursor and
-- the other has the cursor without the state (spec 0013, AC-14).
-- name: ListAppSummaryPage :many
SELECT
    a.id,
    a.name,
    a.slug,
    a.created_at,
    r.release_number AS serving_release_number,
    d.id AS last_deployment_id,
    d.state AS last_deployment_state,
    d.failure_reason AS last_deployment_reason,
    CAST(COALESCE(f.last_finished, '') AS TEXT) AS last_deployed_at
FROM apps a
LEFT JOIN releases r ON r.id = a.current_release_id
LEFT JOIN deployments d ON d.id = (
    SELECT n.id FROM deployments n
    WHERE n.app_id = a.id
    ORDER BY n.created_at DESC, n.id DESC
    LIMIT 1
)
LEFT JOIN (
    SELECT app_id, MAX(finished_at) AS last_finished FROM deployments GROUP BY app_id
) f ON f.app_id = a.id
WHERE a.account_id = @account_id
  AND a.deleted_at IS NULL
  AND (@cursor = '' OR a.id < @cursor)
ORDER BY a.id DESC
LIMIT @page_limit;

-- Every live app's slug, which is the one read the orphan reaper trusts to
-- decide that a namespace owns nothing (spec 0012, AC-24).
-- name: LiveAppSlugs :many
SELECT slug FROM apps WHERE deleted_at IS NULL;

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

-- A rollback replaces the whole configuration set from the release snapshot, so
-- it clears first and then writes the snapshot's keys, both inside MarkHealthy's
-- transaction (spec 0011, AC-13).
-- name: ClearConfig :exec
DELETE FROM app_config WHERE app_id = @app_id;
