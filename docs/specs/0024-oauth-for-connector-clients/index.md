# 0024. OAuth for clients that will not hold a token

**Date**: 2026-08-16
**Status**: Proposed

The decision record (context, options considered, rationale) is in [rationale.md](rationale.md).

## Summary

Every way of connecting to this platform today ends in a bearer token pasted into a file. The Claude desktop app, claude.ai and every other connector surface offer no place to paste one: they take a URL and expect the server to tell them how to sign in. This makes deployer its own authorization server, so a person pastes the deploy address, gets sent to the console, approves the request, and the client is issued a token. The token it receives is an ordinary token, so it is listed and revoked on `/tokens` like every other credential, and the paste and go blocks the command line clients use are untouched.

## Requirements

**User stories**:
- As someone using the Claude desktop app, I want to add deployer by pasting its address and signing in, so that I never have to find a configuration file or handle a password.
- As someone using any other connector that follows the MCP authorization specification, I want the same thing to work without the platform having been written for my client specifically.
- As the platform owner, I want a connector's access to be an ordinary token, so that there is still one place credentials are listed and revoked and no second way for a request to become an account.
- As the platform owner, I want a stranger who registers a client to gain nothing until a signed in person approves them, and I want the approval page to show me what the platform can actually verify rather than what the client claims about itself.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

Discovery

- **AC-1**: The deploy host serves `GET /.well-known/oauth-protected-resource` and `GET /.well-known/oauth-protected-resource/mcp`. Each answers RFC 9728 JSON whose `resource` is the exact address that path describes, `https://<MCPHost>` and `https://<MCPHost>/mcp` respectively, because Claude requires `resource` to match what the person typed. Both carry `authorization_servers` holding exactly one entry, `Config.ConsoleURL`, plus `scopes_supported: ["deploy"]` and `bearer_methods_supported: ["header"]`. Neither needs a credential.
- **AC-2**: A request to `/mcp` with a missing or invalid bearer token answers `401` with `WWW-Authenticate: Bearer resource_metadata="https://<MCPHost>/.well-known/oauth-protected-resource/mcp", scope="deploy"`. The existing JSON body and the existing audit row are unchanged. The header is only on the `401`, because Claude ignores it on a `200`.
- **AC-2a**: The two refusals that are not about the credential keep their current shape and gain no `WWW-Authenticate` header: the limiter's `429` with `too_many_attempts`, and a suspended account, which still reaches the protocol layer and answers `account_suspended` as a tool result.
- **AC-3**: The console host serves `GET /.well-known/oauth-authorization-server`, answering RFC 8414 JSON with `issuer` equal to `Config.ConsoleURL`, the three endpoint URLs below it, `response_types_supported: ["code"]`, `grant_types_supported: ["authorization_code"]`, `code_challenge_methods_supported: ["S256"]`, `token_endpoint_auth_methods_supported: ["none"]`, `scopes_supported: ["deploy"]` and `authorization_response_iss_parameter_supported: true`.
- **AC-3a**: `scopes_supported` does not contain `offline_access`, because Claude appends it when advertised in order to obtain a refresh token, and this platform issues none. `client_id_metadata_document_supported` is absent, so a client falls back to registration.

Registration

- **AC-4**: `POST /register` on the console host takes an RFC 7591 `application/json` body, needs no credential, and answers `201` with `client_id`, `client_id_issued_at`, `token_endpoint_auth_method: "none"` and the accepted `client_name` and `redirect_uris` echoed back. `client_id_issued_at` is Unix seconds, a JSON number, per RFC 7591. No client secret is ever issued, because every client here is public and PKCE is what binds the exchange.
- **AC-4a**: A body that does not parse, one carrying no `redirect_uris` or an empty array, or a `client_name` longer than the bound in AC-20a, refuses with `invalid_client_metadata`. That code exists for exactly those three cases and a bad redirect URI takes AC-5's code instead, so the two never overlap.
- **AC-5**: A `redirect_uris` entry is accepted only when it is an absolute URI with no fragment that is either `https`, or `http` with a host of exactly `localhost`, `127.0.0.1` or `[::1]`. RFC 8252 covers both loopback literals, so excluding the IPv6 one would refuse a conforming client for no reason. Anything else refuses the whole registration with `invalid_redirect_uri`. At least one entry is required and at most ten are accepted.
- **AC-6**: Registration spends a limiter bucket of its own, a third `identity.Settings` value beside `SignInSettings` and `DeployPathSettings`, using the shared address derivation. It must not be the sign in bucket: one connector being added spends it three times in a row from one address, so sharing it would let a person adding a connector lock themselves out of the console they are signing in to. Over the bucket answers `429`.
- **AC-7**: Registration writes no audit row. It is an unauthenticated public write and the audit table is not a place strangers get to fill.
- **AC-8**: The daily sweep deletes an `oauth_clients` row whose `approved_at` is null and whose `created_at` is older than 7 days. A row with `approved_at` set is never deleted by the sweep. The window is a Go constant in the package that owns it, not a `DEPLOYER_*` value, because it is a product decision about how long a half finished connection stays resumable. Seven days rather than a day removes the case where a person leaves the approval page open, presses the button, and gets AC-10's unknown client error, which is indistinguishable from a spoofed client.
- **AC-8a**: The sweep runs on the existing daily runner that already sweeps expired uploads and nulls old audit addresses, not on a ticker of its own. A second scheduler for one delete is a second thing that can silently stop.

