# Review, feat/lifecycle-test-gaps, 2026-08-13

**Reviewed by**: Claude Sonnet 5 (author on Claude, model unspecified)
**Scope**: 28 files, feature branch vs `main` (`90913d2^1..HEAD`, spec 0012 merged plus a follow up test commit)
**Verdict**: Approve with nits

## Summary

This change adds `list_apps` and `delete_app` over the existing app and deployment tables, and gives the reconcile loop its first unattended destructive action: an orphan namespace reaper on its own ticker. I traced the delete path's two writes end to end (the database soft delete inside its own transaction, then the single cascading namespace delete) and every ordering, failure, and audit case in spec 0012 is where the spec says it has to be: the row is retired before the cluster is touched, a namespace already gone or terminating is tolerated inside `internal/kube`, a namespace delete that fails still leaves the row deleted and hands the caller `internal`, and a deploy in flight refuses the whole call before anything is written. I gave the reaper's guards the most scrutiny, since the project's own docs call it out as the first unattended destructive loop: the live slug read runs first and any failure aborts the whole pass rather than reading as "nothing is owned," the label selector matches the existing `AppNamespaces` sweep so an unlabelled namespace is invisible in the safe direction, and the grace period is compared against a caller supplied `now` rather than a clock read inside the package, which is what makes the boundary testable to the second. I did not find a way for the reaper or the delete path to remove a live app's namespace. The issues below are all minor: a wire test that checks the wrong result's `IsError` flag, one new branch (`s.cluster == nil` inside `deleteApp`) that has no test of its own, and a coverage assertion that is a little weaker than its name promises.

## Minor

### 🟡 Wire test asserts the wrong result's `IsError`, `internal/mcp/apps_test.go:1279`
**Problem**: `TestListAppsAndDeleteAppOverTheWire` calls `delete_app` a third time with a nonexistent app name and stores the result in `unknown`, then checks `!refused.IsError` instead of `!unknown.IsError`:
```go
unknown := callOverTheWire(t, s, account, "delete_app", map[string]any{"name": "nothing-here"})
if got := resultText(unknown); !refused.IsError || !strings.HasPrefix(got, string(domain.ReasonAppUnknown)) {
```
`refused` is the previous call's result (the `deployment_in_flight` refusal), whose `IsError` is already `true`, so that half of the condition can never fire regardless of what `unknown.IsError` actually is.
**Why it matters**: The test still catches most regressions in practice, because a successful `delete_app` response would not start with `app_unknown`, but the test no longer proves what its own name claims: that the third call was refused over the wire. A future change that made `delete_app` on an unknown name return a non-error result carrying a string that happened to start with `app_unknown` (unlikely, but the point of asserting `IsError` is to not have to reason about that) would pass silently.
**Suggested fix**: Change `!refused.IsError` to `!unknown.IsError`.

### 🟡 `deleteApp`'s no-cluster branch has no test, `internal/mcp/apps.go:213`
**Problem**: 
```go
if s.cluster == nil {
    return nil, deleteAppOutput{}, s.denyConfig(ctx, account.ID, app.ID, auth.ActionAppDelete,
        domain.ReasonInternal, errors.New("no cluster access, so the app's namespace was left in place"))
}
```
is new branching, error handling logic introduced by this change, and no test in `internal/mcp/apps_test.go` constructs a `Server` with `cluster == nil` and calls `deleteApp`. Every test that exercises `deleteApp` goes through `lifecycleServer()`, which always sets `s.cluster` to a non-nil `stubCluster`.
**Why it matters**: This is the same asymmetry the namespace-delete-failure path already has (row soft deleted, caller told `internal`), and it is deliberate per the spec's own "Negative / tradeoffs" section, but nothing pins that a local run without a cluster credential actually takes this branch rather than, say, panicking on a nil interface call or silently reporting success. TESTS = configured, and this is exactly the class of gap the test guide calls out: untested branching/error-handling logic.
**Suggested fix**: Add a case to `lifecycleServer` (or a small variant) that builds a `Server` with `cluster` left nil, and assert `deleteApp` refuses with `internal` while the app row is still recorded as deleted. Note this mirrors a pre-existing, likewise untested `s.pods == nil` branch in `internal/mcp/logs.go:130`, so this is a place to close the gap for both rather than only the new one, if that is in scope.

