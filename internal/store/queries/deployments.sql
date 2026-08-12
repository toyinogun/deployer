-- name: CreateDeployment :one
INSERT INTO deployments (id, app_id, account_id, upload_id, source_release_id, state, image_digest, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'queued', ?, ?, ?)
RETURNING *;

-- name: GetDeployment :one
SELECT * FROM deployments WHERE id = ?;

-- name: GetInFlightDeploymentForApp :one
SELECT * FROM deployments
WHERE app_id = @app_id AND state NOT IN ('healthy', 'failed', 'cancelled');

-- The transition writes state and its two lifecycle stamps together. started_at
-- and finished_at are only ever set, never cleared, so a null argument leaves
-- whatever is already there.
-- name: UpdateDeploymentState :one
UPDATE deployments
SET state = @state,
    failure_reason = COALESCE(@failure_reason, failure_reason),
    started_at = COALESCE(started_at, @started_at),
    finished_at = COALESCE(finished_at, @finished_at),
    updated_at = @now
WHERE id = @id
RETURNING *;

-- name: InsertDeploymentEvent :exec
INSERT INTO deployment_events (id, deployment_id, from_state, to_state, reason, detail, occurred_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListDeploymentEvents :many
SELECT * FROM deployment_events
WHERE deployment_id = @deployment_id
ORDER BY occurred_at, id;

-- name: RecordBuildResult :execrows
UPDATE deployments
SET build_path = @build_path,
    build_job_name = @build_job_name,
    image_repo = @image_repo,
    image_digest = @image_digest,
    updated_at = @now
WHERE id = @id;

-- One claim wins. The inner select picks the oldest unclaimed queued row and the
-- outer WHERE re-checks the claim, so a racing caller updates zero rows.
-- name: ClaimNextDeployment :one
UPDATE deployments
SET claimed_at = @now, claimed_by = @claimed_by, updated_at = @now
WHERE id = (
    SELECT id FROM deployments
    WHERE state = 'queued' AND claimed_at IS NULL
    ORDER BY id
    LIMIT 1
)
AND claimed_at IS NULL
RETURNING *;

-- name: ListNonTerminalDeployments :many
SELECT * FROM deployments
WHERE state NOT IN ('healthy', 'failed', 'cancelled')
ORDER BY id;

-- name: ListDeploymentsByApp :many
SELECT * FROM deployments
WHERE app_id = @app_id AND (@cursor = '' OR id < @cursor)
ORDER BY id DESC
LIMIT @page_limit;

-- name: InsertRelease :one
INSERT INTO releases (id, app_id, deployment_id, release_number, image_digest, config_snapshot, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: NextReleaseNumber :one
SELECT COALESCE(MAX(release_number), 0) + 1 AS next FROM releases WHERE app_id = @app_id;

-- The release a deployment minted, which is what a successful deploy reports
-- back rather than recomputing the number or the digest.
-- name: GetReleaseByDeployment :one
SELECT * FROM releases WHERE deployment_id = @deployment_id;

-- name: GetRelease :one
SELECT * FROM releases WHERE id = ?;

-- name: ListReleasesByApp :many
SELECT * FROM releases
WHERE app_id = @app_id AND (@cursor = '' OR id < @cursor)
ORDER BY id DESC
LIMIT @page_limit;
