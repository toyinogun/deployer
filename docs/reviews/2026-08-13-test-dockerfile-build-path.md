# Review, test/dockerfile-build-path, 2026-08-13

**Reviewed by**: Claude Sonnet 5 (author on Claude Sonnet 5)
**Scope**: 4 files, branch vs main (merge base 92d0921)
**Verdict**: Approve with nits

## Summary
This change pins the three test/documentation criteria that spec 0009's build path was waiting on: a test that `build_failed`'s message names no single engine and points to `build_path` (AC-13), a test that a rootful Dockerfile image is refused as `image_runs_as_root` before any app object is composed (AC-12), and a test that no build output is ever read from pod logs on either engine path (AC-14). `docs/scope/scope.md` and the spec's own task list are updated to mark the corresponding steps done, consistent with the live proof recorded in the spec on 2026-08-13. No production code changed. All new and existing tests pass under `go test -race ./...`, and `gofmt`/`go vet` are clean.

## Strengths
- `TestARootImageFromADockerfileBuildIsRefusedBeforeAnyAppObject` closes a real gap: every other root-user test in the suite (`reconcile_test.go:316`) exercises the Buildpacks path, which always declares a user, so this is the first test that can actually reach `image_runs_as_root` through a Dockerfile-produced image. It also asserts the stronger claim (zero app `Deployments` created), not just the failure reason.
- `TestBuildFailedNamesNoSingleEngine` is exactly the kind of test that would have caught the original bug it documents: it enumerates both engine names plus their vendor names ("cloud native", "paketo") rather than just checking for the literal strings used elsewhere in the codebase.
- Doc updates (`scope.md`, spec index) are terse and match what the spec's own step 16 records as proved live; no overclaiming beyond what step 16 already states.

## Nits
- ⚪ `internal/reconcile/buildpath_test.go:288-292`, `TestNoBuildOutputIsEverReadOnEitherPath`'s log-subresource assertion can never fail today: nothing in `internal/reconcile` holds a capability to call `GetLogs` at all, so this is closer to a structural guarantee than a regression test. The test's own comment is honest about this ("proved live, not here"), so it's fine to keep as a tripwire for if that capability is ever added, but it isn't currently pulling weight against a real code path.

## Test coverage
Test signal is `configured`. This diff is test-and-doc only; the two new `internal/reconcile/buildpath_test.go` cases and the one new `internal/domain/reason_test.go` case are the payload, and all three exercise real, previously-uncovered branches (root-user refusal specifically via the Dockerfile path, and the reworded `build_failed` message). No production code in this diff to leave uncovered.
