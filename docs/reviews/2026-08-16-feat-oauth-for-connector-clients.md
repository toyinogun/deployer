# Review, feat/oauth-for-connector-clients, 2026-08-16

**Reviewed by**: Claude Sonnet 5 (author on unspecified model)
**Scope**: 35 files (+ 1 uncommitted test file), branch vs `main` (merge base `a4f06f82`)
**Verdict**: Approve

## Summary

This is spec 0024, an OAuth 2.1 authorization server bolted onto the console
for MCP connector clients (Claude desktop, claude.ai and anything else that
follows the MCP authorization spec). It is the highest risk feature in the
repo to date — open redirect, code replay and consent phishing all have a
real attack surface here, none of it caught by the fake clientset — and the
implementation holds up well against that risk. The redirect URI matching
(`identity.MatchRedirectURI`), the validation ordering in
`resolveAuthorize` (client and redirect first, everything else only once a
registered address is known), the atomic code-consume-then-mint transaction
in `store.GrantClientToken`, and the PKCE-decides-the-revoke logic in
`Service.Grant` were all traced line by line against the spec's acceptance
criteria and match exactly, including the subtle ordering the 2026-08-16
amendment (AC-16b) called out: the consumed branch answers before
`client_id`, `redirect_uri` and `resource` are ever compared, and only the
verifier decides whether a replay costs the client its token. The three new
tests in `internal/web/oauth_test.go` (uncommitted) directly pin that
ordering with a table of four ways a replay can be wrong while still proving
the verifier, plus the empty-verifier and repeated-retry cases. Test
coverage across all seven layers (`identity`, `store`, `web`, `httpapi`,
`mcp`, plus the generated sqlc code) is unusually thorough and AC-tagged
throughout. I found no correctness or security defects worth blocking on.

## Strengths

- `identity.Grant` (`internal/identity/oauthgrant.go:178`) implements AC-16b's
  replay rule exactly as specified: the consumed branch checks only the PKCE
  verifier before deciding whether to revoke, never the client id, redirect
  URI or resource, which is what stops a legitimate client's own retry (with
  a typo'd `client_id` it still holds from an earlier code, or any other
  incidental mismatch) from losing a token it was already issued.
- `store.GrantClientToken` (`internal/store/oauth.go:177`) holds the consume,
  the revoke of the client's prior token, the mint, and the `token_id` stamp
  in one `BEGIN IMMEDIATE` transaction, and a name collision (`ErrTokenNameTaken`)
  rolls the whole thing back rather than leaving the code half-consumed —
  which is exactly what lets `Grant`'s ordinal retry loop work correctly,
  since the code is still unconsumed on the next attempt.
- The open redirect defence (`resolveAuthorize` in `internal/web/oauth.go:201`)
  resolves the client and matches the redirect URI before anything else, and
  every other refusal from that point on is a redirect to the matched
  address rather than the requested one — an unknown client or unmatched URI
  can only ever render the no-op error page, never redirect.
- `refuseAuthorize` (`internal/web/oauth.go:304`) genuinely renders nothing
  the caller supplied, which is worth confirming by reading rather than
  taking on faith, and it does.
- Test coverage is excellent and unusually well organized: `internal/identity/oauth_test.go`
  covers the pure redirect/PKCE/name-cleaning logic table-first;
  `internal/store/oauth_test.go` drives the actual SQLite races (`TestTwoExchangesOfOneCodeMintExactlyOneToken`,
  `TestTheDatabaseRefusesTwoLiveTokensForOneClient`); `internal/web/oauth_test.go`
  drives the full HTTP flow including the AC-16b sequential-then-raced pair
  distinction the spec explicitly calls out as the case a build could get
  wrong while passing everything else.
- The uncommitted three tests in `internal/web/oauth_test.go` are a genuinely
  good addition: they specifically pin the ordering inside `Grant` (verifier
  decides, nothing else does) rather than just re-asserting the happy path,
  and the "repeated retries never cost the token" test directly encodes the
  regression the 2026-08-16 amendment fixed.
- Route registration for the new endpoints follows the existing opt-in,
  double-registration pattern exactly (`internal/web/web.go:206`,
  `internal/httpapi/httpapi.go:99`), and `TestAWrongMethodOnAnOAuthRouteIsRefused`
  and the deploy-host negative tests assert the cross-host absence in both
  directions.

## Minor

### 🟡 `ApproveClient` can leave an orphaned approved client with no code, `internal/identity/oauthgrant.go:130`
**Problem**: `ApproveClient` stamps `approved_at` and then writes the
authorization code as two separate store calls, not one transaction. If
`NewSecret` or `CreateOAuthCode` fails after the stamp lands, the client is
now permanently exempt from the sweep (AC-8 only removes rows with
`approved_at IS NULL`) but holds no code and nothing else will ever create
one for that approval attempt.
**Why it matters**: This is a low-probability internal fault path (random
generation failure or a write error), not attacker-reachable, and the code
comment (`internal/identity/oauthgrant.go:128`) shows this ordering is
deliberate — "a client that reaches the redirect is always one the sweep
will leave alone." The consequence is just a permanently un-swept, useless
`oauth_clients` row, not a security or correctness issue. Flagging so it's a
known, accepted tradeoff rather than something to trip over later.
**Suggested fix**: None required; worth a one-line note in the AGENTS.md
conventions if this package's file ever gets a written summary, so a future
reader doesn't mistake it for an oversight.

## Nits

- ⚪ `internal/web/oauth.go:407`, the connector bucket's 429 body uses
  `"slow_down"` as the error code, which isn't one of RFC 6749's core codes
  (closer to RFC 8628's device-flow vocabulary). AC-24 doesn't pin a specific
  code for this case, so it's not a spec violation, just a naming choice
  worth a second look if a real client ever parses it.
- ⚪ `internal/identity/oauth.go:106`, exceeding `MaxRedirectURIs` (10) answers
  `ErrRedirectURIInvalid` rather than `ErrClientMetadataInvalid`; AC-4a lists
  three specific cases for the metadata code and AC-5 covers "at least one,
  at most ten" together, so this reads as intentional, but the two ACs don't
  make it unambiguous which code a count violation should carry.

## Test coverage

Very thorough, AC-tagged throughout, and matches the project's stated
convention (pure logic test-first in `internal/identity`, HTTP/store wiring
tested after with a real SQLite file, no store mocking). Concurrency-
sensitive behaviour is driven as real races rather than asserted from
reasoning alone: `TestTwoExchangesOfOneCodeMintExactlyOneToken` and
`TestASecondGrantForOneClientLeavesExactlyOneLiveToken` in
`internal/store/oauth_test.go`, and `TestARacedPairMintsOnceAndLeavesThatTokenLive`
in `internal/web/oauth_test.go`. The uncommitted additions to
`internal/web/oauth_test.go` close the one gap the spec itself flagged as
easy to get wrong (AC-16b's ordering) with a five-case mutation-checked set
(per the scope.md changelog, each of the five was verified to fail when the
guard it protects is removed). The one thing no test in this repo can reach
— a real client's own metadata quirks and the tunnel's behaviour — is
explicitly out of scope for unit tests and was driven live against the
cluster per `verify.md`, with two deliberately undriven steps (two accounts
at once, and the clock moved forward a day) that don't leave any acceptance
criterion without evidence.