Authorize, the machine half

- **AC-9**: `GET /authorize` on the console host takes `client_id`, `redirect_uri`, `response_type`, `code_challenge`, `code_challenge_method`, `resource`, and optionally `state` and `scope`. A request carrying no session takes the existing session gate.
- **AC-9a**: Signing in from that gate returns the person to the same authorize URL with its query string intact. `safeNext` is exercised with a query string carrying an encoded `redirect_uri` and a `state`, and a test drives the whole round trip rather than asserting the helper alone.
- **AC-10**: Validation order is load bearing. An unknown `client_id`, or a `redirect_uri` that matches none registered for that client, renders an error page on the console and **never redirects**. Every other invalid parameter redirects to the matched `redirect_uri` carrying `error`, `error_description`, the `state` if one was given, and `iss`. That order is what stops this endpoint being an open redirect.
- **AC-10a**: A registered `https` redirect URI matches the request exactly. A registered loopback URI matches with the port ignored, so `http://localhost/callback` matches `http://localhost:3118/callback`, while scheme, host and path must be equal. Claude Code declares both `http://localhost/callback` and `http://127.0.0.1/callback`, so both forms are accepted, as is the `[::1]` form from AC-5.
- **AC-10b**: What "exactly" means is pinned rather than left to the build, because this comparison is the whole open redirect defence and the two sides of it arrive differently. The request's value is the already percent decoded query parameter as `net/url` yields it. The stored value is the string the registration supplied, kept verbatim. They are compared with plain string equality and **no** normalization: no case folding of scheme or host, no trailing slash handling, no default port elision, no re encoding. The single exception is the loopback port rule in AC-10a. A test drives a registered URI against its percent encoded, its uppercased host, and its trailing slash variants, and all three are refused.
- **AC-10c**: The error page AC-10 renders names the platform, says the request could not be matched to a registered client, and offers a link to the console. It renders **nothing the caller supplied**: not the client id, not the requested redirect URI, not the client name. There is therefore nothing on it to escape, which is the point.
- **AC-11**: `response_type` must be `code` and `code_challenge_method` must be `S256`. An absent challenge, or a method of `plain`, redirects with `invalid_request`. There is no path through this endpoint without PKCE.
- **AC-12**: `resource` must equal `https://<MCPHost>/mcp` or `https://<MCPHost>`. Another value redirects with `invalid_target`, and an absent one with `invalid_request`. The value is stored on the code and compared again at the token endpoint, so a token is bound to the resource it was asked for.

Authorize, the person's half

- **AC-13**: The approval page shows, in this order: the redirect URI's host as the heading, because it is the only fact the platform can verify; the client's `client_name` quoted, HTML escaped, and labelled as something the client said about itself; the account being connected; one plain sentence saying the connector will be able to deploy, configure, roll back, read logs and delete apps for that account; and an Approve and a Deny control. It takes the existing session CSRF check exactly as `/tokens` does.
- **AC-13a**: When every redirect URI registered for that client is a loopback address, the page carries an additional warning, worded to say that this request came from a program running on your own machine, that the platform cannot tell which program, and that you should only approve it if you just started this yourself. The MCP specification requires the host to be shown and recommends this warning. The sentence is pinned here for the same reason AC-13 pins the ordering: it is the only thing standing between a person and a local process impersonating their editor.
- **AC-14**: A suspended account never reaches the approval page. The refusal reads as a suspension rather than as a fault, and it lands before any row is written.
- **AC-15**: Deny redirects to the matched redirect URI with `error=access_denied`, the `state` if one was given, and `iss`. Nothing is stamped and no code is written.
- **AC-16**: Approve stamps `approved_at` on the client if it is null, writes one `oauth_codes` row holding the hashed code, and redirects with `code`, the `state` if one was given, and `iss` equal to `Config.ConsoleURL`.
- **AC-16a**: A code expires 60 seconds after it is written and is single use. A second presentation of a consumed code is refused, and it also revokes the token that code issued, which is what OAuth 2.1 asks for on a replay.

