# Review, feat/joining-agent-configuration, 2026-08-16

**Reviewed by**: Claude Sonnet 5 (author on unspecified model)
**Scope**: 26 files, branch vs main (merge base `1b9312b`)
**Verdict**: Approve with nits

## Summary

Spec 0023's `/connect` page: one nullable column (`connected_at`), a conditional stamp statement, a one-time post-signin redirect that a `next` deep link still outranks, and a four-tab page that mints an ordinary API token and renders it into per-client blocks. The implementation matches the spec closely and the layering is respected: the stamp is a single `UPDATE ... WHERE connected_at IS NULL` proven race-free by a concurrent test, the raw token lives in exactly one response body, the endpoint is derived from `Config.MCPURL` and pinned by a configuration-swap test, and a minted token is a genuinely ordinary `api_tokens` row (proven by minting, authenticating, listing, and revoking it through the existing `/tokens` machinery in one test). Test coverage is unusually thorough and each test's doc comment ties back to specific ACs. The only real issues are a small piece of redundant logic in `afterSignIn` and one acceptance criterion (AC-6, the nav link) that has no automated assertion, matching what `verify.md` itself already discloses.

## Minor

### 🟡 `afterSignIn` computes `safeNext(next)` on both the early-return path and the fallback path, `internal/web/identity_pages.go:118-128`
**Problem**: The function is:
```go
func afterSignIn(next string, account identity.Account) string {
	if next != "" {
		return safeNext(next)
	}
	if account.Verified && !account.Connected {
		return "/connect"
	}
	return safeNext(next)
}
```
By the time execution reaches the final `return safeNext(next)`, `next` is guaranteed to be `""` (the first branch already handled every non-empty case), so this is always `safeNext("")`, which is always `"/apps"`.
**Why it matters**: Not a bug today — `safeNext("")` does return `/apps` — but it reads as if `next` might still matter at that point, which it never can. A future edit to `safeNext`'s empty-string behavior, or a refactor that moves the branches around, could silently change this call's meaning without anyone noticing the dead condition.
**Suggested fix**: Replace the final line with `return "/apps"` (or a named constant) to make it clear the deep-link case is fully exhausted, or restructure as `next := safeNext(next)` once at the top and branch on that.

### 🟡 AC-6 (the nav link) has no automated assertion, `internal/web/templates/base.html:21`
**Problem**: The `Connect your agent` nav entry is added to `base.html` and wired via `s.render(..., "connect", ...)`, but no test asserts the link's presence, its `href`, or its `aria-current` behavior on `/connect` itself. `verify.md`'s own AC-6 coverage line reads "manual only (a navigation link no test asserts)."
**Why it matters**: Test signal for this project is `configured`, and the guide calls new/changed logic with no covering test at least a Minor. The risk here is low (a template line, not branching logic) and the gap is honestly disclosed in verify.md rather than hidden, which is why this stays Minor rather than Major.
**Suggested fix**: A one-line addition to an existing rendering test (e.g. asserting `href="/connect"` and `aria-current` toggling on `GET /connect` vs another page) would close this cheaply, consistent with how other nav entries are likely already exercised.

## Nits

- ⚪ `internal/web/connect.go:222`, `mintForClient` calls `s.svc.Now()` once per attempt would be avoidable duplication if the loop retried more than the (currently unreachable in practice) `nameOrdinalLimit` times, but as written `base` is computed once outside the loop, so this is fine — noted only because the loop re-derives `name` each iteration from `base`, which is correct but easy to misread as re-fetching the clock. No change needed.
- ⚪ `internal/web/connect.go:114-116`, `genericClient` is derived as `connectClients[len(connectClients)-1]`, which is correct today (the MCP JSON tab is last) but silently changes meaning if someone reorders `connectClients` without noticing this dependency. A short comment already exists nearby; consider naming the generic client by `Key` lookup instead of positional index for extra safety, author's discretion.

## Strengths

- The concurrency claim in AC-4a is actually tested, not just asserted: `TestTheStampCannotRace` drives 8 concurrent calls to `MarkAccountConnected` directly against the store and asserts exactly one reports `did == true`, then repeats the shape through the HTTP route with two concurrent `GET /connect` calls. This is exactly the kind of race a lesser test suite would only claim in a comment.
- `TestATokenMintedHereIsAnOrdinaryToken` is a genuinely end-to-end proof that the feature adds no second credential model: it mints via `/connect`, authenticates the raw value through `auth.Authenticate` (the same path the deploy pipeline uses), confirms it appears on `/tokens`, revokes it there, and confirms the same raw value stops authenticating afterward.
- The credential-discipline conventions from `/tokens` (`internal/web/AGENTS.md`'s "one response body" rule) are followed to the letter rather than restated in a parallel way, and `leak_test.go` and `consoleedge_test.go` were both extended rather than given parallel new tests, keeping the crawl coverage centralized.
- The ordinal retry in `mintForClient` narrowly targets only `CodeTokenNameTaken` and re-raises everything else immediately (`internal/web/connect.go:234`), so a real refusal (e.g. an unverified account, in principle) is never masked as a naming collision.
- The migration (`00006_joining.sql`) and the store convention doc (`internal/store/AGENTS.md`) were updated together, and the doc's language about "a caller treating zero rows as a failure has the shape backwards" is a genuinely useful guardrail for the next person who touches this pattern.

## Test coverage

Coverage is comprehensive and each test is traceable to specific ACs via doc comments: routing/redirect (AC-1 through AC-5), the stamp and its race (AC-4, AC-4a), all four blocks and their ordering/placeholder/endpoint (AC-7 through AC-14), minting and audit (AC-15, AC-19), the naming ordinal (AC-16, AC-16a, AC-23), the unknown-client fallback (AC-17), CSRF (AC-18), the ordinary-token proof (AC-20, AC-21), and the refusal path (AC-22). The two gaps are both already known to the authors: AC-6 (nav link, no automated test, called out above) and the Codex/Gemini CLI blocks were verified by reading current client docs rather than by driving a real client (disclosed in `verify.md`, accepted as an ongoing cost in the spec's own Consequences section). Neither gap blocks merge.
