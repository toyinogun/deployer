# internal/store

The platform's only writer of the SQLite database. It owns the connection, the migrations, and every transaction that has to hold more than one write together. Governing spec: [docs/specs/0002-platform-data-model/index.md](../../docs/specs/0002-platform-data-model/index.md).

## Layout

- [store.go](store.go): `Open`, the embedded goose migrations (`//go:embed migrations/*.sql`), the clock, and the shared transaction helper.
- [accounts.go](accounts.go), [apps.go](apps.go), [backups.go](backups.go), [config.go](config.go), [deployments.go](deployments.go), [invites.go](invites.go), [uploads.go](uploads.go): the hand written methods, one file per entity group.
- [backupadapter.go](backupadapter.go): `ForBackup`, the narrow view `internal/backup` takes, in the same shape as `ForAuth` and `ForSuspend`.
- [errors.go](errors.go): every sentinel error callers branch on.
- [page.go](page.go): the shared pagination shape for list reads.
- [migrations/](migrations/): goose migrations, embedded into the binary and run at startup.
- [queries/](queries/): the `.sql` files you edit.
- [sqlcgen/](sqlcgen/): sqlc output. Generated, never hand edited.

## Conventions

- Edit `queries/*.sql` or a migration, then run `sqlc generate` from the repo root. Never edit anything under `sqlcgen/` by hand; the next generate run overwrites it.
- Migrations are forward only and additive, and there are deliberately few of them. Each one is designed up front because a migration that rewrites live data is the thing to avoid. There is a nightly backup now (spec 0020), but it is a restore of the whole file to a point in time, not an undo for one bad migration: `00003_invites` adds a table and touches nothing existing, which is why the previous binary still reads the schema it leaves behind.
- An audit row keeps its row and loses its identifier (spec 0021). `00005` adds a nullable `client_address` to `audit_log`, and it is the one migration here carrying a real down step, since dropping a nullable column nothing else reads is safe. A daily sweep sets that column back to null on every row past `DEPLOYER_RETENTION_DAYS` and deletes nothing, in one `UPDATE` that scans the whole table once a day with no supporting index: that is the right cost for a table this size, and it is stated here rather than discovered. A platform written row (a suspension sweep, a reconcile drive, a scheduled backup) leaves the field unset, so once the window passes a nulled row and a platform written one read the same, which is accepted.
- A foreign key naming a row the same transaction has not inserted yet is `DEFERRABLE INITIALLY DEFERRED`, checked at `COMMIT` rather than at the statement. `invites.consumed_by` is the case: the guarded update that spends an invite runs before the account insert, so a dead invite costs no account row, and an immediate check would fail on the update itself.
- Callers branch on the sentinels in `errors.go` (`ErrNotFound`, `ErrTokenInvalid`, `ErrIllegalTransition`, and friends). Anything else is wrapped with `fmt.Errorf("...: %w", err)` and treated as a fault, not a decision.
- A sentinel may wrap another sentinel when one case is a kind of the other, so a caller branching on the general case still matches: `ErrTerminal` (the row was already ended, which is what a supersession does mid drive) wraps `ErrIllegalTransition`.
- Failures that must be indistinguishable stay indistinguishable: `ErrTokenInvalid` covers unknown, revoked, and expired tokens alike. A suspended account is the one deliberate exception and it is not decided here: `ResolveToken` stopped filtering `accounts.disabled_at` and returns the stamp instead, so `auth.Authenticate` can answer `ErrAccountSuspended` for a good token on a suspended account (spec 0018, AC-12). That moves a gate off the query and into Go, which is why the credential shapes carry their own test.
- An app's configuration has two read methods and the split is the invariant, not a convenience: `ListConfigForResponse` withholds every secret value and is the only one a tool response may use, `ListConfigForDeploy` returns them in full and belongs to the deploy path and the release snapshot alone. No caller decides which values it may show; the method it picked already did.
- A release's `config_snapshot` is the configuration the caller composed the workload from, handed to `MarkHealthy`, never a fresh read inside the transaction. The deploy reads configuration once and then waits for readiness, so re reading here would snapshot a change that landed during that wait onto a release that never ran it.
- A snapshot records `{"KEY":{"value":...,"secret":...}}` per key. Decoding is per key rather than per document: a bare string is a snapshot written before that format and every key restored from one is marked secret, because a key wrongly marked secret hides a value `get_config` used to show while the reverse leaks one. No migration, no version field, and both shapes stay readable.
- `app_config` is rewritten from the snapshot inside the same transaction that marks a rollback healthy, replacing the whole set, so stored configuration and the running app cannot disagree because a rollback failed halfway. A `set_config` that commits during that rollout is reverted by it, which is specified behaviour rather than a race to close.
- The release listing has its own query projecting five named columns and never `config_snapshot`, so no configuration value enters the process at all. The `SELECT *` read stays for the callers that need the whole row: a handler remembering not to serialize a field it holds is a much weaker guarantee than never holding it.
- A claim is unclaimed when `claimed_at` is null, because that is the only column `ClaimNextDeployment` tests. Anything handing a row back clears both claim columns, so clearing `claimed_by` alone leaves a row nothing will ever adopt while looking released.
- A guard that decides whether a write is allowed runs inside the transaction that performs it, not before it. `CreateApp` counts the account's live apps at the top of the same `BEGIN IMMEDIATE` transaction that inserts the row, so the write lock is already held when the count runs and two racing creates cannot both pass it. A helper such a guard uses takes the `*sqlcgen.Queries` handle rather than the pool, so it runs inside whatever transaction its caller opened; `nameTaken` is the case.
- Domain and use case packages never import this one. They declare the narrow interfaces they need and take one of these types.
- Every state transition is a database write before it is an action, and a multi write move (a transition plus its event, a deployment plus its release) runs inside one transaction.
- Timestamps come from the injected `ids.Clock`, never from `time.Now()` directly, so tests can control them.

## Tests

Tests run against a real SQLite file in a temp directory. The store is never mocked. That bans inventing store behaviour rather than banning a passthrough: a test type embedding one of these types, delegating every call, and returning a real error on one named call invents nothing, and it is the only way a caller can reach a fault internal to the platform, such as a write that does not land. See `internal/reconcile/stranded_test.go`. Database level invariants (the state `CHECK`, the partial unique indexes, the source XOR constraint, foreign key restrictions) are exercised with raw SQL that bypasses the Go layer, so the schema is proven, not just the code above it.

_Drafted by /sync from the introducing change, worth a quick human pass._