Token

- **AC-17**: `POST /token` on the console host accepts `application/x-www-form-urlencoded` and nothing else, because Claude sends that content type and a JSON only parser answers `415`, which reads as an outage. A body of another type answers `400 invalid_request`. Fields: `grant_type=authorization_code`, `code`, `redirect_uri`, `client_id`, `code_verifier`.
- **AC-18**: The exchange succeeds only when all of these hold: the code exists, is unconsumed and unexpired; it was issued to this `client_id`; the `redirect_uri` equals the one stored on it under AC-10b's comparison rule; the `resource`, when the token request carries one, equals the one stored on the code, and an absent one takes the stored value; and the S256 hash of `code_verifier` equals the stored `code_challenge`. Any failure answers `400` with `invalid_grant`, and the response never says which check failed.
- **AC-18a**: Consuming the code is one conditional statement, `UPDATE oauth_codes SET consumed_at = ? WHERE code_hash = ? AND consumed_at IS NULL`, and only a row count of one proceeds to mint. It is never a read followed by a write, because two token requests arriving together would both read the code as unconsumed and both mint from it. A test drives that race rather than assuming it, the same way spec 0023 drove the `connected_at` stamp.
- **AC-19**: On success the platform revokes any live token this account already holds for this client and mints a new one carrying `oauth_client_id`. The response is `{"access_token": ..., "token_type": "Bearer", "scope": "deploy"}` with no `expires_in` and no `refresh_token`, sent with `Cache-Control: no-store`.
- **AC-19a**: The revoke and the mint are one database transaction, which `identity.Service` cannot express today because it calls store methods one at a time. So the transaction lives in one new store method that revokes and inserts together, reached through one new `identity.Service` method beside `MintToken`. The two service methods share an unexported helper holding the rules they both owe: the verified account check, the token generation, the hash and the prefix. Duplicating those rules in a second mint path rather than sharing them is the failure this criterion exists to prevent, because a rule that lives twice is a rule that will hold in one place.
- **AC-19b**: The partial unique index therefore never sees two live tokens for one client, and a mint that fails never leaves the person with the old token already revoked. A test drives two grants for one client concurrently and asserts exactly one live token.
- **AC-20**: A token issued this way is an ordinary `api_tokens` row. It appears in the `/tokens` list, revokes there, authenticates `/mcp` and `POST /v1/uploads` identically to a pasted one, and is refused identically once revoked.
- **AC-20a**: Its name is derived from the client's `client_name`, truncated to a bounded length, stripped of control characters, plus today's date, with the incrementing ordinal fallback spec 0023 built for a name already live. The name is attacker supplied text that renders in the token list, so the page escapes it and a test drives a hostile name end to end.
- **AC-21**: The token endpoint authenticates no client. `client_id` travels in the body and PKCE binds the exchange, which is what `token_endpoint_auth_methods_supported: ["none"]` in AC-3 promises.
- **AC-22**: The token endpoint spends the same connector bucket registration spends, from AC-6, and not the sign in bucket.
- **AC-23**: A successful exchange writes one audit row using a new `connector_grant` action, carrying the account and the client address from the shared derivation. `auth.Audit` holds one target pair, so the token id is the target (`TargetType` `api_token`, `TargetID` the token id) and the client id travels in `Reason`. No field is added to `Audit`, and `connector_grant` is the only member the closed audit action set gains.

Boundaries

