# Review, test/mail-sender-and-signin-backoff, 2026-08-13

**Reviewed by**: Claude Sonnet 5 (author on Claude Sonnet 5)
**Scope**: 3 files, branch vs main (merge base d1f77eb)
**Verdict**: Approve

## Summary

This is a test-only diff closing the two coverage gaps the scope doc flagged: `internal/mail` had zero tests, and the sign-in lockout backoff (AC-23) was only proved as far as its first 30-second window. Both new suites use an injected `Clock`/`http.RoundTripper` rather than real time or a real HTTP server, so every case is deterministic and fast (`go test -race` for both packages: ~0.6s each, no sleeps). I read `internal/identity/limiter.go` and `internal/mail/mail.go` line by line against the new tests and ran the suites with `-race`, `-cover`, `gofmt -l`, and `go vet`; all clean. `limiter.go` reaches 100% statement coverage from `limiter_test.go` alone. The doubling-and-cap arithmetic, the per-address/per-client bucket isolation, the sweep, and the Resend request/error shapes are all pinned against real observed values rather than mocked-out behavior, which is exactly what the AGENTS.md warning about the fake clientset asks for in the HTTP/K8s-adjacent tests — these are the pure-logic and outbound-HTTP counterparts, and they're written the same way: exercise the real code path with a substituted boundary, not a stub of the assertion.

## Strengths

- `TestLockoutIsCappedSoAnAddressIsNeverLostForGood` runs 80 failures and asserts `left > 0 && left <= 15*time.Minute`. This catches a real class of bug: `lockoutBase << (a.failures - failuresBeforeLockout)` in `limiter.go:129` shifts a `time.Duration` (int64) by up to 75 bits, which mathematically overflows through negative values before zeroing out. Without this test, a boundary regression in the shift or the `delay <= 0` guard on `limiter.go:130` would silently produce an inert (zero-delay) or permanent lockout, and nothing else in the suite would catch it.
- `TestBucketRefillIsCappedAtCapacity` and `TestSweepDropsIdleEntriesRatherThanGrowingForever` both test the boundary condition (an idle client/address for a long time) rather than only the common path, matching the guide's call to trace non-obvious paths.
- The mail tests assert what the error string does *not* contain (recipient address, API key, message body) as well as what it does, directly pinning the AGENTS.md-adjacent redaction discipline described in `mail.go:58-60` and `mail.go:85-89` ("an error string is one more place a caller's address could end up in a log").
- `TestLimiterIsSafeUnderConcurrentUse` is a real addition given both maps in `Limiter` are shared across every request goroutine; it would catch a missing lock regression under `-race`.
- Tests use a hand-advanced fake clock (`dial`) and a function-based `http.RoundTripper`, so nothing here depends on wall-clock timing or an external service. No test leaks state into another (each constructs its own `Limiter`/`Sender`).
- `docs/scope/scope.md` is updated with a specific, falsifiable description of what was added and a coverage number, not just a checkbox flip.

## Minor
### 🟡 AC-27 label overclaims what the assertion proves, `internal/mail/mail_test.go:105-137`
**Problem**: The doc comment on `TestSendReportsAProviderRefusalWithoutItsBody` says it's "the AC-27 half this package owns," but AC-27 (`docs/specs/0007-accounts-tokens-app-ownership/index.md:57`) enumerates raw tokens, session ids, link tokens, and passwords — not recipient addresses or the mail API key. The test's real value (proving the error is `resp.Status` alone, never the response body) is real and worth keeping, but it isn't strictly an AC-27 case; recipient-address non-leakage is a `mail.go`-local design decision documented at `mail.go:58-60`, not one of the four items AC-27 names.
**Why it matters**: A future reader tracing AC-27 coverage by grepping `covers: AC-27` will credit this test with proving something the spec item doesn't actually claim, and could mistakenly believe recipient-address redaction is spec-mandated (so removing it later feels riskier than it is) or, conversely, miss that AC-27's actual four items are otherwise proved only at the HTTP/store layer, not here.
**Suggested fix**: Reword the comment to describe it as mail's own no-body-leak guarantee, and drop the `covers: AC-27` tag (or point it at the design note in `mail.go` instead).

## Test coverage

Both new files are pure additions with no existing coverage to compare against. `internal/mail` reaches 90.5% package coverage (the only gaps are the `Client: nil` default path and the unreachable JSON-marshal-error branch, neither worth a test). `internal/identity/limiter.go` reaches 100% from `limiter_test.go` alone. Verified `gofmt -l`, `go vet ./internal/identity/... ./internal/mail/...`, and `go test -race ./internal/identity/... ./internal/mail/...` all pass clean. The scope.md claim of "86.8% cross-package coverage" for identity+auth+mail is in the right neighborhood (a `-coverpkg` run across the whole suite here measured ~87.4%); the small delta is expected since coverage tools vary by exact invocation, not a documentation error worth flagging.
