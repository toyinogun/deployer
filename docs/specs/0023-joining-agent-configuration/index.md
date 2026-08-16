# 0023. Joining: the ready to paste agent configuration

**Date**: 2026-08-16
**Status**: Accepted

The decision record (context, options considered, rationale) is in [rationale.md](rationale.md).

## Summary

A person who has just verified their address still has to mint a token, work out where their coding agent keeps its configuration, and paste a password into a file by hand. This adds one page, `/connect`, that hands them a finished configuration block for their client with a freshly minted token already inside it, shown once. The first sign in after verifying lands there. The token is an ordinary token: it appears in the token list, it revokes there, and the page never shows it again on a later visit. One nullable column and one migration, no new configuration and no new credential path.

## Requirements

**User stories**:
- As someone who has just been invited to the platform, I want one block to copy into my coding agent, so that I do not have to learn where a token goes or what an MCP server entry looks like.
- As someone setting up a second machine, I want to come back to the same page and get a fresh block, so that I never have to reconstruct the configuration by hand.
- As the platform owner, I want a token handed out this way to be an ordinary token, so that there is one place credentials are listed and revoked and no second security model to reason about.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

Routing and entry

- **AC-1**: `GET /connect` renders the page for a signed in account. It is registered in the public route list, so it is registered twice, once bare and once under the console host pattern, per spec 0021's registration rule. A request with no session takes the existing session gate exactly as `/tokens` does.
- **AC-2**: `POST /connect` is registered the same way, method and all, so the mint form works on the console hostname rather than falling through to the catch all 404.
- **AC-3**: A successful browser sign in by an account whose `email_verified_at` is set and whose `connected_at` is null redirects to `/connect`, but only when the post carries no `next`. A `next` value still wins and takes its current `safeNext` path, so the session gate keeps returning a person to the page they were trying to reach. Every other successful sign in keeps its current destination unchanged.
- **AC-3a**: Both `email_verified_at` and `connected_at` reach the redirect decision as fields on the account the sign in already resolved. They are added to the account structs `internal/identity` and `internal/auth` carry and populated by the read `Login` already performs. The redirect issues no second query.
- **AC-4**: The first `GET /connect` an account is served stamps `connected_at` once. A later `GET /connect` leaves the existing stamp untouched, and the redirect in AC-3 never fires again for that account.
- **AC-4a**: The stamp is one conditional statement, `UPDATE accounts SET connected_at = ? WHERE id = ? AND connected_at IS NULL`, never a read followed by a write. Two concurrent `GET /connect` requests for one account leave exactly one stamp, and a test drives that race rather than assuming it.
- **AC-5**: An account with no email, such as the bootstrap token only account, never satisfies the AC-3 condition, because `email_verified_at` is null. It is never redirected.
- **AC-6**: The console navigation carries a `Connect your agent` link to `/connect`, visible to any signed in account, so the page is reachable deliberately and not only by the one time redirect.

The page and its blocks

- **AC-7**: The page renders four tabs, in this order: Claude Code, Codex, Gemini CLI, and a generic MCP JSON block. Exactly one is shown at a time, and Claude Code is the one selected on first load.
- **AC-8**: The Claude Code tab is a single `claude mcp add` command line, not a configuration file snippet.
- **AC-9**: Every block carries the deploy endpoint read from `Config.MCPURL` and never a hostname written into the block text. A test swaps the configuration value, constructs a fresh server from it and re renders, asserting every one of the four blocks follows. It is a configuration struct swap in process, the same shape as `internal/mcp/description_test.go` (spec 0022, AC-10), never a process rebuild.
- **AC-10**: Every block carries the bearer credential the deploy path authenticates with, in the form that client expects for an authorization header.
- **AC-11**: Every block's text is composed in Go from a per client definition held in `internal/web`. It is not assembled from template literals and it is not read from configuration. This feature adds no `DEPLOYER_*` variable.
- **AC-12**: Before any token has been minted in this response, and on every later visit, each block shows a visible placeholder where the token goes. No token value, past or present, appears.
- **AC-13**: Each tab has a copy button that copies that tab's block. Tabs and copy are driven by the existing `internal/web/static/app.js`.
- **AC-14**: With JavaScript unavailable, a `<noscript>` region renders all four blocks stacked and readable, each with its placeholder, so the page is never blank on the one thing it exists to hand over.

Minting

