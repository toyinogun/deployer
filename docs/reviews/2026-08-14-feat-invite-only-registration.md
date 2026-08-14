# Review, feat/invite-only-registration, 2026-08-14

**Reviewed by**: Claude Sonnet 5 (author on Claude Opus 5)
**Scope**: 35 files, branch vs main (merge base 5f5a6ab2)
**Verdict**: Approve with nits

## Summary

Spec 0015 adds a single-use, seven-day invite as the new front door to registration: one additive migration, a store method that spends the invite and creates the account in one transaction with a full live-predicate guard, and matching refusals on both the JSON and page surfaces. The transaction design is the strongest part of the change — the guarded `UPDATE` inside the same transaction as the account insert, with `consumed_by` declared `DEFERRABLE INITIALLY DEFERRED`, correctly makes the race and the taken-address cases (AC-4, AC-10) provably true rather than probable, and `TestOneInviteMakesOneAccountUnderRace` exercises it against a real SQLite file under `-race`. Both surfaces route through the one `Service.Register` check, the invite lookup precedes the password hash (AC-11), and the leak crawl and audit-row tests are thorough. `gofmt`, `go vet`, `golangci-lint`, and `go test -race` all pass clean on the affected packages. The only real issue is a wrong reason code on an admin-only validation path; everything else is minor coverage gaps.

## Minor

### 🟡 Over-long note answers with the wrong reason code, `internal/identity/invites.go:100`
**Problem**: `CheckNote` fails with `Fail(CodeEmailInvalid, ...)` when an admin's note exceeds 200 characters. `CodeEmailInvalid` is documented at its declaration (`internal/identity/identity.go:25`) as meaning "the address failed net/mail parsing or was too long" — it has nothing to do with a note field.
**Why it matters**: AGENTS.md is explicit that "a failure a caller sees is one of the closed reason codes... never a wrapped error string." The `code` field is the contract a caller is meant to branch on; the message text happens to say the right thing here, but a client (or a future admin UI) that keys off `code` will be told the *email* was invalid when the actual problem was the note. `TestAnOverLongNoteIsRefused` only asserts the 422 status, so this slid through untested.
**Suggested fix**: Add a dedicated code (e.g. `CodeNoteInvalid`) mapped to 422 alongside the others in `statusFor`, or at minimum reuse a genuinely generic validation code rather than one whose doc comment names a different field.

### 🟡 Derived "expired" and "spent" list states aren't asserted through the service/API layer, `internal/identity/invites.go:83` (`InviteStateOf`)
**Problem**: `InviteStateOf` is the pure function that turns three timestamps into the `live`/`spent`/`revoked`/`expired` label shown on the admin list (AC-8). It has no dedicated test in `internal/identity` (no `invites_test.go` there at all), and none of the store/httpapi/web tests assert that an *expired* invite's `state` field actually renders `"expired"`, or that a *spent* one renders `"spent"` with `spent_by` populated at the list/API layer — `TestSpendingAnInviteStampsTheAccountItCreated` checks the raw store row (`ConsumedAt`, `SpenderEmail`) rather than the derived `InviteView`/JSON shape, and no test mints an invite, advances the clock past `InviteLifetime`, and lists it.
**Why it matters**: AGENTS.md calls out "pure logic is written test first," and this is exactly that kind of logic — a four-way branch with a specific documented ordering rationale, sitting right at what a caller displays and could reasonably rely on. It's currently only checked manually per `verify.md`'s "Value sourcing checks." A regression here (e.g. someone reordering the branches or a display bug) would slip past the whole automated suite.
**Suggested fix**: One small table-driven test in `internal/identity` calling `InviteStateOf` directly across the four cases, plus one httpapi- or web-level test that mints an invite, advances the fake clock past `InviteLifetime`, and asserts the list shows `state: "expired"`.

## Nits

- ⚪ `internal/identity/invites.go:33`, `InviteState` values are unexported-adjacent constants (`InviteLive`, `InviteSpent`, ...) but nothing stops a caller comparing a raw string against them without going through `InviteStateOf`; fine as is, just worth knowing the type doesn't close off construction the way an unexported field would.
- ⚪ `internal/web/templates/invites.html:56`, the table has no "Created" column even though the row carries `CreatedAt`; not required by AC-8 (which asks for note, issuer, expiry, state) but the field exists on `InviteView` unused on this page — likely deliberate, worth a one-line note if so.

## Strengths

- The store transaction (`internal/store/invites.go:SpendInviteAndCreateAccount`) is genuinely well designed: the guarded update runs before the insert so a dead invite never costs an account row, `consumed_by` is `DEFERRABLE INITIALLY DEFERRED` specifically so the FK can point at a row that doesn't exist yet mid-transaction, and the migration's comment explains why that matters. `TestOneInviteMakesOneAccountUnderRace` and `TestATakenAddressLeavesTheInviteLive` prove AC-4 and AC-10 against real SQLite rather than asserting it by inspection.
- The raw-code discipline is carried consistently end to end: never logged with its value in an error string (`internal/store/invites.go:34`'s comment on `CreateInvite` calls this out explicitly), never in an audit row, and `leak_test.go` was extended to crawl the invite code as a forbidden value across every admin/auth page.
- `GET /register` (AC-18) genuinely never touches the invites table — it only copies the query param into a hidden field — so the page cannot become a second oracle, and `TestTheRegisterPageNeverValidatesTheCode` pins exactly that behavior across five code types.

## Test coverage

Extensive and well targeted: the race (`-race`, real SQLite), the taken-address rollback, all four dead-invite states reading identically, bootstrap mint-once-on-empty, CSRF on both page mutations, admin-only access with bearer tokens excluded, and the audit rows for issue/revoke/refused-revoke/spend are all covered by dedicated tests across `internal/store`, `internal/identity` (via httpapi), `internal/httpapi`, and `internal/web`. The two gaps noted above (the wrong reason code going unasserted, and the derived `expired`/`spent` list states not being asserted at the service/API layer) are the only holes; nothing else in the diff appears to have untested branching, error-handling, or security-relevant logic.
