# Verify: platform data model · spec 0002 · updated 2026-08-11

_Steps derived from spec 0002 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

## Commands

- [x] `go test -race ./internal/...` → every package passes, coverage at or above 80 percent outside `sqlcgen` → AC-18
- [x] `go test -race -run TestMigrationUpThenDownLeavesTheFileEmpty ./internal/store/` → up creates all nine tables, a second up is a no-op, down leaves none of them → AC-1
- [x] `go test -race -run TestPragmasAreSetOnEveryConnection ./internal/store/` → four separate pool connections each report `wal`, `foreign_keys=1`, and the configured busy timeout → AC-2
- [x] `go test -race -run TestIds ./internal/ids/` → a thousand ids stamped with one instant are distinct and sort into creation order → AC-3
- [x] `go test -race -run TestDatabaseRefusesAnUnknownState ./internal/store/` → a raw `UPDATE deployments SET state='pending'` is rejected by the database, not only by Go → AC-4
- [x] `go test -race -run TestHappyPath ./internal/store/` → one deploy leaves exactly five events in order, release number 1, and `apps.current_release_id` pointing at it → AC-4, AC-5, AC-9
- [x] `go test -race -run TestIllegalTransitionWritesNothing ./internal/store/` → a move out of a terminal state returns `ErrIllegalTransition` and adds no event row → AC-6
- [x] `go test -race -run 'TestSupersession|TestIndexRefusesASecondInFlightRow' ./internal/store/` → the in flight deployment is cancelled with reason `superseded` and its own event, and a raw insert of a second in flight row is refused by the partial unique index → AC-5, AC-7
- [x] `go test -race -run TestClaimRaces ./internal/store/` → eight goroutines race one queued row, exactly one wins, seven get `ErrNotFound`, and `claimed_by` records the winner → AC-8
- [x] `go test -race -run 'TestMarkHealthyIsAllOrNothing|TestMarkHealthyNeedsADigest' ./internal/store/` → a forced release failure leaves the deployment not healthy, no event, and no release pointer; no digest returns `ErrNoDigest` → AC-9
- [x] `go test -race -run TestRollbackFidelity ./internal/store/` → the rollback carries release 1's digest from creation, walks queued to deploying to healthy in three events with no building or pushing, and release 1's snapshot is untouched by the later config change → AC-10
- [x] `go test -race -run 'TestSlugsAreRetiredForever|TestSlugCollisionRetries' ./internal/store/` → a soft deleted slug stays taken, the same name gets a new slug, a pinned collision is retried once and succeeds, and a suffix that never frees up fails with `ErrSlugTaken` after five attempts → AC-11, AC-12
- [x] `go test -race -run 'TestResolveTokenOnlyAcceptsALiveToken|TestAuditRecordsBothOutcomes' ./internal/store/` → unknown, revoked, expired, and disabled all return `ErrTokenInvalid`, and a denial with no resolved account still writes an audit row with a null `account_id` → AC-13, AC-14
- [x] `go test -race -run TestConfigReadPathsAreSplit ./internal/store/` → the response path lists a secret key with its flag and no value while the deploy path returns the value → AC-15
- [x] `go test -race -run 'TestDeploymentSourceIsExactlyOne|TestForeignKeysRestrict' ./internal/store/` → both sources and neither source are refused, and deleting an app that has deployments fails on the foreign key → AC-16
- [x] `go test -race -run TestRetentionSweep ./internal/store/` → the aged failure and all its events go, a failure inside the window stays, aged events of a surviving deployment go, the orphaned upload goes, an upload a live deployment still names stays, and every release and audit row survives → AC-17

## Manual

- [x] Boot on an empty volume: `DEPLOYER_DB_PATH=/tmp/fresh/deployer.db DEPLOYER_REGISTRY_HOST=r DEPLOYER_REGISTRY_USER=u DEPLOYER_REGISTRY_PASSWORD=p DEPLOYER_APP_DOMAIN=d.example go run ./cmd/deployer` → logs `database ready`, then `sqlite3 /tmp/fresh/deployer.db ".tables"` shows all nine tables plus `goose_db_version` → AC-1
- [x] Boot twice against the same file → the second boot logs `database ready` again and adds no migration row → AC-1
- [x] `DEPLOYER_RETENTION_DAYS=0 go run ./cmd/deployer` → refuses to start and names the variable → AC-17
- [x] `DEPLOYER_DB_BUSY_TIMEOUT_MS=abc go run ./cmd/deployer` → refuses to start and names the variable → AC-2

## Value sourcing

One step per row of the spec's Value sourcing table, each exercising the edge that breaks if the source is wrong.

