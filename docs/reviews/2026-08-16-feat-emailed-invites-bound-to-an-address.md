# Review, feat/emailed-invites-bound-to-an-address, 2026-08-16

**Reviewed by**: Sonnet 5 (author on unspecified model)
**Scope**: 25 source files (plus 3 doc files), branch vs `main` (merge base `fc2db8b`)
**Verdict**: Approve

## Summary

Spec 0025 adds an optional address to invite minting: the address is folded into the `LiveInviteByCodeHash` predicate rather than compared after the fact, so a bound invite presented with the wrong address is byte-for-byte the same `invite_invalid` refusal as an unknown, spent, revoked or expired code, at the same query cost. The taken-address guard runs inside the same `BEGIN IMMEDIATE` transaction as the insert, following the `CreateApp` convention. Both admin surfaces (page and JSON) take the same optional field, the same precedence, and the same inline send. I traced the refusal precedence, the audit rows, the CSRF/origin fix, and the migration compatibility case against the spec and found no discrepancy. This is a clean, carefully reasoned implementation; the CSRF opaque-origin fix is a real bug fix, correctly scoped to the mechanism that makes it safe.

## Strengths

- The binding lives entirely in the SQL predicate (`AND (email IS NULL OR email = @candidate)`) rather than as a Go-level comparison after the lookup, which is exactly what makes AC-8's indistinguishability claim true by construction instead of by careful ordering. The rationale doc records that this was a correction to the first draft, and the shipped code matches the corrected design.
- The refusal precedence in `IssueInvite` (note → email format → nil mailer → taken address) matches the spec's documented order exactly, including the reasoning that the nil-mailer check precedes the account read so an unconfigured platform never queries the accounts table for a question it cannot act on.
- No audit row, log line, or response body carries the bound address outside the invite row and the one mailed message, verified by tracing `inviteroutes.go`, `invites.go` (web), and the store's `CreateInvite`/`AccountExistsByEmail` error paths, and confirmed by the new `TestNoAuditRowCarriesTheBoundAddress`.
- The CSRF opaque-origin fix (`csrf.go`, `pretoken.go`) is narrowly scoped: `opaqueIsAbsent` is `true` only on the pre-authentication path, `false` on the authenticated one, and the safety argument (the `SameSite=Lax`, `__Host-`-prefixed nonce cookie still can't be produced by a genuinely cross-site post) is sound and matches the pre-existing threat model, which already trusted an *absent* Origin header on this same path for the same reason.
- The migration is a single nullable `ALTER TABLE ... ADD COLUMN`, with no default and no constraint, and the compatibility path (`email IS NULL OR email = @candidate`) is covered by both a store-level test (`TestEveryDeadInviteReadsTheSame`, `TestATakenAddressLeavesTheInviteLive`) and an end-to-end one (`TestAnUnboundInviteStillTakesAnyAddress`) that exercises the bootstrap invite shape specifically.

## Minor
### 🟡 No test proves the mint-refusal precedence when two conditions are simultaneously true, `internal/identity/invites.go:181`
**Problem**: `IssueInvite`'s doc comment and the spec both state a specific precedence order when several refusals could apply (note → email format → nil mailer → taken address), but no test submits, say, a malformed address *and* an over-long note, or a taken address on a platform with no mailer, to confirm which refusal wins.
**Why it matters**: The order is a documented invariant (spec 0025, "Refusal precedence at the mint"), and precedence bugs are exactly the kind of thing that survive individual-case tests while failing a combined one — e.g., a future refactor that checks `mailer == nil` before `CheckEmail` would still pass every existing single-condition test.
**Suggested fix**: Add one table test in `internal/identity` or `internal/httpapi` that submits combinations of bad note / bad email / no mailer / taken address and asserts which single code comes back.

### 🟡 No test exercises the `CreateInvite` taken-address guard under real concurrency, `internal/store/invites.go:80`
**Problem**: The transaction guard's whole justification (spec 0025 AC-3, and the store's own `AGENTS.md` convention) is that a registration landing between the read and the insert cannot produce a bound invite for an address that already has an account. `TestOneInviteMakesOneAccountUnderRace` proves a different race (two registrations against one invite); nothing drives a concurrent registration against a concurrent mint to the same address the way that existing test drives its own scenario.
**Why it matters**: This is the one line the migration plan itself flags as worth reading twice, and it is currently proven only by the transaction shape (`BEGIN IMMEDIATE`) and by a sequential test (`TestARefusedMintWritesNothing`), not by an actual interleaving.
**Suggested fix**: A goroutine-based test mirroring `TestOneInviteMakesOneAccountUnderRace`'s shape, racing a bound mint against a registration that just created the same address, would close the gap. Not blocking — the guard is the same one `CreateApp` already uses and trusts.

## Nits
- ⚪ `internal/web/csrf.go:63-78`, the doc comment on `checkOrigin` explains the opaque-origin exception in terms of "the register page's own no-referrer header," but the parameter is threaded through `checkPreCSRF` for all five pre-authentication forms (login, register, forgot, reset, resend), not just register. The security argument still holds for all five (the nonce/SameSite pair is what actually guards them), but the comment reads as if register were the only page this touches.
- ⚪ `internal/identity/invites.go:224`, `inviterName`'s fallback to `thePlatform` when the admin's `DisplayName` is empty or the lookup fails is untested directly (only indirectly, via the display name every test harness gives its admin accounts).

## Test coverage

Test signal is `configured`. Every acceptance criterion in spec 0025 has at least one test exercising it, spanning the store (`invites_test.go`), identity (`nomailer_test.go`), httpapi (`boundinvites_test.go`), and web (`boundinvites_test.go`, `boundaudit_test.go`, `opaqueorigin_test.go`) packages, matching the project's convention that most `Service` coverage lives in the surface harnesses rather than in `internal/identity` itself. The compatibility case (AC-16), the indistinguishability case (AC-8), the audit boundary (AC-13), the no-resend property (AC-17), and the CSRF fix all have dedicated tests. `go build ./...` and `go test ./internal/store/... ./internal/identity/... ./internal/httpapi/... ./internal/web/...` pass. The two gaps above (combined-refusal precedence, and true concurrency on the taken-address guard) are the only untested paths worth naming, and neither is a correctness risk on its own — both are already provable by the code's structure — so they are Minor rather than blocking.
