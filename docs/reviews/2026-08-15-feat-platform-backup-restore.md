# Review, feat/platform-backup-restore, 2026-08-15

**Reviewed by**: claude-opus-5[1m] (author model not recorded)
**Scope**: 52 files (46 committed since `ca7b8a6`, 6 untracked test files), branch vs `main`
**Verdict**: Changes requested

## Summary

Spec 0020 lands the platform's self-backup: `VACUUM INTO` snapshot, integrity and
account checks on the plaintext, streaming age encryption to a public recipient,
minio-go upload, read-back verification, a `backup_runs` record with a partial
unique index, a ticker with a startup catch-up and stranded sweep, failure and
recovery mail, an admin page, and a `deployer restore` subcommand. The quality is
high: the layering is clean (`internal/backup` declares its own `Runs` and
`ObjectStore` interfaces and imports no store), the closed reason set is real and
enforced at the store boundary, error messages are deliberately stripped of
bucket/key/credential detail and there are tests that assert that, and the
snapshot's 0600 mode is pinned by an in-package test after `/check verify` caught
it at 0644. `gofmt`, `go vet` and `go test -race` are all clean.

Two things should be fixed before merge. The manual "Back up now" path runs the
whole backup on the HTTP request's context, so an admin who disconnects can leave
a `running` row that no live sweep will ever clear, silently disabling every
subsequent backup with no alert. And `confirmRecipient`, the single check that
catches encrypting to the wrong key, has no test at all despite the spec naming
it as a critical scenario and `verify.md` claiming it is covered by one.

## Major

### 🟠 A manual run bound to the request context can permanently stall all backups, `internal/web/backups.go:85`

**Problem**: `adminBackupRun` calls `s.backups.Run(r.Context(), admin.ID)` and
blocks for the entire backup (snapshot, encrypt, upload, read-back). Go cancels
the request context the moment the client's connection closes. When that happens
mid-run, every subsequent step fails, and the recovery steps use the *same*
cancelled context: `FinishBackupRunFailed` (`internal/backup/run.go:154`) fails
its SQLite write and is only logged, and `s.alert` (`run.go:157`) hands the
cancelled context to the mailer, which also fails and is only logged.

**Why it matters**: The row stays `outcome = 'running'` forever inside a live
process. `Service.tick` (`internal/backup/schedule.go:99-103`) sees a run in
flight and skips every tick from then on; the partial unique index refuses every
manual run; and no mail is sent about any of it. Backups stop silently until the
pod is restarted, which is precisely the failure AC-13 exists to prevent. The
scheduled path is protected from this by the startup sweep, and the manual path
is not. The trigger is an admin closing a tab, an ingress or proxy timeout on a
run that outlives it, or simply a slower run as the database grows, so the
likelihood rises over time rather than falling.

