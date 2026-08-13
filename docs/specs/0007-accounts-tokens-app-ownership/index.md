# 0007. Accounts, API tokens and app ownership: people register, machines carry tokens

**Date**: 2026-08-12
**Status**: Accepted

## Summary

Today the platform has exactly one credential, a token seeded from an environment variable, and no way to make another. This spec adds people: a person registers with an email and a password, verifies the address by clicking a link, signs in to a browser session, and mints API tokens from there for their coding agent to carry. The account row that already owns apps, deployments and uploads becomes that person, so nothing about ownership changes shape. No pages are built here: every endpoint is JSON and drivable with curl, and the web interface is its own later feature designed on top of this one.

## Requirements

**User stories**:
- As a person who wants to deploy, I want to register with my email and password so that I get an account without you creating one for me.
- As a registered person, I want to mint and revoke API tokens so that my coding agent can deploy and I can cut off a credential I leaked.
- As an app owner, I want another account to be unable to deploy to, read logs from, or learn anything about my app so that the platform is safe to share.
- As the platform owner, I want to see who registered and disable an account so that I stay in control of my own cluster.

**Acceptance criteria**:

Registration and verification
- **AC-1**: `POST /v1/auth/register` with a valid email and a password of at least 12 characters creates an account, stores the password as an argon2id hash, issues a single use verification link valid for 24 hours, and answers `202` with a generic body that says to check the email.
- **AC-2**: Registering with an address that already has an account produces the identical `202` and body, and sends that address a "someone tried to register, sign in instead" message rather than a second verification link. No response, timing or status difference lets a caller learn whether an address is registered.
- **AC-3**: A password shorter than 12 characters, or an email that `net/mail.ParseAddress` rejects or that exceeds 254 characters, is refused `422` with the error code `password_too_short` or `email_invalid`. No composition rules beyond length are applied.
- **AC-4**: The first account created with an email becomes an admin (`is_admin = 1`); every later one does not. The bootstrap account, which has no email, never counts as the first and never becomes admin.
- **AC-5**: `GET /v1/auth/verify?token=…` consumes the link, stamps `email_verified_at`, and answers `200` with `{"verified": true}`. It works exactly once. A second use, an expired link, an unknown token, and a token whose `purpose` is `password_reset` rather than `verify_email` all answer `400` with `link_invalid`, in the same words for all four: a link is matched on its hash and its purpose together, never on the hash alone.
- **AC-6**: `POST /v1/auth/resend` issues a fresh link and marks the previous one consumed, so at most one verification link is live per account. It is limited to 3 per address per hour, and shares the per client bucket in AC-24.

Sessions
- **AC-7**: `POST /v1/auth/login` with correct credentials for a verified account creates a session row, sets an opaque session id in a cookie marked `HttpOnly` and `SameSite=Lax`, marked `Secure` when `DEPLOYER_PUBLIC_URL` is an `https` URL, and answers `200` with the account's email, display name and admin flag.
- **AC-8**: A wrong password, an unknown address, and a disabled account all answer `401` with the single code `credentials_invalid`, and every failure writes an audit row. A registered but unverified account is the one deliberate exception: it answers `403 email_unverified`, so the person is sent to resend rather than to reset. This is a bounded enumeration oracle and it is accepted knowingly, see the security model.
- **AC-9**: A session expires 30 days after its last use, and each authenticated request pushes that expiry forward. `POST /v1/auth/logout` revokes the current session immediately and clears the cookie by setting it empty with `Max-Age=0`.
- **AC-10**: Disabling an account, and changing its password, revoke every live session and every live verification or reset link the account holds, in the same transaction as the change.
- **AC-11**: Signing in to an account that has no `password_hash`, which is the bootstrap account, is refused with `credentials_invalid` and takes the same work as a real password check, so the answer is not faster.