- **AC-15**: `POST /connect` mints a token for the signed in account and re renders `/connect` with the raw value substituted into every block. It follows `POST /tokens` exactly: no redirect, the raw value lives in that one response body and nowhere else, never in a URL, never in a log, never re rendered on a later request.
- **AC-16**: The token is minted with no expiry and a default name naming the client tab it was minted from plus the date, such as `Claude Code 2026-08-16`, so a person can tell two machines apart in the token list afterwards.
- **AC-16a**: `identity.MintToken` refuses a name an account already holds live, with `token_name_taken`. So the default name appends an incrementing ordinal when the dated name is already taken, and two mints from the same tab on the same day both succeed with distinct names. A test drives two mints in a row and asserts two live tokens rather than a refusal.
- **AC-17**: The client tab is submitted as a form field. An unknown or absent value falls back to the generic MCP JSON tab rather than refusing, so a stale or tampered field costs a tab selection and not a mint.
- **AC-18**: The mint requires the existing session CSRF check. A post with no token or a wrong one is refused by the same mechanism `/tokens` uses.
- **AC-19**: The mint writes one audit row using the existing `token_mint` action, carrying the account, the client address from `s.clientAddress(r)`, and the token id. No member is added to the closed audit action set.
- **AC-20**: A token minted from `/connect` is an ordinary row in `api_tokens`. It appears in the `/tokens` list, revokes there, and authenticates the deploy path identically to one minted from `/tokens`.
- **AC-21**: `/tokens` is unchanged by this feature. Its one time panel gains no client blocks and its mint path is not rerouted.
- **AC-22**: A refused mint re renders `/connect` with the refusal message and no token, and a fault internal to the platform takes the existing internal error path, both exactly as `/tokens` does.
- **AC-23**: There is no new cap and no new limiter on minting from this page. Pressing the button twice mints two ordinary tokens.

Schema

- **AC-24**: Migration `00006` adds `connected_at TEXT` to `accounts`, nullable with no default. It is purely additive, so the previous binary reads the schema unharmed and no existing row needs a value.

## Decision

**Chosen option**: Option 1: A `/connect` page that mints on a button press and renders client tabbed blocks.

A signed in person gets one page holding a finished configuration block per client, presses one button to mint the token that goes in it, and copies the result. The token is an ordinary `api_tokens` row and the page is an ordinary console page, so the feature adds a surface and not a credential path.

**Implementation skills**: `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`)

## Feature design

**Data model sketch**:

One nullable column. Nothing else in the schema moves.

| Entity | Change | Type | Null | Meaning |
|---|---|---|---|---|
| `accounts` | `connected_at` | `TEXT` (RFC3339) | yes | Null means this person has not yet been handed their agent configuration. Set once, on the first `GET /connect`, never cleared. |
| `api_tokens` | none | | | A token minted here is an ordinary row. Nothing marks where it came from, because nothing acts on that distinction. |

The column records a fact rather than inferring one. Deriving the same signal from token history (an account that has never held a token) needs no migration but fires again for anyone who revokes their last token, and couples a one time onboarding redirect to ongoing credential administration.

**State transitions**:

`accounts.connected_at`: `null` → stamped. One transition, one direction, triggered by the first `GET /connect` the account is served. There is no path that clears it.

The sign in destination reads that state, and a deep link outranks it:

| `next` on the post | `email_verified_at` | `connected_at` | Destination after a successful browser sign in |
|---|---|---|---|
| present | anything | anything | `safeNext(next)`, unchanged from today |
| absent | set | null | `/connect` |
| absent | set | stamped | unchanged from today |
| absent | null | anything | unchanged from today (an unverified account cannot sign in) |

A `next` only exists when the session gate put it there, meaning the person was already trying to reach a specific page. Sending them somewhere else instead would drop that intent with nothing to recover it from, and because they are then never stamped, the plain sign in that follows still takes them to `/connect`.