## Nits

- ⚪ `internal/mcp/apps.go:854` and `:897`, `list_apps` and `delete_app` both route their refusals through `denyConfig`, the same helper `rollback_app` and `list_releases` already reuse for non-configuration actions (flagged as a nit in the prior review of PR #28 too). It writes the right audit row and behaves correctly; a generically named helper (`deny`, matching `s.deny` mentioned in that same prior review) would read more clearly as this project accumulates more non-config tools reusing it, but it is not worth a change on its own.
- ⚪ `cmd/deployer/main_test.go:169`, `TestReconcileOptions_carriesEveryDurationAcrossFromTheConfig` walks every `time.Duration` field on `reconcile.Options` and asserts it is positive, which is exactly right for the historical bug it documents (a field left unassigned arriving as zero and crash-looping `time.NewTicker(0)`). It would not catch a copy-paste swap between two positive-but-different duration fields (for example `ReapInterval: cfg.ReconcileInterval`), since both are still positive. Worth a follow-up assertion that ties each field to its own expected value if this class of bug recurs, but the test as written is a real improvement over having nothing.

## Strengths

- The reaper's guards are exactly where the spec's "Key invariants" section says they have to be, and each one is pinned by its own test rather than inferred from the happy path: `TestAFailedSlugReadReapsNothing` proves the pass aborts on a broken slug read using a store stub whose every other method panics if reached, `TestTheGraceKeepsAFreshNamespaceOutOfReach` proves the boundary in both directions with an explicit `CreationTimestamp` (the fake clientset would otherwise leave it zero, which would pass for the wrong reason and the test comments say so), and `TestOneUndeletableNamespaceDoesNotStopThePass` proves a stuck namespace does not take the rest of the pass down with it.
- The delete path's ordering is provably correct rather than merely documented: `TestADeleteRetiresTheRowThenTheNamespace` asserts on the stub's own action list (`apps.deleted`, `cluster.deleted`) rather than only the final response, and `TestANamespaceDeleteThatFailsStillLeavesTheRowDeleted` and `TestADeployInFlightRefusesTheWholeDelete` both pin the two failure asymmetries the spec calls out by name (row gone but namespace not, versus nothing written at all).
- `internal/kube.DeleteNamespace`'s read-then-delete shape keeps the NotFound/Terminating tolerance entirely inside `internal/kube`, matching the existing `ignoreExists` pattern, and `internal/kube/namespace_test.go` proves both tolerated cases and the one that is not with a `PrependReactor` on the fake clientset rather than by inspecting the final state alone.
- `TestReconcileOptions_carriesEveryDurationAcrossFromTheConfig` is a genuinely good regression test: its own doc comment names the production incident it exists to prevent (a config field silently reaching the reconcile loop as its zero value), and reflecting over every `time.Duration` field means a future field added to `Options` and forgotten in `reconcileOptions` fails this test immediately rather than waiting for the next `DEPLOYER_*` variable to hit the same bug.
- `internal/mcp/deleted_test.go`'s `TestEveryToolRefusesADeletedApp` earns AC-32 honestly: it deploys and cancels a real deployment through the store rather than faking a deleted app's shape, then drives every other tool's actual refusal path, which is exactly the "pin it rather than assume it" the spec asks for.

## Test coverage

Test signal is `configured`. Coverage is thorough and traceable to acceptance criteria across all four build tasks: `internal/config/lifecycle_test.go` (defaults, validation, override), `internal/kube/namespace_test.go` (delete tolerance, age filtering), `internal/reconcile/reaper_test.go` (the four guard scenarios above), `internal/store/applisting_test.go` (the listing query against real SQLite, the slug-stays-reserved/name-comes-free split, the configuration-free projection), `internal/mcp/apps_test.go` (both tools' handler logic and the wire session), and `internal/mcp/deleted_test.go` (the after-delete behaviour of every other tool). I did not find untested security-relevant logic; the two gaps I found (above) are a test asserting the wrong variable and one narrow, deliberately-asymmetric branch with no coverage of its own. I ran `go build ./...` as part of this review and it is clean; I did not re-run `go test -race ./...` myself, taking the stated "full suite passes" at face value.