Tokens
- **AC-12**: `POST /v1/tokens` with a name, and an optional lifetime in days, mints a token, returns the raw value exactly once in that response, and stores only its SHA-256 hash and its 8 character prefix. A lifetime between 1 and 365 sets `expires_at`; anything outside that range is refused `422 invalid_expiry`; without one the token never expires.
- **AC-13**: `GET /v1/tokens` lists the calling account's live tokens, newest first by `created_at`, with id, name, prefix, created, last used and expiry, and never a raw value or a hash. It lists no other account's tokens. There is no pagination: a token list is small by construction, since one live name per account is already enforced.
- **AC-14**: `DELETE /v1/tokens/{id}` revokes a token the caller owns; the token stops authenticating on the very next request. A token id belonging to another account answers `404`, the same answer an unknown id gets.
- **AC-15**: Minting a token from an unverified account is refused `403` with `email_unverified`.

Ownership and the verified gate
- **AC-16**: A token presented by an account whose `email` is set and whose `email_verified_at` is null is refused on every surface, MCP and upload alike, with the same answer an invalid token gets. An account with no email, meaning bootstrap, is exempt.
- **AC-17**: With two real registered accounts, the second cannot deploy to, read the status of, read logs from, or discover the existence of the first's app. Each refusal is the existing closed reason code (`app_unknown`, `deployment_unknown`, `upload_invalid`) and each is recorded in `audit_log`.
- **AC-18**: Two accounts may each own an app called the same name, and each gets its own hostname, because a slug carries a random suffix.

Admin
- **AC-19**: `GET /v1/admin/accounts` lists every account, newest first by `created_at`, with email, display name, verified state, admin flag and disabled state. No pagination, for the same reason as AC-13. `POST /v1/admin/accounts/{id}/disable` and `/enable` set and clear `disabled_at`. `DELETE /v1/admin/accounts/{id}/tokens/{tokenId}` revokes another account's token.
- **AC-20**: Every admin endpoint requires a live session belonging to an account with `is_admin = 1`. A session for an ordinary account answers `403` with `admin_required`; an API token presented instead of a session answers `401`, because admin surfaces accept sessions only.
- **AC-21**: An admin cannot deploy to, read logs from, or read the status of another account's app. The ownership boundary has no admin override.
- **AC-22**: Every admin action, every token mint and revoke, and every failed sign in writes an `audit_log` row naming the acting account and, where there is one, the target account or token.

Rate limiting and failure
- **AC-23**: After 5 failed sign ins for one address, further attempts are refused `429 rate_limited` for 30 seconds, doubling with each further failure to a ceiling of 15 minutes, and resetting on a successful sign in. The counter lives in memory and is lost on restart.
- **AC-24**: `POST /v1/auth/register`, `/login`, `/resend` and `/forgot` share an in process token bucket keyed by client address, capacity 10, refilling one token every 6 seconds, answering `429 rate_limited` when empty. The client address is the last hop of `X-Forwarded-For` when the request came through the ingress, never the raw connection address, which is the ingress pod for every caller.
- **AC-25**: A failure to send mail never fails the request that triggered it. The account or the link is still created, the failure is logged at error level, and the caller sees the same `202`.
- **AC-26**: With `DEPLOYER_RESEND_API_KEY` unset, register, resend and password reset answer `503` with `mail_unavailable`; every other endpoint, and the whole MCP and upload path, works normally.
- **AC-27**: No response body, log line, or audit row ever contains a raw token, a raw session id, a raw verification or reset link token, or a password.

Password reset
- **AC-28**: `POST /v1/auth/forgot` always answers `202` regardless of whether the address exists, and mails a single use reset link valid for 24 hours to an address that does exist.
- **AC-29**: `POST /v1/auth/reset` with a valid `password_reset` link token and a password of at least 12 characters sets the new hash, consumes the link, and revokes every live session for that account. A `verify_email` token presented here answers `400 link_invalid`, the same as an unknown one.

## Decision

**Chosen option**: Option 2: extend the account row into a person, and add a session backed web surface beside the existing token surface.