**Suggested fix**: Do not let a caller's connection own the lifetime of a state
machine the caller does not own. Either run the manual backup on a context
derived from the process (`context.WithoutCancel` of the request context, or the
service's own context) and have the handler wait on a result channel with its own
bound; or, at minimum, close the row and send the alert on a fresh short-lived
context so a cancelled run always reaches a terminal row. A cheap belt-and-braces
addition is to make `tick` age out a `running` row older than some multiple of
the interval rather than skipping forever.

### 🟠 `confirmRecipient` (AC-4c) has no test, and `verify.md` claims it has one, `internal/backup/snapshot.go:198`

**Problem**: Nothing in the suite exercises `confirmRecipient`. `grep` over
`internal/backup/*_test.go` finds no reference to it, and no test encrypts to one
recipient and confirms against another. The spec's critical test scenarios name
this case explicitly ("encrypting to a recipient other than the configured one is
caught by the header check and uploads nothing, verifies AC-4c"), and
`docs/specs/0020-platform-backup-restore/verify.md` asserts "AC-4c covered by
unit test only" — which is not true today.

**Why it matters**: This is the only check in the entire design that catches an
object encrypted to a key nobody holds. Nothing else can: the read-back compares
bytes, the header check is the sole recipient assurance, and no code in the
cluster can decrypt. A regression here produces a bucket full of well-formed,
verified, permanently unreadable backups that every other check passes and that
nobody discovers until restore day. The function also has real branching worth
covering: the empty-`want` guard, the stanza-line mismatch, and the body
mismatch. Per the review guide, untested new security-relevant branching logic is
a Major.

**Suggested fix**: Add a package-internal test (it is unexported, so it belongs
beside `snapshot_internal_test.go`) that encrypts a file to recipient A, calls
`confirmRecipient` with the stanzas from recipient B, and asserts the refusal;
plus the happy path and the empty-stanza guard. Correct the coverage line in
`verify.md` in the same change so the document stops asserting coverage that does
not exist.

## Minor

### 🟡 Restore misreports a missing or forbidden object as a decryption failure, `internal/backup/restore.go:52`

**Problem**: `S3Store.Get` wraps `minio.Client.GetObject`, which is lazy: it
returns a `*minio.Object` with a nil error and does not issue the request until
the first `Read`. So on a real bucket, a mistyped key, an expired credential, or
a 403 does not surface at `store.Get`; it surfaces inside `age.Decrypt`, which
reports "backup: decrypting the object". The in-memory `fakeBucket` errors
eagerly, which is why `TestRestore_failsWhenTheObjectIsNotThere` passes while the
real client behaves differently.

**Why it matters**: This is the one command run on the worst day, by someone
under pressure, typing a key by hand. Being told the object will not decrypt when
the real problem is a typo or a stale credential is exactly the wrong signal — it
points the operator at the age identity, the one thing they cannot re-derive.

**Suggested fix**: Have `S3Store.Get` call `Stat()` on the object (or issue
`StatObject`) before returning it, and wrap the failure so a missing object and a
refused credential read as themselves. `verifyObject` is unaffected either way,
since it fails on the copy.

### 🟡 The stranded-run sweep is skipped whenever backups are off, `cmd/deployer/main.go:271`

**Problem**: `startBackups` returns nil before constructing the service when
`cfg.BackupsConfigured()` is false or `NewS3Store` fails, so `svc.Sweep` — and
with it `StrandBackupRuns` — never runs. AC-9 states the sweep happens at startup
unconditionally.

**Why it matters**: A platform that had a run in flight when backups were turned
off (or when the bucket client stopped building) keeps a permanently `running`
row. The admin page shows it as still going forever, and the row occupies the
partial unique index, so re-enabling backups depends on the sweep that only runs
once the service builds. It self-heals on re-enable, which is why this is Minor
rather than Major, but the record lies in the meantime.

**Suggested fix**: Run the strand sweep off `store.ForBackup(st)` before the
configured check, next to the `EmptyTempDir` call that is already unconditional
there for the same reason.

### 🟡 A failed list read overwrites the message the action just produced, `internal/web/backups.go:127`

**Problem**: `renderBackups` unconditionally replaces `data.Message` with "The
backup record could not be read just now." when `ListBackupRuns` fails, discarding
the caller's message and downgrading a 409 refusal or a `verify_failed` result to
a bare 500.

**Why it matters**: The admin who just pressed the button loses the answer to
what happened to their run, and is told only that a list read failed.

**Suggested fix**: Append the read failure to the existing message rather than
replacing it, and only raise the status when there was no more specific one.

### 🟡 A failed restore leaves an unusable plaintext file that blocks the retry, `internal/backup/restore.go:63-80`

**Problem**: The destination is created before the copy and the integrity check.
If `io.Copy` fails partway, or `checkSnapshot` rejects the result, the partial or
unsound plaintext database is left at `-out`. The next attempt then hits the
"already exists; restore never overwrites" guard at line 41 and refuses.

**Why it matters**: Two coupled costs on restore day: a plaintext database file
left on the operator's disk after a failure, and a retry that is refused with a
message pointing at the wrong problem. The operator has to work out that they
must delete the failed artefact first.

**Suggested fix**: Write to a temporary file beside the destination and rename it
into place only after `checkSnapshot` passes; remove the temporary on every
failure path. That also makes the "never overwrites" guarantee atomic rather than
a check-then-write.

### 🟡 `go.mod` is not tidy: both new direct dependencies are marked indirect, `go.mod:18`

**Problem**: `filippo.io/age` and `github.com/minio/minio-go/v7` are in the
`// indirect` require block despite being imported directly by
`internal/backup`, `internal/config` and `cmd/deployer`. Running `go mod tidy`
moves them (verified: 2 insertions, 2 deletions).

**Why it matters**: `go.mod` no longer records what this module actually depends
on directly, which is the file a reader checks to answer "what did this feature
add". CI does not appear to gate on tidiness, so it will stay wrong.

**Suggested fix**: Run `go mod tidy` and commit the result.

## Nits

- ⚪ `internal/backup/snapshot.go:222`, `confirmRecipient` substring-matches the
  raw base64 stanza body. age wraps stanza bodies at 64 columns, so this holds
  only because an X25519 body is 43 characters; a longer stanza type would fail
  closed for the wrong reason. Worth a comment naming the assumption.
- ⚪ `internal/backup/objectstore.go:99`, `splitEndpoint` hand-rolls prefix
  matching and length arithmetic where `url.Parse` would do. It also passes a
  path through as part of the host — the test at
  `objectstore_internal_test.go:32` pins that behaviour rather than questioning
  it, and minio-go will not do anything sensible with it.
- ⚪ `internal/web/web.go:106`, `New` is now eight positional parameters plus an
  options struct, with `st` passed twice (as `Data` and as `BackupRuns`). The
  next addition should probably fold these into `Options` or a deps struct.
- ⚪ `internal/backup/schedule_test.go:25` and `run_test.go:466`, the schedule
  tests rely on a fixed `time.Sleep` to let the catch-up decide. It works, but it
  is the kind of thing that gets flaky on a loaded CI runner.
- ⚪ `internal/backup/snapshot.go:43`, the `VACUUM INTO` destination is string
  interpolated. It is safe today and the comment explains why, but a single quote
  anywhere in `TempDir` would break it silently; a guard on the constant would
  cost nothing.
- ⚪ `cmd/deployer/main.go:262` and `internal/backup/schedule.go:25`,
  `EmptyTempDir` runs twice at startup on the configured path. Harmless, but one
  of the two is redundant.

## Strengths

- The layering is genuinely clean. `internal/backup` declares `Runs`,
  `ObjectStore`, `Alerter` and `Mailer` itself and imports neither the store nor
  `net/http`; `backupadapter.go` translates `ErrBackupInFlight` into
  `backup.ErrInFlight` and nothing else, so a real write fault cannot be
  laundered into a benign refusal. AC-8a is upheld in code, not just in prose.
- The tests do the hard version rather than the easy one: `run_test.go` generates
  a real age key pair, takes a real `VACUUM INTO` snapshot of a real SQLite file,
  and decrypts the resulting object back into a database it then queries.
  `failingRuns` in `schedule_test.go` is exactly the passthrough the root
  AGENTS.md carves out — it embeds the real in-memory record and fails one named
  call, inventing no behaviour.
- Secret hygiene is tested, not asserted. `TestMailAlerterBackupFailed_carries
  NothingBesidesTheReason` pins the entire mail body per reason code,
  `TestVerifyObject_theFailureDoesNotCarryTheObjectKey` and
  `TestSplitEndpoint_theRefusalDoesNotEchoTheEndpoint` prove errors that reach an
  alert carry no bucket detail, and `TestTheAgeIdentityHasNoConfigurationValue`
  proves configuration never reaches for a variable that could hold a private
  key.
- `snapshot_internal_test.go` is the right response to a `/check verify` finding:
  the 0644 mode the fakes could never have caught is now pinned in-package, with
  a comment explaining why a process umask was rejected.
- The migration is exemplary: `STRICT`, additive only, reversible, with the
  in-flight rule as a partial unique index rather than a Go check, and every
  ending update carrying `AND outcome = 'running'` so a terminal row cannot be
  rewritten. The `sqlc.yaml` override ordering comment is the kind of note that
  saves the next person an hour.
- The all-or-nothing config group with the alert address inside it, tested one
  absent member at a time, correctly makes "backups with nowhere to complain to"
  unrepresentable.

## Test coverage

Strong overall, and unusually so for a feature with this much I/O. Covered: the
end-to-end thread against real SQLite and real age; verify mismatch, empty
database, upload failure, non-unique insert fault, in-flight refusal, and the
recovery-mail sequence; the sweep and both of its failure modes; the startup
catch-up in both directions; `EmptyTempDir` including nested leftovers and an
unmakeable directory; every restore refusal including a wrong identity, a
non-identity file, a missing object, and a decoy that decrypts and is not a
database; the store's in-flight index, terminal-row protection, unknown-reason
rejection, strand sweep, and trigger columns against a real SQLite file; the
config group one absent member at a time; the page's admin gate, CSRF refusal,
unconfigured state, in-flight refusal, audit rows, and reason-code-only rendering.

Gaps, in priority order:

1. `confirmRecipient` — no test at all. Raised as Major above; this is the check
   the whole encryption design leans on.
2. Context cancellation mid-run — nothing covers what the row and the alerter do
   when the run's context dies partway. This is the mechanism behind the Major
   above and a test would have surfaced it.
3. `snapshot` and `encryptTo` failure paths — `BackupSnapshotFailed` and the
   encryption half of `BackupEncryptFailed` are never reached by a test, so those
   two of the seven closed codes are only proven by the `domain` package's string
   assertions.
4. `S3Store.Put`/`Get` are only covered for construction and endpoint parsing, by
   design (`/check verify` owns the real bucket). Reasonable, but it is why the
   lazy-`GetObject` behaviour above slipped through.