- [x] Ids come from `internal/ids` with a fixed prefix per entity → every row's id starts with its own prefix (`acc_`, `app_`, `dep_`, `rel_`, `evt_`, `tok_`, `upl_`, `aud_`), covered by `TestPrefixes`
- [x] Timestamps come from the injected `Clock`, never `CURRENT_TIMESTAMP` → run the suite with the fixed clock and confirm no row's `created_at` moves between runs; grep the migration for `CURRENT_TIMESTAMP` and expect nothing
- [x] A slug is derived once from the name and never re derived → rename intent aside, confirm `apps.slug` is never written by an update: `grep -rn "SET slug" internal/store/` returns nothing
- [x] Namespace and hostname are derived at use time, not stored → `grep -rn "namespace\|hostname" internal/store/migrations/` returns nothing
- [x] A fresh slug after a collision comes from a regenerated suffix → `TestSlugCollisionRetries` pins the suffix source and proves the retry uses the new one
- [x] The superseded deployment id comes from the in flight row inside the same transaction → `TestSupersession` asserts the returned id is the first deployment's
- [x] The first event has `from_state` null → `TestHappyPath` asserts event zero is `"" to queued`
- [x] A rollback's digest comes from the source release at creation → `TestRollbackFidelity` asserts the digest is present before any transition runs
- [x] `from_state` on an event is read inside the transaction, never passed in → the `Transition` signature takes no from state; confirm with `grep -n "func (s \*Store) Transition" internal/store/deployments.go`
- [x] `started_at` is stamped leaving queued and `finished_at` entering a terminal state → `TestHappyPath` asserts a queued row has no `started_at` and a healthy row has `finished_at`
- [x] `build_path` and the image digest come from the build result, not the caller of `MarkHealthy` → `TestMarkHealthyNeedsADigest` proves `MarkHealthy` accepts no digest argument and refuses without one on the row
- [x] `release_number` is `MAX + 1` per app inside the transaction → `TestRollbackFidelity` asserts a rollback is release 3, so numbers are never reused
- [x] `config_snapshot` is the app's full configuration, secrets included, read inside the transaction → `TestSnapshotIncludesSecrets` and `TestRollbackFidelity` assert the value at the moment of the release, not the current one
- [x] `claimed_by` comes from `DEPLOYER_POD_NAME`, falling back to the hostname → `TestDataModelDefaults` asserts the fallback, `TestDataModelOverrides` asserts the injected name
- [x] `token_hash` is the only stored form of a token → `TestResolveTokenOnlyAcceptsALiveToken` asserts the stored hash and prefix; confirm no `token` column holds plaintext
- [x] `redeemed_at` is set by a conditional update, so redeem is single use → `TestRedeemIsSingleUse` asserts the second attempt returns `ErrUploadRedeemed`
- [x] `audit_log.account_id` is null when resolution itself failed → `TestAuditRecordsBothOutcomes` asserts it
- [x] The sweep cutoffs are `now` minus retention, and 24 hours for uploads → `TestRetentionSweep` ages rows past the window and asserts what goes and what stays

## Acceptance-criteria coverage

- AC-1 migration up and down · AC-2 pragmas per connection · AC-3 id generation · AC-4 state CHECK · AC-5 row and event in one transaction · AC-6 illegal transition writes nothing · AC-7 one in flight per app · AC-8 the claim race · AC-9 MarkHealthy is all or nothing · AC-10 rollback fidelity · AC-11 permanent slugs and the retry · AC-12 soft delete · AC-13 token resolution · AC-14 audit on both outcomes · AC-15 split config reads · AC-16 foreign keys and the source CHECK · AC-17 the retention sweep · AC-18 a real SQLite file, nothing mocked

## Known gap

- The spec fixes the `app_config.key` CHECK as `GLOB '[A-Z_][A-Z0-9_]*'`. SQLite `GLOB` is shell style globbing, so its trailing `*` matches any characters and the constraint only polices the first character: `HAS-DASH` passes it. The migration keeps the constraint exactly as the spec fixed it, and `domain.ValidConfigKey` enforces what the pattern plainly means, so `ErrInvalidKey` is real. Feature 11 owns the key naming rules and should decide whether to tighten the CHECK itself.

## Verify run 2026-08-11 · PASS after one fix

Five steps failed the first run on an intermittent id ordering fault, not on their own
logic: `TestHappyPath` read its events rotated and `TestAuditRecordsBothOutcomes` read its
two audit rows swapped, twice in twelve `go test -race ./internal/store/` runs.

Root cause: `ids.New` handed the caller's timestamp straight to one process wide
`ulid.Monotonic` entropy. That entropy only climbs while the timestamp holds steady, and
reseeds at random when it moves backwards. Concurrent callers read their clocks in one
order and reach `New` in another, so ids stopped sorting in creation order, and the three
reads that lean on that (`ORDER BY occurred_at, id`, the lowest queued id in
`ClaimNextDeployment`, and id as a paging cursor) came back wrong. `New` now pins the
timestamp to its high water mark, so an earlier stamp can no longer reseed it.
`TestOrderingSurvivesAnEarlierStamp` locks it in. Fifteen consecutive `-race` runs clean.