- **AC-24**: `/register`, `/authorize` and `/token` answer OAuth error codes, in the RFC 6749 JSON shape or as redirect parameters, never a `internal/domain` reason code. `internal/domain/reason.go` gains no member, and the MCP tools' reason codes are unchanged. This is a written exception to the closed reason code rule and it covers exactly those three routes, named here so the exception is a list rather than a principle. A later route claiming it needs its own criterion saying so, because "some routes answer OAuth codes" is how a closed set stops being closed.
- **AC-25**: Route registration is deliberate on both hostnames. The deploy host pattern gains exactly the two documents in AC-1. The console host pattern gains exactly `GET /.well-known/oauth-authorization-server`, `POST /register`, `GET /authorize`, `POST /authorize` and `POST /token`. Neither catch all is loosened.
- **AC-25b**: Every one of those seven registrations names its method, so the standard mux answers `405` for a wrong method on a path that exists and the host's catch all answers `404` for a path that does not. Both are asserted, because "registered deliberately" is only checkable if the two refusals are distinguishable.
- **AC-25a**: A test asserts the negative in both directions: none of the five console routes answers on the deploy host, and neither protected resource document answers on the console host.
- **AC-26**: `/connect` gains a fifth tab, after the existing four, for the Claude app and other connectors. It shows exactly `Config.MCPURL + "/mcp"` and nothing else: no token, no placeholder, and no mint button while it is selected, because this flow issues its own credential. The endpoint follows a configuration swap the way the other four blocks do, asserted by the same style of test.
- **AC-27**: The `<noscript>` region renders the new tab's content alongside the other four.
- **AC-28**: None of the five endpoints performs work that can take seconds. Claude abandons discovery, registration and token requests after 10 seconds and a refresh after 30. Every one of them is a small number of local SQLite statements.
- **AC-29**: Migration `00007` adds `oauth_clients`, `oauth_codes`, the nullable `api_tokens.oauth_client_id` column and the partial unique index in the design below. It is purely additive, so the previous binary reads the schema unharmed.
- **AC-30**: This feature adds no `DEPLOYER_*` variable. Every address it needs is derived from `Config.MCPURL` and `Config.ConsoleURL`, which boot already requires.

## Decision

**Chosen option**: Option 1: Deployer becomes its own authorization server.

The platform serves the MCP authorization discovery documents, registers clients dynamically, approves them on a console page behind the existing session, and issues an ordinary `api_tokens` row as the access token.

**Implementation skills**: `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `database-migrations` (`~/.claude/skills/database-migrations/`)

## Rationale

See [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

| Entity | Field | Type | Null | Meaning |
|---|---|---|---|---|
| `oauth_clients` (new) | `id` PK | TEXT | no | The `client_id` issued at registration |
| | `name` | TEXT | no | What the client called itself. Never trusted, escaped wherever shown |
| | `redirect_uris` | TEXT | no | JSON array, validated at registration (AC-5) |
| | `created_at` | TEXT | no | RFC3339 |
| | `approved_at` | TEXT | yes | Null until some account approves. Null and old is what the sweep deletes (AC-8) |
| `oauth_codes` (new) | `code_hash` PK | TEXT | no | Hashed the way `api_tokens.token_hash` is. The raw code exists only in the redirect |
| | `client_id` FK | TEXT | no | → `oauth_clients.id`, `ON DELETE CASCADE` |
| | `account_id` FK | TEXT | no | → `accounts.id`, `ON DELETE RESTRICT`, matching `api_tokens` |
| | `redirect_uri` | TEXT | no | Compared again at the token endpoint (AC-18) |
| | `code_challenge` | TEXT | no | S256 only |
| | `resource` | TEXT | no | The RFC 8707 value the authorize request carried |
| | `token_id` | TEXT | yes | The token this code issued, so a replay can revoke it (AC-16a) |
| | `expires_at` | TEXT | no | 60 seconds after `created_at` |
| | `consumed_at` | TEXT | yes | Single use |
| | `created_at` | TEXT | no | |
| `api_tokens` | `oauth_client_id` | TEXT | yes | → `oauth_clients.id`. Null for every token minted by hand, which is every token today |

Two indexes carry rules rather than speed:

- `CREATE UNIQUE INDEX api_tokens_live_client ON api_tokens(account_id, oauth_client_id) WHERE revoked_at IS NULL AND oauth_client_id IS NOT NULL` makes one live token per client per account something the database refuses to break.
- `CREATE INDEX oauth_codes_expiry ON oauth_codes(expires_at)`, so the sweep of dead codes is a range scan.

The existing `api_tokens_live_name` index still applies, which is why AC-20a needs the ordinal fallback.

**State transitions**:

`oauth_clients.approved_at`: `null` → stamped, once, on the first Approve by any account. Nothing clears it. A row that never leaves `null` is swept.

`oauth_codes`: `issued` → `consumed` (a successful exchange) or `expired` (60 seconds pass). One direction only. A presentation of a consumed code is a refusal and revokes `token_id`.

`api_tokens` for a client: at most one live row per `(account_id, oauth_client_id)`. A new grant revokes the previous one in the same transaction.

**API surface**:

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/.well-known/oauth-protected-resource` and `/.well-known/oauth-protected-resource/mcp` (deploy host) | GET | none | `resource`, `authorization_servers`, `scopes_supported` | none | none |
| `/.well-known/oauth-authorization-server` (console host) | GET | none | RFC 8414 metadata | none | none |
| `/register` (console host) | POST | `client_name`:string, `redirect_uris`:array (req) | `client_id`, `client_id_issued_at` | none | `400 invalid_redirect_uri`, `400 invalid_client_metadata`, `429` |
| `/authorize` (console host) | GET | `client_id`, `redirect_uri`, `response_type`, `code_challenge`, `code_challenge_method`, `resource` (req), `state`, `scope` (opt) | the approval page | session cookie | error page on an unknown client or unmatched redirect, otherwise a redirect carrying `invalid_request`, `invalid_target` or `unsupported_response_type`; the session gate; a suspension refusal |
| `/authorize` (console host) | POST | the same parameters, `approve`:string, CSRF token (req) | a redirect carrying `code` or `error=access_denied` | session cookie | `403` on a bad CSRF token |
| `/token` (console host) | POST, form encoded | `grant_type`, `code`, `redirect_uri`, `client_id`, `code_verifier` (req) | `access_token`, `token_type`, `scope` | none | `400 invalid_grant`, `400 invalid_request`, `400 unsupported_grant_type`, `429` |
| `/mcp` (deploy host) | POST, GET | unchanged | unchanged | bearer | unchanged, plus the `WWW-Authenticate` header on `401` |

