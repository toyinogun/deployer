# internal/store

The platform's only writer of the SQLite database. It owns the connection, the migrations, and every transaction that has to hold more than one write together. Governing spec: [docs/specs/0002-platform-data-model/index.md](../../docs/specs/0002-platform-data-model/index.md).

## Layout

- [store.go](store.go): `Open`, the embedded goose migrations (`//go:embed migrations/*.sql`), the clock, and the shared transaction helper.
- [accounts.go](accounts.go), [apps.go](apps.go), [deployments.go](deployments.go), [uploads.go](uploads.go): the hand written methods, one file per entity group.
- [errors.go](errors.go): every sentinel error callers branch on.
- [page.go](page.go): the shared pagination shape for list reads.
- [migrations/](migrations/): goose migrations, embedded into the binary and run at startup.
- [queries/](queries/): the `.sql` files you edit.
- [sqlcgen/](sqlcgen/): sqlc output. Generated, never hand edited.

## Conventions

- Edit `queries/*.sql` or the migration, then run `sqlc generate` from the repo root. Never edit anything under `sqlcgen/` by hand; the next generate run overwrites it.
- The migration is a single forward migration. It is designed once, up front, because there is no database backup yet, so a migration against live data is the thing to avoid.
- Callers branch on the sentinels in `errors.go` (`ErrNotFound`, `ErrTokenInvalid`, `ErrIllegalTransition`, and friends). Anything else is wrapped with `fmt.Errorf("...: %w", err)` and treated as a fault, not a decision.
- A sentinel may wrap another sentinel when one case is a kind of the other, so a caller branching on the general case still matches: `ErrTerminal` (the row was already ended, which is what a supersession does mid drive) wraps `ErrIllegalTransition`.
- Failures that must be indistinguishable stay indistinguishable: `ErrTokenInvalid` covers unknown, revoked, expired, and disabled account tokens alike.
- Domain and use case packages never import this one. They declare the narrow interfaces they need and take one of these types.
- Every state transition is a database write before it is an action, and a multi write move (a transition plus its event, a deployment plus its release) runs inside one transaction.
- Timestamps come from the injected `ids.Clock`, never from `time.Now()` directly, so tests can control them.

## Tests

Tests run against a real SQLite file in a temp directory. The store is never mocked. Database level invariants (the state `CHECK`, the partial unique indexes, the source XOR constraint, foreign key restrictions) are exercised with raw SQL that bypasses the Go layer, so the schema is proven, not just the code above it.

_Drafted by /sync from the introducing change, worth a quick human pass._
