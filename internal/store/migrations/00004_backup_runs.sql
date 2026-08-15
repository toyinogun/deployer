-- Spec 0020. Platform backup and restore: the record of every snapshot the
-- control plane takes of its own database.
--
-- Purely additive. One new table and its indexes, nothing existing altered, so
-- a previous binary reads the schema it leaves behind unharmed (AC-11 note in
-- Consequences).

-- +goose Up

-- One row per backup run, inserted running before the snapshot is taken and
-- updated exactly once when the run ends (AC-7). Rows are never pruned, the
-- same deliberate choice releases already make: one row a day is a cost the
-- database carries indefinitely, and a prune is code that can delete the wrong
-- thing (AC-11).
CREATE TABLE backup_runs (
    id             TEXT PRIMARY KEY,
    -- RFC3339 UTC, and also the timestamp half of the object key. Both are
    -- rendered from one held value so the two can never disagree.
    started_at     TEXT NOT NULL,
    -- Null while the run is still going.
    finished_at    TEXT,
    outcome        TEXT NOT NULL CHECK (outcome IN ('running', 'succeeded', 'failed')),
    -- Set once the object lands in the bucket.
    object_key     TEXT,
    -- Of the encrypted object, known before the upload begins because
    -- encryption streams to a file while hashing (AC-4b).
    size_bytes     INTEGER,
    -- SHA-256 of that same ciphertext, what the read back compares (AC-6).
    checksum       TEXT,
    -- One of the closed codes in internal/domain, never a wrapped error string
    -- (AC-10).
    failure_reason TEXT,
    trigger        TEXT NOT NULL CHECK (trigger IN ('schedule', 'manual')),
    -- The admin who pressed the button. Null on a scheduled run (AC-21).
    triggered_by   TEXT REFERENCES accounts(id) ON DELETE RESTRICT
) STRICT;

-- At most one run in flight, enforced here rather than in code, exactly the way
-- deployments_one_in_flight already works. A second run is refused by the
-- database, so two callers cannot both pass a read (AC-8).
CREATE UNIQUE INDEX backup_runs_one_in_flight
    ON backup_runs(outcome)
    WHERE outcome = 'running';

CREATE INDEX backup_runs_started ON backup_runs(started_at DESC);

-- +goose Down
DROP TABLE backup_runs;
