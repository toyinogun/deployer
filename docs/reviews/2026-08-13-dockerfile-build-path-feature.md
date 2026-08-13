# Review, test/dockerfile-build-path, 2026-08-13

**Reviewed by**: Claude Sonnet 5 (author on Claude, session unspecified)
**Scope**: 47 files, feature range (`9b47e20a` diff `test/dockerfile-build-path`)
**Verdict**: Approve with nits

## Summary

This slice adds a second build engine (rootless BuildKit) selected by reading an uploaded archive's tar headers on the control plane, records the choice on the deployment row before the Job exists, and routes every later cluster call (Job create, credential, poll, delete) through a namespace derived from that recorded path rather than re-derived from the archive. The Kubernetes surface (two namespaces, two pod security levels, four named deviations on the BuildKit pod) is composed field by field and pinned with strong unit tests, including a test that the two namespaces' NetworkPolicy files stay identical in shape and a test that the resumed/watchdog path addresses the Job in the right namespace after a simulated restart. Test coverage is unusually thorough and directly targets the acceptance criteria in the spec. The one real gap is that path detection (`source.HasRootDockerfile`) walks tar headers without bounding the total bytes it reads through the gzip stream, unlike `Extract`, which does bound it — a crafted archive can make detection decompress far more than its compressed size implies, on the single pod that also holds the database.

## Major

### 🟠 Path detection has no decompression bound, unlike extraction, `internal/source/detect.go:26`
**Problem**: `HasRootDockerfile` bounds only the entry *count* (`lim.MaxFiles`) as it walks `tr.Next()` in a loop. `archive/tar`'s `Reader.Next()` has to discard the unread body of the previous entry before returning the next header, and that discard reads through the underlying `gzip.Reader`, i.e. it decompresses. `Extract` (`internal/source/extract.go:58`) bounds this explicitly: it tracks `remaining := lim.MaxBytes` and calls `writeFile` with that ceiling, so extraction can never decompress more than `DEPLOYER_MAX_EXTRACTED_BYTES`. `HasRootDockerfile` receives the same `Limits` value (`reconcile.go` passes `MaxBytes: r.opts.MaxExtractedBytes`) but the field is never read in `detect.go` — it is dead on this path.
**Why it matters**: A caller can upload a small, highly compressible gzip stream (a large run of zero bytes compresses at roughly 1000:1 under DEFLATE) that is well under `DEPLOYER_MAX_UPLOAD_BYTES` (100MB) but decompresses to tens of gigabytes. `startBuild` calls `buildPath` — and therefore `HasRootDockerfile` — for every queued deployment, before any Job or credential exists, in the reconcile loop of the single control-plane pod that also holds the SQLite database. An archive built to trigger this stalls or exhausts memory/CPU on that pod during the walk. This directly undercuts the property the function's own doc comment claims ("reads headers only... stops at the same entry limit `Extract` enforces, so an archive of nothing but headers cannot make the control plane walk without limit") and the spirit of spec 0009 AC-3 ("the walk is bounded... so a header only archive cannot make the control plane work without limit") — the bound that exists (entry count) does not cover this case, and the bound that would cover it (byte count) is threaded through as an argument and silently dropped.
**Suggested fix**: Wrap the reader passed into `tar.NewReader` (or the `gzip.Reader` itself) in an `io.LimitReader`/counting reader bounded by `lim.MaxBytes`, so a stream that would decompress past the limit fails fast with `ErrRejected` instead of continuing to decompress. `Extract`'s `writeFile` already has the pattern to mirror.

## Minor

### 🟡 CI's BuildKit `Config.User` split mishandles a uid-only value, `.github/workflows/ci.yml` (builder-uid step)
**Problem**: `user=$(config ... | jq -r '.config.User // ""')` is split with `${user%%:*}` (uid) and `${user##*:}` (gid). If an OCI image ever declares `User` as a bare uid with no `:gid` (valid per the OCI spec, defaulting gid to 0 at the runtime), `${user##*:}` returns the whole string unchanged (no `:` to strip), so the "gid" compare silently checks the uid value against `DEPLOYER_BUILDKIT_GID` instead of catching the missing gid.
**Why it matters**: Low likelihood — the currently pinned `moby/buildkit` image declares `"1000:1000"` — but a future repin to an image that declares only a uid would either false-pass or false-fail this drift check in a confusing way, defeating the purpose of AC-9 (CI fails on real drift, not on a formatting surprise).
**Suggested fix**: Detect the no-colon case explicitly (`case "$user" in *:*) ;; *) echo "no gid in Config.User" ;; esac`) rather than relying on `##*:` degrading gracefully.

## Nits

- ⚪ `internal/build/job.go:56`, `Path`'s zero value quietly means Buildpacks by design (documented), which is fine, but nothing stops a caller from constructing `Input{Path: "bogus"}` and getting Buildpacks silently rather than a construction-time error — acceptable given `Path` is never caller-supplied, just worth knowing if this type is ever exported further.
- ⚪ `docs/specs/0009-dockerfile-build-path/index.md` follow-up section already flags the admission-refusal reason-code gap (`build_failed` after the full deploy budget on a mis-routed Job) as deliberately deferred; no action needed here, just noting it was read and is intentional, not missed.

## Strengths

- The four named security deviations on the BuildKit pod are pinned by `TestDockerfilePodDeviatesInExactlyFourFields` (`internal/build/buildkit_test.go:166`), which checks both that the Buildpacks pod still satisfies every `restricted` field *and* that BuildKit deviates in exactly those four fields and no more — a genuinely strong regression fence for a security-relevant pod shape.
- `TestAResumedDockerfileBuildDeletesItsJobInTheRightNamespace` (`internal/reconcile/buildnamespace_test.go:88`) simulates a real restart (row rebuilt via `store.ForReconcile(w.store).ListNonTerminal`, not built by hand) and proves the watchdog deletes the Job in the namespace it actually lives in — exactly the AC-19a scenario the spec calls out as easy to get wrong.
- The `COALESCE` fix to `RecordBuildResult` (`internal/store/queries/deployments.sql`) correctly prevents the second `RecordBuild` call (image digest) from clobbering the `build_path` the first call wrote, and the Go-side `ptr()`-on-non-empty pattern in `store.RecordBuildResult` means empty Go strings correctly become SQL `NULL` rather than empty-string overwrites — this was worth double-checking given COALESCE's well-known "empty string is not NULL" trap, and it is done correctly.
- `TestTheTwoBuildFencesHoldTheSameRules` (`internal/config/buildspolicy_test.go`) diffs the two namespaces' NetworkPolicy specs against each other, not just against a hardcoded shape, so the two files structurally cannot drift apart without failing — directly satisfies AC-21's stated concern.

## Test coverage

Coverage is thorough and well-targeted at the spec's acceptance criteria: detection (root/nested/case/directory-named Dockerfile, corrupt stream, entry-limit), routing to both namespaces including the resumed/watchdog path, failed-build path reporting, root-image refusal, no-build-output-read assertion, and the full security-context pinning for both engine pods. No missing-coverage findings beyond the detection byte-bound gap above, which is a logic gap rather than a test gap — a test could be added once the fix lands (feed `HasRootDockerfile` a small highly-compressible stream and assert it is refused rather than left to run unbounded).