An account becomes a person: `accounts` gains `email`, `password_hash`, `email_verified_at`, `is_admin` and `display_name`, and every other table keeps pointing at `accounts(id)` exactly as it does today. Humans authenticate with a session cookie on new `/v1/auth`, `/v1/tokens` and `/v1/admin` endpoints; machines keep authenticating with a bearer token on `/mcp` and `/v1/uploads`, unchanged. Verification mail goes out through Resend.

**Implementation skills**: `resend` (`resend/resend-skills`, `.agents/skills/resend/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `database-migrations` (`~/.claude/skills/database-migrations/`)

## Rationale

Reasoning, the options weighed, and the premise note: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**

`accounts`, five columns added by migration `00002_identity.sql`:

| Column | Type | Null | Note |
|---|---|---|---|
| `email` | TEXT | yes | the login. Null on the bootstrap account, which is why it can never be signed into |
| `password_hash` | TEXT | yes | argon2id encoded string carrying its own salt and parameters |
| `email_verified_at` | TEXT | yes | null means unverified, which gates every surface |
| `is_admin` | INTEGER NOT NULL DEFAULT 0 | no | `CHECK (is_admin IN (0, 1))` |
| `display_name` | TEXT | yes | what a person is shown as. Deliberately not unique |

`display_name` is a new column rather than a reuse of `accounts.name`, because `name` is `NOT NULL UNIQUE` and was designed as a machine identifier. Two people whose addresses are `info@a.example` and `info@b.example` would otherwise collide on a constraint with no sensible error to return. Registration writes the account's own id into `name`, so the unique constraint is satisfied by construction and never becomes a user visible failure, and the human label lives in `display_name`.

Uniqueness on `email` is a partial unique index, not a column constraint, because SQLite cannot `ADD COLUMN ... UNIQUE`: `CREATE UNIQUE INDEX accounts_email ON accounts(email) WHERE email IS NOT NULL`. The partial predicate also lets every token only account keep a null.

`sessions`, new. Shaped like `api_tokens` on purpose: hashed value, `revoked_at`, `last_used_at`.

| Column | Type | Note |
|---|---|---|
| `id` | TEXT PRIMARY KEY | `ids.New(ids.Session, …)` |
| `account_id` | TEXT NOT NULL | references `accounts(id)` ON DELETE RESTRICT |
| `token_hash` | TEXT NOT NULL UNIQUE | SHA-256 hex of the cookie value |
| `expires_at` | TEXT NOT NULL | rolling, pushed forward on use |
| `last_used_at` | TEXT | |
| `revoked_at` | TEXT | |
| `created_at`, `updated_at` | TEXT NOT NULL | |

`email_tokens`, new. One table serving verification and password reset.

| Column | Type | Note |
|---|---|---|
| `id` | TEXT PRIMARY KEY | |
| `account_id` | TEXT NOT NULL | references `accounts(id)` ON DELETE RESTRICT |
| `purpose` | TEXT NOT NULL | `CHECK (purpose IN ('verify_email', 'password_reset'))` |
| `token_hash` | TEXT NOT NULL UNIQUE | SHA-256 hex of the link token |
| `expires_at` | TEXT NOT NULL | 24 hours from creation |
| `consumed_at` | TEXT | set on use, and on being superseded by a resend |
| `created_at` | TEXT NOT NULL | |

`CREATE UNIQUE INDEX email_tokens_live ON email_tokens(account_id, purpose) WHERE consumed_at IS NULL`, so at most one live link per purpose. A resend stamps the previous row consumed first, the same shape as `api_tokens_live_name`.

Unchanged: `api_tokens` (its existing `expires_at` finally gets written, optionally), `audit_log` (new action strings only), `apps`, `deployments`, `deployment_events`, `releases`, `uploads`, `app_config`. Rate limiting holds no state in the database.

Relationships: `accounts` 1:N `sessions`, `api_tokens`, `email_tokens`, `apps`, `uploads`, `deployments`. No membership table and no join: a person is exactly one account.

**State transitions**

An account walks: `registered` (email set, `email_verified_at` null) → `verified` (`email_verified_at` set) → `disabled` (`disabled_at` set) and back to `verified` on enable. Only `verified` may mint a token or authenticate on any surface. The bootstrap account sits outside this machine entirely, because it has no email.

An `email_tokens` row walks: `live` → `consumed` (used, or superseded by a resend) or simply expires by `expires_at` passing. There is no way back.

**API surface**

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/v1/auth/register` | POST | `email`, `password`, `name` (optional) | `202`, generic message | none | 422 `email_invalid`, 422 `password_too_short`, 429, 503 `mail_unavailable` |
| `/v1/auth/verify` | GET | `token` query param | `200`, `{"verified": true}` | none | 400 `link_invalid` |
| `/v1/auth/resend` | POST | `email` | `202`, generic message | none | 429, 503 `mail_unavailable` |
| `/v1/auth/login` | POST | `email`, `password` | `200`, `email`, `name`, `is_admin`; sets cookie | none | 401 `credentials_invalid`, 403 `email_unverified`, 429 |
| `/v1/auth/logout` | POST | none | `204` | session | 401 |
| `/v1/auth/forgot` | POST | `email` | `202`, generic message | none | 429, 503 `mail_unavailable` |
| `/v1/auth/reset` | POST | `token`, `password` | `204` | none | 400 `link_invalid`, 422 `password_too_short` |
| `/v1/auth/me` | GET | none | `email`, `name`, `is_admin`, `verified` | session | 401 |
| `/v1/tokens` | POST | `name`, `expires_in_days` (optional, 1 to 365) | `id`, `name`, `prefix`, `token` (once) | session | 401, 403 `email_unverified`, 409 `token_name_taken`, 422 `invalid_expiry` |
| `/v1/tokens` | GET | none | list of id, name, prefix, created, last used, expiry | session | 401 |
| `/v1/tokens/{id}` | DELETE | path id | `204` | session | 401, 404 |
| `/v1/admin/accounts` | GET | none | list of accounts | admin session | 401, 403 `admin_required` |
| `/v1/admin/accounts/{id}/disable` | POST | path id | `204` | admin session | 401, 403, 404 |
| `/v1/admin/accounts/{id}/enable` | POST | path id | `204` | admin session | 401, 403, 404 |
| `/v1/admin/accounts/{id}/tokens/{tokenId}` | DELETE | path ids | `204` | admin session | 401, 403, 404 |

Error bodies are `{"error": {"code": "…", "message": "…"}}`, where `code` is drawn from a closed set declared in `internal/identity`: `email_invalid`, `password_too_short`, `credentials_invalid`, `email_unverified`, `link_invalid`, `admin_required`, `token_name_taken`, `invalid_expiry`, `not_found`, `rate_limited`, `mail_unavailable`, `internal`. This set is separate from `internal/domain.Reason`, which describes deploy outcomes and stays exactly as it is.

**Value sourcing**

| Action | Value produced / displayed | Source |
|---|---|---|
| register | account id | `ids.New(ids.Account, clock.Now())`, as today |
| register | `password_hash` | argon2id over the input password, parameters below |
| register | `display_name` | the optional `name` input; when absent, the local part of the email before the `@`. Never unique |
| register | `accounts.name` | the account's own id, so the existing `NOT NULL UNIQUE` constraint is satisfied without a user visible collision |
| verify, reset | the matched link row | `email_tokens` looked up on `token_hash` **and** `purpose` together, so a token minted for one purpose cannot be spent on the other |
| mint token | `expires_in_days` bound | 1 to 365 inclusive; outside it is `422 invalid_expiry` |
| list tokens, list accounts | row order | `created_at DESC`, matching the existing `apps_by_account` and `deployments_by_app` convention |
| any rate limited endpoint | the client address | the last hop of `X-Forwarded-For`, trusting only the ingress. The raw connection address is the ingress pod for every caller, which would collapse every bucket into one |
| register | `is_admin` | computed: 1 when no other account with a non null `email` exists, inside the creating transaction |
| register, resend, forgot | the link token | 32 bytes from `crypto/rand`, base64url encoded, hashed with the existing `auth.HashToken` before storage |
| register, resend, forgot | the link URL in the mail | `DEPLOYER_PUBLIC_URL` plus the endpoint path plus the raw token. Not `DEPLOYER_INTERNAL_URL`: this text is handed to a person, which is exactly the rule in AGENTS.md |
| register, resend, forgot | the From address | `DEPLOYER_MAIL_FROM` |
| login | the session cookie value | 32 bytes from `crypto/rand`, base64url encoded; only its SHA-256 hash is stored |
| login | the cookie `Secure` flag | derived: true when `DEPLOYER_PUBLIC_URL` parses to scheme `https` |
| login | session `expires_at` | `clock.Now()` plus 30 days, recomputed on each authenticated request |
| mint token | the raw token | 32 bytes from `crypto/rand`, base64url encoded, prefixed `dpl_` so it is recognisable in a paste |
| mint token | `token_prefix` | the existing `auth.TokenPrefix`, the first 8 characters |
| mint token | `expires_at` | `clock.Now()` plus `expires_in_days` when given, otherwise null |
| list tokens | `last_used_at` | the `api_tokens` column the existing `TouchToken` already writes |
| any authenticated request | current account | resolved from the session cookie hash, or the bearer token hash, never from a request body or argument |
| admin list | verified state | `email_verified_at IS NOT NULL` |
| every audit row | acting account, target | the resolved account id and, for admin actions, the path id |

**Key invariants**

- A raw credential exists only in the request or response that carries it. Passwords, session ids, link tokens and API tokens are stored as hashes and never logged, at any level.
- Every account resolution, on every surface, goes through one function that also applies the verified gate and the disabled check, so a new surface cannot forget them. Concretely: `internal/auth.Account` grows `Email`, `Verified`, `Disabled` and `IsAdmin`, and `internal/auth.Authenticator` stays the single resolution path for both routes, gaining a session lookup beside its token lookup. `internal/identity` owns the pure rules (password policy, hashing, link and session lifetimes, the first admin rule, the error code set) and never resolves a caller itself. Without this split the invariant cannot be implemented, because two packages would each hold half of it.
- An account holding a non null `email` with a null `email_verified_at` authenticates nowhere. An account with a null `email` is exempt, which is precisely the bootstrap account.
- Ownership has no override. Nothing in the platform, including an admin session, reads or writes an app it does not own.
- The database decides uniqueness. Email uniqueness, one live token name per account, and one live link per purpose are unique indexes; a losing insert is caught and translated, never guarded by a read before the write.
- Every state change that must happen together happens in one transaction: create account plus first admin computation; disable plus session and link revocation; password change plus session revocation.
- Mail is best effort and never part of a transaction's success. It is sent after the commit.
- Concurrent password hashes are bounded by a semaphore, so a burst of sign ins cannot push the pod past its 512Mi limit.

**Security model**

- **Machines** hold API tokens and reach `/mcp` and `/v1/uploads`. That path is unchanged except for the verified gate.
- **People** hold session cookies and reach `/v1/auth`, `/v1/tokens` and `/v1/admin`. A session never authenticates a machine surface, and a token never authenticates an admin surface.
- **Ownership** is the account id on the row, exactly as it is today. Admin adds visibility over accounts and the power to disable one or revoke its tokens; it adds nothing over apps, deployments or logs.
- Password hashing is argon2id with `m = 19456` KiB, `t = 2`, `p = 1`, a 16 byte random salt and a 32 byte output, which is the current OWASP minimum, held to roughly 50ms per hash. A semaphore caps concurrent hashes at 4.
- Account enumeration is closed at register and forgot: the same status, the same body, and comparable work whether the address exists or not. Login carries one deliberate exception, `403 email_unverified`, which does reveal that an address is registered. It is accepted knowingly: a person who registered and never received the mail must be sent to resend rather than to password reset, and the perimeter is a tailnet, so the attacker this leaks to is already inside your network. A wrong password, an unknown address and a disabled account remain indistinguishable.
- The perimeter remains the tailnet. Nothing here is reachable from the public internet, which is why per address rate limiting in memory is proportionate rather than a distributed limiter.
- No regulated data is in scope. Email addresses and password hashes are personal data, so they are never logged and never returned to anyone but their owner or an admin.

**Configuration required**

- `DEPLOYER_RESEND_API_KEY`: the Resend API key, held in a sealed Secret. Optional. Unset means no mail sender, and the endpoints that need one answer `503 mail_unavailable`.
- `DEPLOYER_MAIL_FROM`: the From address, `noreply@deploy.toyintest.org`. Required whenever the API key is set, and validated together with it at startup.

Both follow the existing rule in AGENTS.md: validated in `internal/config` at startup, never at first use.

**Critical test scenarios**

- Happy path: register, receive the link, verify, sign in, mint a token, deploy with it, verifies **AC-1**, **AC-5**, **AC-7**, **AC-12**.
- Enumeration: registering an existing address is byte identical to registering a new one, verifies **AC-2**.
- First admin: on a database holding only the bootstrap account, the first registration is admin and the second is not, verifies **AC-4**.
- Ownership: account B is refused deploy, status and logs against account A's app, with the existing reason codes and an audit row each, verifies **AC-17**, **AC-21**.
- Verified gate: a token minted before verification, then verification reversed in the database, stops authenticating on MCP, verifies **AC-16**.
- Revocation: a session survives use, then a disable revokes it and the next request is refused, verifies **AC-10**.
- Bootstrap: signing in as `bootstrap` is refused and takes comparable time to a real password check, verifies **AC-11**.
- Mail down: with the Resend call failing, registration still succeeds and logs the failure, verifies **AC-25**.
- No mail configured: with the key unset, register answers `503` and a deploy still works, verifies **AC-26**.
- Throttle: repeated failed sign ins answer `429` and a correct password afterwards resets the counter, verifies **AC-23**.
- Race: two concurrent registrations of one address produce one account and one duplicate answer, verifies **AC-2**.
- Redaction: no raw token, session id, link token or password appears in any response, log or audit row across the whole suite, verifies **AC-27**.

## Build plan

Tracer Bullet: the thin thread is register, verify, sign in, mint, deploy. Task 2 stands that whole thread up end to end before anything is thickened, and mail is real from the first moment because a link that never arrives is the failure most worth finding early.

1. The migration and the store layer: `00002_identity.sql` with the five columns, the partial unique email index, `sessions` and `email_tokens`; the sqlc queries and hand written store methods for each; the new `ids` kinds; store tests against a real SQLite file including the partial index behaviour. Satisfies **AC-1**, **AC-5**, **AC-7**, **AC-12**.
2. The thin thread end to end: `internal/identity` with the password policy, argon2id hashing behind its semaphore, link and session token generation, the first admin rule and the closed error code set, all pure and test first; `internal/mail` with the Resend client behind the interface `identity` declares; register, verify, login and mint wired into `internal/httpapi` and the cookie set correctly. Proves register through deploy in one pass. Satisfies **AC-1**, **AC-3**, **AC-4**, **AC-5**, **AC-7**, **AC-12**, **AC-25**, **AC-26**.
3. The gate everywhere, and the ownership proof: one account resolution path applying disabled and unverified on both the session and the bearer route; the exemption for an account with no email; the two account ownership tests across deploy, status and logs. Satisfies **AC-15**, **AC-16**, **AC-17**, **AC-18**, **AC-21**.
4. Session lifecycle and token management: rolling expiry, logout, revocation on disable and on password change; list and revoke tokens; forgot and reset sharing the `email_tokens` machinery; resend superseding the live link. Satisfies **AC-6**, **AC-9**, **AC-10**, **AC-13**, **AC-14**, **AC-28**, **AC-29**.
5. Admin, audit and the hardening: the four admin endpoints behind the session only admin check; the new audit actions; the enumeration safe register and forgot answers with the second mail template; the constant work bootstrap sign in; per address and per client rate limiting; the redaction sweep across responses, logs and audit rows. Satisfies **AC-2**, **AC-8**, **AC-11**, **AC-19**, **AC-20**, **AC-22**, **AC-23**, **AC-24**, **AC-27**.

## Consequences

**Positive**:
- The platform stops being single tenant in practice. More than one person can use it, and the ownership boundary that was written in slice 1 finally gets proved with two real accounts rather than argued for.
- A leaked credential has an answer that is not "rotate the sealed Secret and restart": revoke the token, or disable the account.
- The bootstrap token keeps working exactly as it does, so nothing about the current deploy path or your existing scripts changes.
- Every acceptance criterion here is drivable with curl, so this slice is fully verifiable before a single page exists.

**Negative / tradeoffs**:
- The platform gains its first hard dependency on an outside service. Resend being down, or an API key expiring, means nobody new can register, and the failure is visible only in the platform log.
- Passwords are now a thing that can be stolen from you. The database file, already a secret store because releases snapshot configuration in clear, now also holds password hashes, and it still has no backup or encryption at rest.
- Spec 0002's single migration decision ends here. There will be a `00002`, run against a live database that has no backup, which is exactly the situation that decision was written to avoid.
- A meaningful amount of security sensitive code that is easy to get subtly wrong: enumeration, timing, session fixation, cookie flags, rate limiter keys. It is all conventional and all worth reviewing carefully.
- The MCP path takes a small cost it did not have: one extra check per authenticated call for the verified and disabled state.

**Neutral**:
- Three new packages, `internal/identity`, `internal/mail`, and new handlers in `internal/httpapi`, following the same layering the project already uses: pure rules inside, edges at the boundary.
- A DNS change you have to make yourself: SPF, DKIM and DMARC records on `deploy.toyintest.org` so Resend can send as `noreply@deploy.toyintest.org`.
- `golang.org/x/crypto` becomes a direct dependency. It is pure Go, so the cgo free build holds.
- The web interface is now unblocked and is the obvious next feature, with this surface as its whole API.

## Follow-up

- [ ] Take a copy of the SQLite file off the cluster before the `00002` migration rolls out. There is no backup story, so this is manual and it matters.
- [ ] Add the SPF, DKIM and DMARC records for `deploy.toyintest.org` in the DNS zone, and verify the domain in Resend, before task 2 is verified.
- [ ] The web interface is a new scope feature to design on this surface, covering pages, composition and look. It is not designed here.
- [ ] Account deletion is deliberately out of scope. It needs app decommissioning, which is slice 9, to answer what happens to a running workload, its namespace and its hostname.
- [ ] The official Resend MCP server (`https://mcp.resend.com/mcp`, hosted, OAuth) would let a build send real test mail and inspect delivery. Not connected; connecting it is a config step you run.
- [ ] The `resend` skill's conventions are not yet in an `AGENTS.md`. They are area specific, so they belong in a nested `internal/mail/AGENTS.md` once that package exists, not in root.
- [ ] Platform backup and restore is now more clearly load bearing than it was, because the database holds password hashes. Still deferred, still worth raising in priority.

## Migration plan

**Strategy**: additive migration, no code freeze, no data transformation.

**Phases**:
1. Copy the SQLite file off the cluster by hand. There is no backup mechanism, so this is the entire safety net.
2. Roll out the binary carrying `00002_identity.sql`. Goose runs it at startup, as it already runs `00001`. Every added column is nullable or has a constant default, and the two new tables are empty, so no existing row needs a value and no read path changes meaning.
3. Register the first account, which becomes the admin, and confirm the bootstrap token still deploys.

**Rollback**: the previous binary reads the new schema without harm, because the added columns are nullable and the added tables are ignored. Rolling back the image is enough; the goose down migration exists but should not be needed, and running it would drop live sessions and tokens.

**Risks**: the pod runs one replica with the database on a volume, so the migration happens during the normal rollout gap with no traffic to lose. The real risk is not the migration but the manual copy in phase 1 being skipped, which is why it leads the plan rather than sitting in Follow-up alone.