**Value sourcing**:

| Action | Value produced or displayed | Source |
|---|---|---|
| protected resource document | `resource` | the request path plus `Config.MCPURL`, one document per path so each is exact (AC-1) |
| protected resource document | `authorization_servers` | `Config.ConsoleURL`, a single entry, because Claude uses the first and does not fall back |
| `401` on `/mcp` | `resource_metadata` in the header | `Config.MCPURL` plus the path aware well known path |
| authorization server document | `issuer` and the three endpoint URLs | `Config.ConsoleURL` plus constant paths |
| `/register` | `client_id` | a fresh platform id, the same generator every other id uses |
| `/register` | `client_id_issued_at` | the service clock as Unix seconds, a JSON number per RFC 7591 (AC-4) |
| `/register` | the accepted redirect URIs | the request body, after the AC-5 validation, stored verbatim as JSON so AC-10b can compare against the exact registered string |
| `/authorize` | the host shown as the heading | parsed from the **matched registered** redirect URI, never from the request parameter |
| `/authorize` | the client name shown as a quote | `oauth_clients.name`, escaped at render |
| `/authorize` | whether the loopback warning shows | every registered redirect URI for that client being loopback (AC-13a) |
| `/authorize` | the account being connected | the resolved session, never anything the caller sent |
| `/authorize` | the raw authorization code | freshly generated, hashed for storage, present only in the redirect |
| `/authorize` | `iss` on every redirect | `Config.ConsoleURL` |
| `/token` | the raw access token | the new `identity.Service` grant method in AC-19a, sharing its generation helper with `MintToken`, held in that one response body |
| `/token` | the token's name | `oauth_clients.name` bounded and cleaned, plus today's date from the service clock, plus an ordinal when taken (AC-20a) |
| `/token` | the audit row's target and reason | the token id as the target, the client id in `Reason` (AC-23) |
| `/token` and `/register` | the audit row's and the limiter's client address | the shared `clientAddress` derivation, as every console write uses |
| `/register`, `/authorize`, `/token` | which limiter bucket is spent | the new connector `identity.Settings`, never `SignInSettings` (AC-6) |
| sweep | which client rows die | `approved_at IS NULL AND created_at < now - 7 days`, the window a Go constant (AC-8) |
| `/connect` fifth tab | the address shown | `Config.MCPURL + "/mcp"` (AC-26) |

**Key invariants**:

- A redirect only ever goes to a URI already registered for that `client_id`, compared with no normalization (AC-10, AC-10b), which is the whole open redirect defence.
- An authorization code is exchanged at most once, enforced by a conditional update rather than by a read (AC-18a), and a second attempt costs the token the first one issued (AC-16a).
- No bucket this feature spends is the sign in bucket, so no amount of connecting can lock a person out of the console (AC-6).
- No code path issues a token without a verified PKCE verifier (AC-11, AC-18).
- One live token per `(account_id, oauth_client_id)`, enforced by the partial unique index rather than by remembering to check.
- Nothing the client said about itself is ever treated as a fact. The host shown on the approval page and the host a redirect goes to both come from the stored registration, and the name is escaped everywhere.
- The control plane makes no outbound HTTP request as part of this flow.
- A token issued here is indistinguishable to `Authenticate` from one pasted by hand, so every existing rule about suspension, ownership and caps applies without a second implementation.

**Security model**:

Unauthenticated: the three discovery documents and `POST /register`. A registration grants nothing; it creates a row that can be approved. Registration and the token endpoint are rate limited on the address, on a bucket of their own rather than the sign in one (AC-6, AC-22).

Session backed: `/authorize` in both methods. The account comes from the resolved session and nowhere else, the CSRF check is the one `/tokens` uses, and a suspended account is refused before the page renders (AC-14).

Bearer backed: `/mcp` and `POST /v1/uploads`, unchanged. `POST /token` is deliberately unauthenticated in the client sense, which is correct for a public client and is what PKCE plus the single use code exists to make safe.

The person is the authorization boundary. Anyone on the internet can register a client called anything; nothing happens until a signed in account approves it on a page that shows the one host the platform can actually verify.

No regulated data is in scope. The audit trail gains one action (AC-23).

**Configuration required**:

None. This feature adds no `DEPLOYER_*` variable (AC-30).

**Critical test scenarios**:

- Happy path: a real Claude desktop connector added by pasting `https://<MCPHost>/mcp`, discovering, registering, approving on the console and running a tool call, verifies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-9**, **AC-16**, **AC-19**, **AC-20**.
- Failure case: an authorization code presented twice refuses the second and revokes the token the first issued, verifies **AC-16a**.
- Failure case: two token requests for one code arriving together mint exactly one token, driven as a real race, verifies **AC-18a**.
- Failure case: a `redirect_uri` that is not registered for that `client_id` renders an error and issues no redirect, verifies **AC-10**, **AC-10c**.
- Failure case: a registered redirect URI presented percent encoded, with an uppercased host, and with a trailing slash is refused all three times, verifies **AC-10b**.
- Failure case: a wrong `code_verifier` answers `invalid_grant` and says nothing about which check failed, verifies **AC-18**.
- Failure case: two grants for one client driven concurrently leave exactly one live token, verifies **AC-19b**.
- Failure case: adding a connector three times over does not consume the sign in allowance for that address, verifies **AC-6**.
- Auth: a suspended account reaching `/authorize` is refused before consent, verifies **AC-14**.
- Auth: every console OAuth route answers 404 on the deploy host, and both protected resource documents answer 404 on the console host, verifies **AC-25a**.
- Hostile input: a client registering with a name full of markup and control characters is rendered escaped on the approval page and produces a sane token name in the list, verifies **AC-13**, **AC-20a**.

## Build plan

Tracer Bullet, the project default. A thin thread through all five endpoints goes first and is proven against a real Claude desktop connection before any bound is thickened, because the parts most likely to be wrong are the ones no unit test can reach: the exact metadata shapes, the redirect URI Claude actually sends, and whether the two hostname registrations behave as expected through the tunnel.

