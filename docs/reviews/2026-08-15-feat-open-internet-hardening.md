# Review, feat/open-internet-hardening, 2026-08-15

**Reviewed by**: Claude Sonnet 5 (author on unspecified model)
**Scope**: 26 files, branch vs main (merge base `7d64a0a`)
**Verdict**: Approve with nits

## Summary

Implements spec 0019 in full: a signed double-submit cookie for the five pre-authentication forms (`/login`, `/register`, `/forgot`, `/reset`, `/resend`), and a two-object ingress fence around `deployer-system` (a `networking.k8s.io/v1` pair for pod-sourced peers, a namespaced `CiliumNetworkPolicy` for node-sourced ones by identity). Both halves match their acceptance criteria closely, are backed by table/edge-case tests that exercise the failure paths (malformed cookie, wrong field, cross-site origin ordering, rate-limit ordering, two-tab reuse), and the network policy shape is pinned by parse tests in `internal/config` the same way the existing build policies are. `gofmt`, `go vet`, `golangci-lint`, and `go test -race ./...` (including the full repo, not just the touched packages) are all clean. AGENTS.md and the three touched specs (0008, 0013, 0019) were updated consistently with the code. No blockers or majors found; a couple of minor documentation/process gaps are worth a look.

## Minor

### 🟡 Two verify.md steps for AC-12 and AC-19 are left unchecked while scope.md marks verification done, `docs/specs/0019-open-internet-hardening/verify.md:34,38`
**Problem**: `verify.md` line 34 (the in-namespace registry probe proving the control plane's own peer on port 5000, AC-12/AC-14) and line 38 (the AC-19 stop condition) are unchecked (`- [ ]`), along with the plain-HTTP local run at line 61 (AC-2a). Yet `docs/scope/scope.md`'s feature 20 entry marks `Verify it: /check verify open internet hardening` as done (`[x]`).
**Why it matters**: AC-12's in-namespace peer is called out in the spec itself as the exact bug that broke every deploy the first time this policy was attempted ("leaving this out breaks every deploy... failure lands one step later wearing the reason code `build_no_digest`"). If that specific live check genuinely was not run, the fence's riskiest peer is unproven, and the scope checklist currently overstates what was verified.
**Suggested fix**: Either run the three unchecked steps against the cluster and check them off, or if they were run and the file just wasn't updated, tick them so `verify.md` reflects reality. Low severity because the deploy-to-`healthy` step (line 35/36) and the console sign-in (line 31) each transitively exercise the same registry reachability path, so it is very likely already proven; this is a paperwork gap rather than a functional one.

## Nits

- ⚪ `internal/web/pretoken.go`: `preAuthForm.withPreCSRF` returns `any` rather than a named type, since `formPage` and `unverifiedPageData` don't share a concrete return type. Reasonable given the two page shapes, but it does mean a caller can't chain further methods without a type assertion. Not worth a generic for two implementers.
- ⚪ `docs/specs/0019-open-internet-hardening/verify.md:34,38,61` (also listed above): consider a bot/human sweep of open checkboxes before a feature's scope row is marked `Verify it: done`, so the two documents can't drift.

## Strengths

- The origin-check-before-nonce-check ordering (`checkPreCSRF` in `internal/web/pretoken.go`) is directly pinned by `TestTheOriginCheckAnswersACrossSitePostBeforeTheNonceCheck`, including the subtle case (a pairless cross-site post must not be handed a fresh nonce cookie minted for a page it never legitimately loaded). This is exactly the kind of ordering bug that "reads as a tightening and is really a lockout" per the spec's own invariant, and it's tested both ways.
- `TestAMalformedNonceCookieIsTreatedAsAbsent` walks four adversarial cookie shapes (empty, one-hex-short, one-hex-long, right-length-but-not-hex) and confirms none of them lets an empty posted field match, closing the exact bypass the length check exists to prevent (`preCSRFToken("")` returning `""`).
- The Cilium node policy correctly encodes the two identities (`host` for kubelet probes, `remote-node` for cross-node image pulls) discovered the hard way per the spec's rationale (address-based `ipBlock` peers silently admitted nothing), and `internal/config/nodepolicy_test.go` deliberately types the port as a string to catch the int32-decodes-to-zero trap the spec calls out — a real "the parse test would have missed this" catch turned into a pinned regression test.
- `TestARefusedPostDoesNotSpendARateLimitAttempt` and `TestTwoTabsOnTheSameFormBothSubmit` both pin subtle UX/DoS-adjacent behaviors (guard runs before `s.spend`, nonce is reused not rotated) that would be easy to regress silently since neither breaks a naive happy-path test.
- Both `internal/web/AGENTS.md` and `deploy/AGENTS.md` were updated in the same change with content that accurately reflects the new code (verified by reading both the docs and the corresponding source), and the three touched specs (0008, 0013, 0019) each got a superseded/reversed/closed annotation pointing at 0019 rather than being silently left stale.

## Test coverage

Thorough and TDD-appropriate for a `configured` test signal. `internal/web/pretoken_test.go` and `pretoken_edges_test.go` cover essentially every acceptance criterion (AC-1 through AC-10) including negative paths, cross-tab reuse, and the leak boundary (extended in `leak_test.go` to include the nonce). `internal/config/controlplanepolicy_test.go` and `nodepolicy_test.go` pin the full YAML shape for both new manifests, including the “no ports clause means every port” and “no ipBlock anywhere” cases the spec calls out as the historical failure modes. The one class of criteria these tests structurally cannot cover — a missing peer, since a shorter list still parses as a valid policy — is explicitly acknowledged in both the spec and the test comments, and is covered instead by the live-cluster walk in `verify.md` (see the Minor finding above about its incomplete checkbox state). No untested new logic was found in the diff.
