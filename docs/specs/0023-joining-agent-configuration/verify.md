# Verify: joining · spec 0023 · updated 2026-08-16
_Steps derived from spec 0023 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

Most of what a unit test can hold is already held: `internal/web/connect_test.go` covers the
routing, the redirect, the stamp and its race, the four blocks, the placeholder, the naming
ordinal, the CSRF refusal, the audit row and the ordinary token, and every assertion in it was
mutation checked. What is below is the part the fake clientset, `httptest` and a fixed clock
cannot reach: a real client parsing a real block, a real hostname, and a real edge.

## UI / manual

- [x] Sign in as a verified account whose `connected_at` is null, carrying no `next` → lands on `/connect` → AC-3
- [x] Sign in again after that page has been served → lands on `/apps`, never `/connect` again → AC-4
- [x] Follow a session gate deep link (visit `/tokens` signed out, then sign in) → lands on `/tokens`, and the next plain sign in lands on `/connect` → AC-3
- [ ] Sign in as the bootstrap token only account → never sent to `/connect` → AC-5
- [x] `/connect` before pressing the button → every block shows `YOUR_TOKEN_HERE` and no token value → AC-12
- [x] Press the button → the raw token is in all four blocks; reload the page → the placeholder is back → AC-15, AC-12
- [x] Press each of the four tabs, then Copy on each → the clipboard holds that tab's block → AC-7, AC-13
- [x] Load `/connect` with JavaScript disabled → all four blocks render stacked and readable, tab strip hidden → AC-14
- [x] The console navigation carries `Connect your agent`, reachable without the one time redirect → AC-6
- [x] Both `/connect` routes answer on `DEPLOYER_CONSOLE_HOST` and the mint form posts there → AC-1, AC-2

## Commands

- [x] Paste the Claude Code line into a real Claude Code on a machine with no Tailscale, then drive a deploy to healthy → AC-8, AC-10, AC-15, AC-20
- [ ] Repeat with the Codex block (`~/.codex/config.toml`) and the Gemini CLI block (`~/.gemini/settings.json`) → AC-10
      _2026-08-16: not run in either client. Both blocks were read against current
      sources instead and neither needs a change: Codex still takes
      `[mcp_servers.<id>]` with `url` and `http_headers`, Gemini CLI still takes a
      top level `mcpServers` with `httpUrl` and `headers`. Each now documents an
      env var for the token rather than a literal, which this page cannot use
      because it hands over the value itself._
      _Only the endpoint inside these is pinned by a test. The format is not, and it is set by
      somebody else's release schedule, so this step is the only thing that catches a stale block._
- [x] `kubectl exec` the pod and confirm `accounts.connected_at` exists, is nullable and has no default → AC-24
- [x] Change `DEPLOYER_MCP_HOST`, restart, reload `/connect` → every block's endpoint follows, no stale hostname anywhere → AC-9
- [x] Mint twice from one tab on one real day → two live tokens named `<Client> <date>` and `<Client> <date> (2)` → AC-16, AC-16a, AC-23
- [x] Read the `token_mint` audit row for a mint made through the tunnel → `client_address` is the visitor's address from `CF-Connecting-IP`, not the tunnel's → AC-19
- [x] Revoke a token minted here from `/tokens`, then try a deploy with it → refused → AC-20, AC-21

## Acceptance-criteria coverage

- AC-1 · console host list in `consoleedge_test.go` + signed out gate test · manual on the real host
- AC-2 · console host list in `consoleedge_test.go` · manual
- AC-3, AC-3a · `TestAVerifiedPersonIsSentToConnectExactlyOnce`, `TestADeepLinkOutranksTheConnectRedirect` · manual
- AC-4 · `TestTheStampIsNotMovedBySecondVisit` · manual
- AC-4a · `TestTheStampCannotRace` (mutation checked against dropping the conditional)
- AC-5 · `TestAnAccountWithNoVerifiedAddressIsNeverRedirected` · manual on the bootstrap account
- AC-6 · manual only (a navigation link no test asserts)
- AC-7, AC-8 · `TestThePageRendersFourTabsWithClaudeCodeFirst` · manual for the real tab behaviour
- AC-9 · `TestEveryBlockCarriesTheConfiguredEndpoint` (configuration swap in process) · manual for a real host change
- AC-10 · `TestEveryBlockShowsAPlaceholderUntilOneIsMinted` for the header shape · manual for each client accepting it
- AC-11 · held by construction, the blocks are Go functions; asserted indirectly by AC-9's swap
- AC-12 · `TestEveryBlockShowsAPlaceholderUntilOneIsMinted`, plus the `/connect` entry in the leak crawl
- AC-13, AC-14 · markup asserted in `TestThePageRendersFourTabsWithClaudeCodeFirst` · behaviour is manual
- AC-15 · `TestMintingPutsTheRawTokenInEveryBlock`
- AC-16, AC-16a, AC-23 · `TestASecondMachineOnTheSameDayStillMints` (mutation checked against removing the ordinal)
- AC-17 · `TestAnUnknownClientFieldFallsBackToTheGenericTab`
- AC-18 · `TestAMintWithoutTheSessionTokenIsRefused`
- AC-19 · `TestMintingPutsTheRawTokenInEveryBlock` for the row · manual for the real address behind the tunnel
- AC-20, AC-21 · `TestATokenMintedHereIsAnOrdinaryToken` · manual through a real deploy
- AC-22 · `TestARefusedMintRerendersWithNoToken`
- AC-24 · exercised by every test in the package running the migration · manual against the live database
