-- Spec 0023. Joining: whether this person has been handed their agent
-- configuration yet.
--
-- Purely additive, one nullable column on one existing table with no default, so
-- a previous binary reads the schema it leaves behind unharmed and no existing
-- row needs a value (AC-24). Every account that already exists is null here,
-- which is what sends each of them to /connect once on their next plain sign in.
--
-- No index. The column is read off the account row a sign in already resolved
-- and is never a search key.

-- +goose Up

-- Null means this person has not yet been handed their agent configuration. Set
-- once, by the first GET /connect they are served, and never cleared: anything
-- clearing it would resurrect a one time redirect somebody already dismissed.
ALTER TABLE accounts ADD COLUMN connected_at TEXT;

-- +goose Down
ALTER TABLE accounts DROP COLUMN connected_at;
