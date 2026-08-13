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

-- A field the caller left empty keeps whatever the row already holds, which is
-- what the Go wrapper always meant by only setting the fields it was given. It
-- matters because this statement runs twice for one deployment: once when the
-- build starts, carrying the path and the Job name, and once when the image is
-- resolved, carrying the digest. Without the coalesce the second call erases what
-- the first wrote, and the build path a failed deployment reports is exactly the
-- field that would go missing (spec 0009, AC-4).
-- name: RecordBuildResult :execrows
UPDATE deployments
SET build_path = COALESCE(@build_path, build_path),
    build_job_name = COALESCE(@build_job_name, build_job_name),
    image_repo = COALESCE(@image_repo, image_repo),
    image_digest = COALESCE(@image_digest, image_digest),
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

-- The release a rollback names, addressed the way a caller addresses it: by the
-- per app number, never by an id the caller has no way to know.
-- name: GetReleaseByNumber :one
SELECT * FROM releases WHERE app_id = @app_id AND release_number = @release_number;

-- name: ListReleasesByApp :many
SELECT * FROM releases
WHERE app_id = @app_id AND (@cursor = '' OR id < @cursor)
ORDER BY id DESC
LIMIT @page_limit;

-- The listing's own read, projecting five named columns so config_snapshot never
-- enters the process at all. Not reusing ListReleasesByApp is the point: a query
-- that cannot load the snapshot is a stronger guarantee than a handler that
-- remembers not to serialize it (spec 0011, AC-4).
-- name: ListReleaseSummariesByApp :many
SELECT id, release_number, image_digest, deployment_id, created_at
FROM releases
WHERE app_id = @app_id
ORDER BY id DESC
LIMIT @page_limit;

-- The app's most recent deployment, which is what a status read by name reports.
-- name: GetLatestDeploymentForApp :one
SELECT * FROM deployments
WHERE app_id = @app_id
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- The deployment that came after this one for the same app, which is what
-- superseded_by is derived from. Ordered by id, a monotonic ULID, never by
-- created_at, because two rows can share one timestamp (spec 0005, AC-13).
-- name: GetNextDeploymentForApp :one
SELECT * FROM deployments
WHERE app_id = @app_id AND id > @after
ORDER BY id
LIMIT 1;
