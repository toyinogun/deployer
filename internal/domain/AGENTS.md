# internal/domain

The platform's pure rules. Slug derivation, the deployment state machine, the
closed failure code sets, the configuration bounds and key rule, and the reserved
hostname labels. It imports nothing from the store, `net/http`, or `client-go`,
which is what lets every rule here be read and tested on its own, and it is the
inner layer the root [AGENTS.md](../../AGENTS.md) layering rule points at.

Nothing here does I/O, takes a context, or returns a wrapped error.

## Files

- `state.go`: `State`, the seven deployment states, and `allowed`, the whole
  transition table. Also carries the package doc comment, which still says "the
  deployment state machine and slug derivation" and now understates the package
  by some way.
- `slug.go`: `DeriveSlug`, `SlugBaseMaxLen`, `SlugSuffixLen`.
- `reserved.go`: `ReservedLabel` and the labels an app may never occupy.
- `reason.go`: `Reason`, the closed set a caller is refused with, and the one
  sanitized line each code carries.
- `backupreason.go`: `BackupReason`, a separate closed set for backup runs.
- `config.go`, `configkey.go`: the per app configuration bounds, the keys the
  platform composes itself, `ConfigValue`, and `ValidConfigKey`.
- `image.go`: `RunsAsRoot`.

## Conventions

- **A slug is derived once and stored, never derived again.** The suffix is
  always applied, so two apps with the same name never collide on readability
  alone, and re deriving would produce a different answer every time. Treat
  `DeriveSlug` as a create time function only.
- **`ReservedLabel` asks about the readable base, not the whole slug.** Every
  slug carries a random suffix, so no derived slug is ever literally `console`
  and a check against the whole thing would refuse nothing. The base is what a
  person reads off the hostname, so the base is what is judged.
- **The closed sets are separate on purpose and must not be merged.** `Reason` is
  what a machine surface caller is refused with, `BackupReason` is why a backup
  run failed, and `identity.Code` over in [internal/identity](../identity) is the
  third, for the person facing surface. They answer different questions to
  different readers; merging any two would let a deploy failure code reach a
  backup row, or an identity refusal reach an agent, with nothing noticing. The
  one value two sets share is `internal`, recorded deliberately in
  `closedsets_test.go` rather than left to be rediscovered.
- **A code added to `reason.go` must be added to `theSet` in `reason_test.go` and
  to the pinned wire map beside it.** Four property tests iterate that hand
  written list, so a code missing from it is a code with no test that its message
  is short, leaks nothing, and says something no other code says. This is not
  theoretical: the list said twenty one while the package held twenty seven, and
  six codes went untested from spec 0016 to spec 0022, including both codes spec
  0022 added. The count constant is a reminder, not a guard, because nothing in
  the package reports how many codes it really holds.
- **The bounds in `config.go` are constants rather than `DEPLOYER_*` variables**,
  the same reasoning as the log bounds in [internal/logs](../logs): they are
  product decisions about what an app should carry, not knobs for whoever runs
  the platform.
- **`ValidConfigKey` enforces what the database CHECK only appears to.** Spec
  0002 fixed the constraint as `GLOB '[A-Z_][A-Z0-9_]*'`, and GLOB is shell style
  globbing, so that trailing `*` matches anything at all and the constraint really
  only polices the first character. The CHECK stays as the spec fixed it and is
  the backstop; this function is the promise.
- **An image naming no user counts as root**, because that is what a container
  runtime does with an empty `USER`.

## Tests

Pure logic, so written test first per the root rules, and table driven. The
suite is unusually load bearing here: this package is where a rule is stated
once for the whole platform, and nothing outside it re states the rule to
disagree with.

_Drafted by /sync at the engineer's request, worth a quick human pass._