1. Migration `00007` with both tables, the new column and both indexes, plus the sqlc queries the later steps call, satisfies **AC-29**.
2. The thin thread, end to end and no wider: both protected resource documents, the `WWW-Authenticate` header, the authorization server document, a registration endpoint, a plain approval page, and a token endpoint that mints through the new transaction backed grant path from the start, since retrofitting a transaction under a working mint is worse than building it once. Proved by adding the connector in the real Claude desktop app and driving one deploy to healthy before anything else is built, satisfies **AC-1**, **AC-2**, **AC-3**, **AC-3a**, **AC-4**, **AC-9**, **AC-16**, **AC-17**, **AC-19**, **AC-19a**, **AC-20**, **AC-25**.
3. The refusals that keep it safe: redirect URI validation including both loopback literals, the exact comparison rule, the validation order that forbids an open redirect and the error page that leaks nothing, the PKCE requirement, the resource check at both ends, single use codes enforced by a conditional update with the replay revoke, and OAuth error codes throughout, satisfies **AC-4a**, **AC-5**, **AC-10**, **AC-10a**, **AC-10b**, **AC-10c**, **AC-11**, **AC-12**, **AC-16a**, **AC-18**, **AC-18a**, **AC-21**, **AC-24**.
4. The person's half done properly: the approval page's real content and ordering, the loopback warning in its pinned words, Deny, the CSRF check, the suspension refusal, and the session gate returning to an authorize URL with its query intact, satisfies **AC-9a**, **AC-13**, **AC-13a**, **AC-14**, **AC-15**.
5. The bounds and the trail: the new connector limiter bucket kept off the sign in one, the registration sweep on the existing daily runner, the `connector_grant` audit row in the one target pair `Audit` has, the concurrency proof on the grant, the bounded and cleaned token name, and the check that nothing here can take seconds, satisfies **AC-6**, **AC-7**, **AC-8**, **AC-8a**, **AC-19b**, **AC-20a**, **AC-22**, **AC-23**, **AC-28**.
6. The surface and the proof nothing else moved: the fifth `/connect` tab with its `<noscript>` fallback, the negative registration test in both directions with the method refusals distinguished, and the confirmation that the limiter's `429` and the suspended tool result are unchanged, satisfies **AC-2a**, **AC-25a**, **AC-25b**, **AC-26**, **AC-27**, **AC-30**.

## Consequences

**Positive**:
- The platform becomes addable from the Claude desktop app, claude.ai, Claude mobile and any other client that follows the MCP authorization specification, without being written for any of them.
- Joining loses its last technical step. A person pastes one address and presses one button.
- A connector's access is an ordinary token, so `/tokens` stays the single place credentials are listed and revoked, and suspension, the app cap and ownership all apply with no second implementation.
- The deploy path finally advertises itself. A client that cannot connect now gets a `401` that says where to look, instead of silence.

**Negative and tradeoffs**:
- This is the largest single feature since the public edge, and it is the first one whose failure modes are open redirect, code replay and consent phishing. None of those has a test in this repo today, and none of them is caught by the fake clientset.
- The platform is now a hand written authorization server with no library behind it. Every future correction to OAuth 2.1 or the MCP authorization specification is work that lands here.
- Registration is a public unauthenticated write, which is a new category for this codebase. The sweep bounds it and the limiter slows it, but a stranger can still create rows.
- Choosing dynamic registration over Client ID Metadata Documents is choosing the mechanism the MCP draft has deprecated. It works today and Claude falls back to it, but it will need revisiting.
- The console host now serves machine endpoints. It has been a browser only surface since spec 0021 and that is no longer true, so the mental model of what lives where costs a sentence more to explain.
- An access token with no expiry and no refresh token means a connector's access ends only when someone revokes it. That matches every other token here, and it does mean a stolen one is good until noticed.

**Neutral**:
- One new audit action, `connector_grant`, the first member added to that closed set in several specs.
- A third `identity.Settings` value beside `SignInSettings` and `DeployPathSettings`. Three named buckets is the point at which the set is worth reading as a list rather than as two special cases.
- `identity.Service` gains its first method whose store call is a transaction. The rules stay shared with `MintToken` through one helper, but the shape is new and the next person adding a mint path should follow it rather than adding a third.
- One written exception to the closed reason code rule, scoped to three routes and recorded in AC-24.
- No new configuration, no new credential path, and no change to the four command line blocks on `/connect`.

## Follow-up

- [ ] Before building, check whether Claude's `static_headers` beta is available on this account. If it is, the tokens `/connect` already issues may work in the desktop app with no code at all, which would change the urgency of this whole spec even though it would not cover other clients.
- [ ] Decide later whether to advertise Client ID Metadata Documents alongside registration, which the MCP draft prefers and which needs an answer to letting the control plane fetch an HTTPS URL a stranger chose.
- [ ] Consider whether the approval page should list a person's currently connected clients, or whether the token list carrying the client name is enough.
- [ ] Anthropic's outbound traffic comes from a published range, `160.79.104.0/21`. Worth deciding whether that is useful at the tunnel, or whether it is a bound that breaks quietly the day it changes.
- [ ] Per tool scopes were deliberately not built. Revisit if a connector is ever wanted that reads logs without being able to deploy.
