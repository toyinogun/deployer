-- Spec 0020. The record of every backup run.
--
-- The insert carries no in flight check of its own. The partial unique index
-- backup_runs_one_in_flight refuses a second running row, so concurrency is
-- decided by the database rather than by a read the caller ran first (AC-8).
--
-- Every ending update carries `AND outcome = 'running'` so a terminal row can
-- never be rewritten, which is the same shape the deployment transitions use.

-- name: InsertBackupRun :one
INSERT INTO backup_runs (id, started_at, outcome, trigger, triggered_by)
VALUES (@id, @started_at, 'running', @trigger, @triggered_by)
RETURNING *;

-- name: FinishBackupRunSucceeded :execrows
UPDATE backup_runs
SET outcome     = 'succeeded',
    finished_at = @finished_at,
    object_key  = @object_key,
    size_bytes  = @size_bytes,
    checksum    = @checksum
WHERE id = @id AND outcome = 'running';

-- name: FinishBackupRunFailed :execrows
UPDATE backup_runs
SET outcome        = 'failed',
    finished_at    = @finished_at,
    failure_reason = @failure_reason
WHERE id = @id AND outcome = 'running';

-- The read the ticker makes before it attempts anything, so a scheduled tick
-- skips rather than vanishing into a swallowed constraint error (AC-12a).
-- name: GetRunningBackupRun :one
SELECT * FROM backup_runs WHERE outcome = 'running';

-- What the startup catch up compares against the interval (AC-12).
-- name: LatestSucceededBackupRun :one
SELECT * FROM backup_runs
WHERE outcome = 'succeeded'
ORDER BY started_at DESC
LIMIT 1;

-- The previous terminal run, read before the current row is written, which is
-- what decides whether a success is a recovery (AC-13).
-- name: LatestTerminalBackupRun :one
SELECT * FROM backup_runs
WHERE outcome IN ('succeeded', 'failed')
ORDER BY started_at DESC
LIMIT 1;

-- name: ListBackupRuns :many
SELECT * FROM backup_runs
ORDER BY started_at DESC
LIMIT @lim;

-- The startup sweep. One replica and a Recreate strategy make a running row
-- definitionally dead, so this ends every one of them without a grace period
-- (AC-9).
-- name: StrandRunningBackupRuns :execrows
UPDATE backup_runs
SET outcome        = 'failed',
    finished_at    = @finished_at,
    failure_reason = @failure_reason
WHERE outcome = 'running';
