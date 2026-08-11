-- +goose Up
-- The whole platform data model lands in one migration (spec 0002): later slices
-- add queries, not schema, because this database has no backup story yet.

CREATE TABLE accounts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    disabled_at TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL,
    last_used_at TEXT,
    expires_at   TEXT,
    revoked_at   TEXT,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- A name is only reserved while the token is live, so revoking frees it for reuse.
CREATE UNIQUE INDEX api_tokens_live_name ON api_tokens(account_id, name) WHERE revoked_at IS NULL;

CREATE TABLE apps (
    id                 TEXT PRIMARY KEY,
    account_id         TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    name               TEXT NOT NULL,
    -- Unique across every row that ever existed, soft deleted included, so a
    -- retired hostname is never handed to somebody else's app.
    slug               TEXT NOT NULL UNIQUE,
    current_release_id TEXT REFERENCES releases(id) ON DELETE RESTRICT,
    deleted_at         TEXT,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX apps_live_name ON apps(account_id, name) WHERE deleted_at IS NULL;
CREATE INDEX apps_by_account ON apps(account_id, created_at DESC);

CREATE TABLE app_config (
    app_id     TEXT NOT NULL REFERENCES apps(id) ON DELETE RESTRICT,
    -- SQLite has no regex without an extension, so the shape of a key is a GLOB
    -- pattern, deliberately: an upper case letter or underscore, then more of
    -- the same plus digits.
    key        TEXT NOT NULL CHECK (key GLOB '[A-Z_][A-Z0-9_]*'),
    value      TEXT NOT NULL,
    is_secret  INTEGER NOT NULL DEFAULT 0 CHECK (is_secret IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (app_id, key)
) STRICT;

CREATE TABLE uploads (
    id               TEXT PRIMARY KEY,
    account_id       TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    path             TEXT NOT NULL,
    size_bytes       INTEGER NOT NULL,
    sha256           TEXT NOT NULL,
    fetch_token_hash TEXT NOT NULL UNIQUE,
    redeemed_at      TEXT,
    expires_at       TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
) STRICT;

CREATE TABLE deployments (
    id                TEXT PRIMARY KEY,
    app_id            TEXT NOT NULL REFERENCES apps(id) ON DELETE RESTRICT,
    account_id        TEXT NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    upload_id         TEXT REFERENCES uploads(id) ON DELETE RESTRICT,
    source_release_id TEXT REFERENCES releases(id) ON DELETE RESTRICT,
    state             TEXT NOT NULL CHECK (state IN ('queued', 'building', 'pushing', 'deploying', 'healthy', 'failed', 'cancelled')),
    build_path        TEXT CHECK (build_path IS NULL OR build_path IN ('buildpacks', 'dockerfile')),
    build_job_name    TEXT,
    image_repo        TEXT,
    image_digest      TEXT,
    failure_reason    TEXT,
    claimed_at        TEXT,
    claimed_by        TEXT,
    started_at        TEXT,
    finished_at       TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    -- A deployment is unambiguously a build deploy or a rollback, never both and
    -- never neither.
    CHECK ((upload_id IS NULL) <> (source_release_id IS NULL))
) STRICT;

-- At most one deployment in flight per app, enforced here rather than in code.
CREATE UNIQUE INDEX deployments_one_in_flight
    ON deployments(app_id)
    WHERE state NOT IN ('healthy', 'failed', 'cancelled');
CREATE INDEX deployments_claimable ON deployments(state, claimed_at);
CREATE INDEX deployments_by_app ON deployments(app_id, created_at DESC);
CREATE INDEX deployments_finished ON deployments(finished_at);

CREATE TABLE deployment_events (
    id            TEXT PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE RESTRICT,
    from_state    TEXT CHECK (from_state IS NULL OR from_state IN ('queued', 'building', 'pushing', 'deploying', 'healthy', 'failed', 'cancelled')),
    to_state      TEXT NOT NULL CHECK (to_state IN ('queued', 'building', 'pushing', 'deploying', 'healthy', 'failed', 'cancelled')),
    reason        TEXT,
    detail        TEXT,
    occurred_at   TEXT NOT NULL
) STRICT;

CREATE INDEX deployment_events_by_deployment ON deployment_events(deployment_id, occurred_at);
CREATE INDEX deployment_events_occurred ON deployment_events(occurred_at);

CREATE TABLE releases (
    id              TEXT PRIMARY KEY,
    app_id          TEXT NOT NULL REFERENCES apps(id) ON DELETE RESTRICT,
    deployment_id   TEXT NOT NULL UNIQUE REFERENCES deployments(id) ON DELETE RESTRICT,
    release_number  INTEGER NOT NULL,
    image_digest    TEXT NOT NULL,
    config_snapshot TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    UNIQUE (app_id, release_number)
) STRICT;

CREATE TABLE audit_log (
    id          TEXT PRIMARY KEY,
    -- Null when the presented token resolved to nothing, which is exactly the
    -- denial most worth recording.
    account_id  TEXT REFERENCES accounts(id) ON DELETE RESTRICT,
    action      TEXT NOT NULL,
    target_type TEXT,
    target_id   TEXT,
    outcome     TEXT NOT NULL CHECK (outcome IN ('allowed', 'denied')),
    reason      TEXT,
    occurred_at TEXT NOT NULL
) STRICT;

CREATE INDEX audit_log_occurred ON audit_log(occurred_at);

-- +goose Down
DROP TABLE audit_log;
DROP TABLE releases;
DROP TABLE deployment_events;
DROP TABLE deployments;
DROP TABLE uploads;
DROP TABLE app_config;
DROP TABLE apps;
DROP TABLE api_tokens;
DROP TABLE accounts;