**API surface**:

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/connect` | GET | none | the four blocks, each with a placeholder | session cookie | the existing session gate redirect |
| `/connect` | POST | `client`:string (opt), CSRF token (req) | the four blocks, each carrying the raw token once | session cookie | 403 on a bad CSRF token, the refusal status from `identity.CodeOf` on a refused mint |

Both are in the public route list, so both are registered on the bare pattern and the console host pattern, method included (AC-1, AC-2).

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| `GET /connect` | the deploy endpoint in every block | `Config.MCPURL`, itself derived from `DEPLOYER_MCP_HOST` in `internal/config/edge.go`, which boot already requires, so it is never empty |
| `GET /connect` | which tabs exist, their order, their labels, their block text | the per client definition slice in `internal/web`, a Go value |
| `GET /connect` | the token placeholder shown in each block | a constant on that same client definition |
| `GET /connect` | which tab is selected on arrival | a constant, Claude Code (AC-7) |
| `GET /connect` | whether the stamp lands | not read at all. The conditional `UPDATE` in AC-4a decides it in the write itself, so nothing reads then writes |
| `POST /connect` | the raw token | the `MintToken` return value from `internal/identity`, held in that one response body |
| `POST /connect` | the token's name | the client definition's label, from the submitted `client` field matched against the slice, plus today's date from the service clock, plus an ordinal when that name is already live (AC-16, AC-16a). An unknown or absent `client` resolves to the generic tab (AC-17) |
| `POST /connect` | the audit row's client address | `s.clientAddress(r)`, spec 0021's single derivation, shared with every other console write |
| sign in | the redirect destination | `next` from the post first, then `email_verified_at` and `connected_at` as fields on the account `Login` already resolved, added to the account structs in `internal/identity` and `internal/auth` (AC-3, AC-3a) |

**Key invariants**:

- A raw token exists in exactly one response body and nowhere else. No path re renders one, none puts one in a URL, none logs one, none stores one. This is the rule `internal/web/tokens.go` already holds, and `/connect` inherits it rather than restating it in a second way.
- `connected_at` moves once and never back. Anything that would clear it would resurrect a one time redirect for someone who already dismissed it.
- Every block's endpoint comes from `Config.MCPURL`. A hostname written into block text is a second place a name lives, which is the exact drift spec 0022 removed when it deleted `DEPLOYER_PUBLIC_URL`.
- A token minted here is indistinguishable from any other token at every layer below the page. There is one token table, one list, one revoke path, one authenticator.

**Security model**:

- Both routes need a signed in session, checked by the same `s.session` gate every other console page uses. There is no account scoping to get wrong, because both act only on the session's own account.
- The mint is a state changing post, so it takes the existing session CSRF check (spec 0019's mechanism, the signed in half).
- A suspended account is already signed out by `internal/suspend`'s database first lockout, so neither route needs its own suspension check.
- The page carries a credential, so it inherits `/tokens`' whole discipline about the one response body. It adds no new storage, no new transport and no new lifetime.
- The raw token appears four times in that body rather than once, since every tab's block carries it. The one response body rule still holds literally, but the exposed surface really is wider: four places in the document for a screen capture, a browser extension reading the page, or someone looking over a shoulder to find it. That widening is accepted as the cost of tabs, and it is bounded by the fact that all four copies die with the response.
- No new configuration, so no new secret and no new startup validation.
- No regulated data. The page shows a platform issued credential to the person it belongs to.

**Configuration required**: none. The feature reads `Config.MCPURL`, which boot already requires.

**Critical test scenarios**:

- Happy path: a verified account with a null `connected_at` signs in, lands on `/connect`, posts the mint with the Claude Code tab selected, and the rendered command line carries both the configured endpoint and the raw token, verifies **AC-3**, **AC-8**, **AC-9**, **AC-15**.
- Configuration follows: change `DEPLOYER_MCP_HOST`, rebuild, re render, and assert all four blocks carry the new endpoint, verifies **AC-9**.
- Shown once: mint, then `GET /connect` again in the same session, and assert the response body contains the placeholder and no token value, verifies **AC-12**.
- One shot: `GET /connect` twice, assert the stamp is unchanged by the second, then sign in again and assert no redirect, verifies **AC-4**.
- Concurrency: two `GET /connect` requests for one account driven at once leave exactly one stamp, verifies **AC-4a**.
- Second machine: two mints from the Claude Code tab on the same day both succeed and produce two live tokens with distinct names, rather than the second being refused `token_name_taken`, verifies **AC-16a**, **AC-23**.
- Deep link preserved: a sign in carrying a `next` lands on that page and not on `/connect`, and the plain sign in after it does land on `/connect`, verifies **AC-3**.
- Failure case: a mint refused by `identity` re renders the page with the message, no token in the body, verifies **AC-22**.
- Auth and permission: `POST /connect` with a missing CSRF token is refused, and `GET /connect` with no session takes the session gate, verifies **AC-1**, **AC-18**.
- Console host: both `GET /connect` and `POST /connect` answer on the console host pattern rather than the catch all 404, verifies **AC-1**, **AC-2**.

## Build plan

Ordered by the project's Tracer Bullet approach: the first slice carries one real person from a verified sign in to a real deploy driven by a block they pasted, through every layer this feature touches. Nothing is thickened until that thread is proved.

1. Migration `00006` adding `accounts.connected_at`, the conditional stamp statement, and both `connected_at` and `email_verified_at` threaded onto the account structs in `internal/identity` and `internal/auth` off the read `Login` already does, satisfies **AC-3a**, **AC-4a**, **AC-24**.
2. The thin thread, end to end: `GET /connect` and `POST /connect` in the public route list, the sign in redirect on the verified and unstamped condition with `next` still winning, the stamp on first serve, and one client definition (Claude Code) whose block carries `Config.MCPURL` and a really minted token. Prove it by pasting the rendered command into a real client and driving a deploy to healthy, satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-5**, **AC-8**, **AC-15**.
3. The naming rule before a second mint can hit it: the dated default name with its ordinal fallback, and two mints from one tab on one day both landing, satisfies **AC-16**, **AC-16a**, **AC-23**.
4. The three remaining client definitions, the default selected tab and the placeholder rendering, plus the configuration swap test asserting every block's endpoint follows, satisfies **AC-7**, **AC-9**, **AC-10**, **AC-11**, **AC-12**.
5. The credential discipline and the refusals: the CSRF check, the `token_mint` audit row with its client address, the tab field fallback, the refusal and internal fault paths, and the leak crawl extended over the new page, satisfies **AC-17**, **AC-18**, **AC-19**, **AC-22**.
6. The surface: the tab and copy behaviour in `app.js`, the `<noscript>` stacked fallback, and the navigation link, satisfies **AC-6**, **AC-13**, **AC-14**.
7. The proof that nothing else moved: a token minted here listed and revoked on `/tokens`, the same token authenticating the deploy path, and `/tokens` itself unchanged, satisfies **AC-20**, **AC-21**.

## Consequences

**Positive**:

- The step the scope names as the one that actually goes wrong, a person handling a password by hand, becomes a copy button. Nothing about the credential itself changes, so nothing new can go wrong with it.
- The page is useful past onboarding. Someone setting up a second machine gets the same block instead of reconstructing it, which is the case that would otherwise send them to guess at a config format.
- Joining costs one migration and one page. There is no invite link that mints, no new token kind, no unauthenticated credential path, so the security model in specs 0007 and 0015 is untouched.
- The endpoint in every block is derived, so a hostname change moves one configured value and the blocks follow, with a test that fails if they do not.

**Negative / tradeoffs**:

- The platform now carries three client configuration formats it does not own. Each is set by someone else's release schedule and will go stale silently: the page will keep rendering a confidently wrong command long after the client changed it. The pinning test in AC-9 catches an endpoint that drifts, not a format that does. This is the real ongoing cost of choosing tabs over one generic block, and it is a maintenance commitment rather than a one time build.
- `GET /connect` writes. A read route with a side effect is a thing to remember, and it means a prefetch or a link preview can stamp `connected_at` and quietly consume the one time redirect. The write is an idempotent marker rather than a credential, which is why it is acceptable here and why minting stayed a post.
- Tabs and copy are JavaScript, so the ordinary experience of the page depends on it. The `<noscript>` fallback keeps the page useful but not pleasant.
- Someone who lands, looks and leaves is never sent back. They have to find the nav link, which is the cost of not nagging.

**Neutral**:

- Every account that already exists has a null `connected_at` and a set `email_verified_at`, so on the day this ships each of them is sent to `/connect` once on their next plain sign in, months into using the platform. That is expected rather than a defect, since the page is a useful configuration reference and the detour happens exactly once, but it is a visible change for existing people and worth knowing about before it lands rather than after.
- One migration, purely additive, so a rollback to the previous binary is safe without a down migration being exercised.
- No new `DEPLOYER_*` variable, so `deploy/` and the startup validation in `internal/config` are untouched.
- The closed audit action set does not grow, which also means the audit trail cannot tell a joining mint from any other. If measuring how well joining works becomes a question, that is a later change rather than something this spec leaves half done.

## Follow-up

- [ ] The three client configuration formats have no drift check. Worth deciding later whether a periodic manual review, a documented version each format was correct for, or a link to each client's documentation beside its tab is the cheapest way to notice a stale block.
- [ ] The scope's own feature 23 note is worth revisiting once this ships: the one click version that provisions the client from an invite link, with no sign in, is still unaddressed and is still a new credential path with its own security model.
