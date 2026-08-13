# Review, feat/isolation-test-pins, 2026-08-13

**Reviewed by**: Claude Sonnet 5 (author on unspecified model)
**Scope**: 28 files, branch vs base (`b50cb2b`, spec 0008 workload isolation and network policy, PR #11 plus three follow-up commits)
**Verdict**: Approve with nits

## Summary

This lands the network fence spec 0008 designs: `DefaultDenyPolicy`/`AllowPolicy` composed field by field in `internal/deploy`, applied before the Deployment and made fatal on failure, a startup `PolicySweep` that retrofits pre-existing app namespaces, a static YAML pair for the build namespace with a drift test pinning its `except` list against the Go default, structural pins for pod-spec privilege absence and `Input` passthrough absence, and a probe app for live verification. The implementation is faithful to the spec line for line: selector shapes, port numbers, write order, and fail-closed behavior all match the acceptance criteria, and `go build`, `go vet`, `golangci-lint`, and `go test -race ./...` are all clean. I found no correctness, security, or convention issues that should block merge — only a couple of minor robustness points in test code.

## Minor

### 🟡 A nil `Capabilities` would panic the test instead of failing it, `internal/deploy/isolation_test.go:538`
**Problem**: `assertContainerIsFenced` first checks `sc.Capabilities == nil || len(sc.Capabilities.Add) != 0` (line 538, short-circuits safely), but then unconditionally does `for _, cap := range sc.Capabilities.Drop` a few lines later with no nil guard. If a future change to `Deployment`/`Job` ever left `SecurityContext.Capabilities` nil, this specific AC-17 pinning test — whose entire job is to catch exactly that kind of regression — would panic with a bare nil-pointer stack trace instead of reporting a clear `t.Errorf`.
**Why it matters**: This is the test that exists specifically to catch a container losing its capability drop. A panic instead of a clean failure message makes the CI output harder to read at the one moment it matters, and (depending on how failures are aggregated) can obscure other subtests' results.
**Suggested fix**: Guard the loop the same way the check above it does, e.g. `if sc.Capabilities != nil { for _, cap := range sc.Capabilities.Drop { ... } }`, or return early after the first `t.Errorf` when `sc.Capabilities == nil`.

## Nits

- ⚪ `internal/config/isolation.go:379`, `prefix.String()` preserves whatever host bits an operator typed (e.g. `10.0.0.5/8` stays `10.0.0.5/8` rather than being normalized to `10.0.0.0/8`). Harmless with the shipped default and likely accepted by the API server either way, but `prefix.Masked().String()` would make a hand-typed override always canonical.
- ⚪ `internal/deploy/isolation_test.go:542`, the loop variable `cap` shadows the builtin `cap()`. Not flagged by golangci-lint's current config, but worth renaming (`c` is already taken by the container, so e.g. `capability`) if `predeclared`/shadow linting is ever turned on.

## Strengths

- The composed policy objects (`internal/deploy/networkpolicy.go`) and the static build-namespace YAML are provably kept in lockstep: `TestTheBuildNamespacePolicyMatchesTheBlockedDefault` and `TestTheBuildNamespaceEgressIsCoreDNSTheRegistryAndThePublicInternet` parse the real YAML into the real API types rather than eyeballing text, so a hand edit to either copy of the blocked-CIDR list fails CI exactly as AC-20 requires.
- `TestThePoliciesAreWrittenBeforeTheDeployment` and `TestAPolicyWriteFailureEndsTheDeploymentWithNoWorkload` prove the fail-closed ordering (AC-13) directly against the fake clientset's write order and the absence of a Deployment object, rather than asserting on a mocked call — a real regression here (policy after workload, or a swallowed write error) would be caught.
- `TestAllowPolicyCopiesTheBlockedList` catches an aliasing bug class (mutating the caller's slice after composition) that is easy to introduce silently and easy to miss in review; good defensive test.
- `PolicySweep` correctly reads namespace state off the cluster rather than the database (AC-12), and `TestPolicySweepCarriesOnPastAFailure` / `TestPolicySweepWritesNoDeploymentState` pin both the isolation-of-failure and the no-deployment-state-touched properties the spec calls out as what makes it different from `Sweep`.
- `deploy.Input`'s no-passthrough invariant (AC-18) is enforced by reflection over the struct's field kinds and names, not by a hand-maintained list of "known good" fields, so a future field addition is checked automatically rather than relying on the next author to remember the rule.

## Test coverage

Every acceptance criterion in spec 0008 that a unit test can reach (AC-1 through AC-5, AC-11 through AC-15, AC-17, AC-18, AC-20) has a corresponding table-driven or object-shape test, and `verify.md` correctly defers the ones that can't (AC-6 through AC-10, AC-16, AC-19) to the real-cluster probe run, consistent with the repo's stated rule that the fake clientset resolves no names and enforces no policy. No untested new logic found; the one gap noted above is a robustness issue in an existing test, not missing coverage.
