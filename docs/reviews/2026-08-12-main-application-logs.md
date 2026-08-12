# Review, main, 2026-08-12

**Reviewed by**: Claude Sonnet 5 (author on Claude, model unspecified)
**Scope**: 17 files, commit range `a8295d7~1..HEAD` (four commits) vs `main`
**Verdict**: Approve with nits

## Summary

Adds `get_logs`, a bounded, redacted, live read of an app's own pod output over MCP: pure parse/redact/bound logic in `internal/logs` (import-clean of client-go), two new methods on `internal/kube`, and the tool itself in `internal/mcp/logs.go`, wired through `cmd/deployer/main.go`. The empty-case gate (decide from pod status before ever calling the log API), the independent current/previous byte-and-line ceilings, the closed and audited `app_unknown`/`internal` reason split, and the RBAC-already-granted check are all implemented exactly as spec 0006 describes, and the test suite (`logs_test.go`, `kube_test.go`, `mcp/logs_test.go`) exercises essentially every acceptance criterion, including the negative-space ones (log API never called in the empty case, no audit row on a fault, no partial answer when the previous read fails). `go build`, `go vet` are clean. The one substantive gap is a partial-redaction hole in the `key|token|secret|password` assignment pattern that a caller could easily mistake for a full block.

## Minor

### 🟡 The assignment redaction pattern stops at the first space, so a multi-word secret is only half blanked, `internal/logs/logs.go:148`
**Problem**: The value class in the fifth pattern, `[^\s",;}]+`, excludes whitespace. `PASSWORD=hunter two` or `{"password": "correct horse battery"}` (or the very passphrase-style secrets these tools most often protect, e.g. a multi-word app secret with an errant space, or a value like `API_KEY: sk live abc123` split by a formatter) redacts only the first token and leaves the rest of the value in clear, right next to a `[REDACTED]` marker that reads as if the whole value were removed.
**Why it matters**: This is worse than not matching at all — the output visibly claims "I redacted this" while the remainder of the secret is still readable in the very next word. A caller skimming for the `[REDACTED]` marker and trusting it will miss the leaked tail. AC-6 and the tool description already frame redaction as best effort, so this doesn't block merge, but a value-with-space is common enough (generated passphrases, base64 with padding stripped oddly, human-typed secrets) that it's worth closing.
**Suggested fix**: Widen the value class to also match up to end-of-line, a comma, or a closing brace/quote when the value clearly continues past a space that isn't a plausible separator — or, simpler, run a second pass that treats the character run from the assignment through end-of-line (bounded to a sane max) as the value when no comma/brace/quote is seen first. At minimum, add a test case pinning the current (partial) behavior so it's a documented tradeoff rather than a silent gap.

## Nits

- ⚪ `internal/mcp/logs.go:174`, `emptyCase`'s `noPod` parameter is really "no pod was found" but is passed as `len(pods) == 0` at one call site and `true` (namespace not readable) at the other — both correct, but a doc comment noting the second call site treats "namespace not readable yet" as "no pod" would save a reader the cross-reference to `readLogs`.
- ⚪ `cmd/deployer/main.go`, the `podReader` helper exists solely to convert a nil `*kube.Client` into a nil `mcp.Pods` interface (the classic Go nil-interface trap) — worth a one-line comment at the call site too, since `buildAPI` is where the trap would otherwise resurface if someone passes `cluster` directly in a future edit.

## Strengths

- The pod-status gate in `readLogs` (`internal/mcp/logs.go:134-140`) genuinely never calls the log API for the empty case, and every empty-case test (`TestLogsEmptyCaseIsASuccess`, `TestLogsWhileTheNamespaceIsNotThereYetIsTheEmptyCase`) proves it via a `stubPods.logErr` that would fail the test if the log path were ever reached — that's a real assertion, not a mocked no-op.
- `internal/logs.Bound` correctly composes the line-count trim and the byte-ceiling trim into one `dropped` count while always keeping the newest entry even when it alone exceeds the ceiling (`TestBoundOnOneOversizeEntry`); the "oversize single entry" edge case is exactly the kind of thing a naive tail-and-truncate implementation gets wrong.
- The independent previous-container cap is proven under contention (`TestLogsCarriesThePreviousContainerAfterARestart` deliberately floods the current block past its ceiling and still asserts the previous block is intact), and `TestLogsFailsWholeWhenOnlyThePreviousContainerReadDies` proves the "no partial success" invariant holds even after the current block is already redacted and bounded in memory.
- `internal/kube.PodsForApp` maps both `Forbidden` and `NotFound` from the fake clientset to the same `logs.ErrNoNamespace`, matching the documented reality that Kubernetes RBAC declines to distinguish "doesn't exist" from "you can't see it" — and the verify.md log confirms this was checked live against the real cluster, not just asserted in the fake.
- `deploy.selector` was correctly promoted to exported `Selector` rather than duplicated in `internal/kube`, keeping the Deployment/Service selector and the log-read selector as one source of truth — a real instance of the "no string templating, compose in Go" rule paying off across packages.

## Test coverage

Coverage is thorough and well targeted at the spec's own "critical test scenarios" list. Every AC has at least one test exercising both the positive and a plausible negative path (empty case vs fault, refusal vs internal, redacted vs preserved). The redaction test suite (`TestRedact`) explicitly includes a "long line that is merely long" and "ordinary hostname is not read as a JWT" case, proving false positives are checked as well as false negatives — but it does not include a multi-word/spaced secret value, which is exactly the gap noted above; adding one would have caught it before review.
