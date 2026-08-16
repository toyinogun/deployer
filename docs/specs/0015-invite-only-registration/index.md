# 0015. Invite only registration

**Date**: 2026-08-14
**Status**: Accepted

The decision record (context, options considered, rationale) is in [rationale.md](rationale.md). The verify checklist is in [verify.md](verify.md).

## Summary

Anyone who can open `/register` today can create an account, which was safe only because reaching the page meant being on the tailnet (the private network the platform sits behind). Slice 13 puts the platform on the open internet, so registration needs its own door: a single use invite link an admin mints and hands to a person. This spec adds one `invites` table, one refusal code, one admin page, and one new check at the top of the register path. Everything else about registering stays exactly as spec 0007 built it.

## Requirements

**User stories**:
- As the platform owner, I want to hand a specific person a link that creates exactly one account, so that a publicly reachable registration page does not mean a publicly open platform.
- As the platform owner, I want to see and revoke the invites I have issued, so that a link I sent to the wrong place, or that a person never used, stops being a way in.
- As an invited person, I want the link to take me to an ordinary registration form, so that joining is one step and not a support conversation.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):
- **AC-1**: Registration with no invite is refused `invite_invalid` on both surfaces, `POST /register` and `POST /v1/auth/register`, and writes no account row.
- **AC-2**: Unknown, expired, revoked, and already spent codes are each refused with the same `invite_invalid` code, the same status, and the same words, on both surfaces.
- **AC-3**: A valid invite registers exactly as before: the account is created, the verification message is sent, and the invite is stamped `consumed_at` and `consumed_by` in the same transaction as the account insert.
- **AC-4**: An invite creates at most one account. Two registrations racing on one code end with exactly one account created and the other refused `invite_invalid`.
- **AC-5**: An invite issued more than seven days ago is refused, with no admin action needed, and the expiry is enforced inside the spend guard as well as in the check that precedes it.
- **AC-6**: An admin can issue an invite from `/admin/invites` with an optional note of at most 200 characters, and the resulting link is shown once on the page that minted it.
- **AC-7**: An admin can revoke a live invite. It is refused from then on, and it stays in the list marked revoked rather than disappearing. Revoking an invite that is already spent or expired is refused `not_found` and changes nothing.
- **AC-8**: The invite list shows, per invite, its note, who issued it, when it expires, and its one current state: live, spent by a named account, revoked, or expired.
- **AC-9**: Only an admin session reaches the invite pages and endpoints. An ordinary session is refused `admin_required`, and an API bearer token never reaches them at all, exactly as the accounts admin surface behaves.
- **AC-10**: Registering with a valid invite on an address that already has an account answers byte for byte identically to a fresh registration, creates nothing, and leaves the invite live and usable.
- **AC-11**: The invite is checked before the password is hashed, so a caller with no valid invite never costs the platform a key derivation.
- **AC-12**: Existing accounts, sessions, and API tokens are untouched. The migration is additive and the previous binary reads the resulting schema without harm.
- **AC-13**: On startup, when no account has an email address and no live bootstrap invite already exists, the platform mints one bootstrap invite and writes its link at info level. It mints nothing and logs nothing when either condition fails, so a restarting pod cannot leave several live bootstrap invites behind.
- **AC-14**: A raw invite code exists in exactly two places the platform controls, the admin response that minted it and the bootstrap log line. It is never in the database, never in another platform log line, never in an audit row, and never in a mailed message. The register page sends `Referrer-Policy: no-referrer`, so the code in the query string does not travel onward from the browser. _Amended by [spec 0025](../0025-emailed-invites-bound-to-an-address/index.md) AC-12: once an invite can be bound to an address, the one message sent to that address is a third allowed place. Everything else in this criterion stands._
- **AC-15**: Issuing, revoking, and spending an invite each write an audit row, the spend naming the invite and the account it created. A refused revoke writes one too, carrying its reason, matching what `adminFailure` already does on the accounts page.
- **AC-16**: `GET /register` with no code renders normally and says in one plain sentence that registration is invite only.
- **AC-17**: A refused invite spends the same rate limit bucket a refused registration already spends.
- **AC-18**: `GET /register` never validates the code it is given. Unknown, expired, revoked, spent, and valid codes all render the identical form, and every distinction is made on the post.
- **AC-19**: Both invite mutations, the mint and the revoke, carry the same synchroniser token check `adminDisable` and `adminEnable` already carry, and a post without it is refused.

## Decision

**Chosen option**: Option 1: An invite row checked at the top of the existing register path.

Registration requires a single use, seven day, unbound invite code, minted by an admin, checked before any other registration work, and spent in the same transaction that creates the account.

**Implementation skills**: `security-patterns` (`~/.claude/skills/security-patterns/`) · `database-migrations` (`~/.claude/skills/database-migrations/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`)

## Feature design

**Data model sketch**:

`invites` (new table)

| Column | Type | Null | Meaning |
|---|---|---|---|
| `id` | TEXT PRIMARY KEY | no | |
| `code_hash` | TEXT NOT NULL UNIQUE | no | `HashSecret` of the raw code. The raw value exists only between minting and the one response that shows it |
| `note` | TEXT | yes | The admin's own words about who this went to, at most 200 characters, validated where `CheckEmail` validates. The only record of that, since the invite is not bound to an address |
| `created_by` | TEXT REFERENCES accounts(id) ON DELETE RESTRICT | yes | Null means the platform minted it at boot, the same way the bootstrap account carries a null email |
| `expires_at` | TEXT NOT NULL | no | Issued at plus seven days |
| `consumed_at` | TEXT | yes | Stamped in the account creation transaction |
| `consumed_by` | TEXT REFERENCES accounts(id) ON DELETE RESTRICT | yes | The account this invite created |
| `revoked_at` | TEXT | yes | An admin pulled it back before anyone spent it |
| `created_at` | TEXT NOT NULL | no | |

`STRICT`, like every table since `00001`. Index `invites_by_created ON invites(created_at DESC)` for the admin list; `code_hash` is already unique.

Relationships: `accounts` 1:N `invites` through `created_by`, and `invites` 1:1 `accounts` through `consumed_by`. Nothing else in the schema changes, and no existing column moves.

**State transitions**:

`live` → `spent` (a registration succeeded) · `live` → `revoked` (an admin acted) · `live` → `expired` (time passed, no write).

`live` is the predicate every lookup uses, in full and without exception: `consumed_at IS NULL AND revoked_at IS NULL AND expires_at > now`. The spend guard and the revoke guard both carry all three clauses, so neither can act on a row the other has already ended, and `revoked_at` is only ever set on a row that was live at the moment it was revoked. That is also what removes any question of precedence in the derived state, since no row can be both revoked and expired at the point it was written. The three end states are terminal and mutually exclusive: a revoke on a spent or expired invite changes nothing and is refused `not_found`, and expiry is computed rather than stored, so nothing sweeps the table.

**API surface**:

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/register` | GET | `invite` (query, optional) | the form, code carried in a hidden field, `Referrer-Policy: no-referrer` | none | none, the code is never validated here (AC-16, AC-18) |
| `/register` | POST | `invite`, `email`, `password`, `display_name` | redirect to check your mail | none, rate limited | 403 `invite_invalid`, 422 `email_invalid`, 429 `rate_limited` |
| `/v1/auth/register` | POST | `invite`, `email`, `password`, `display_name` | 204 | none, rate limited | the same codes and statuses |
| `/admin/invites` | GET | | the list plus the mint form | admin session | 403 `admin_required` |
| `/admin/invites` | POST | `note` (optional, 200 chars) | the list, with the new link shown once | admin session, CSRF | 403 `admin_required`, 422 on an over long note |
| `/admin/invites/{id}/revoke` | POST | | redirect to the list | admin session, CSRF | 403 `admin_required`, 404 `not_found` |
| `/v1/admin/invites` | GET | | the list as JSON, never a raw code | admin session | 403 `admin_required` |
| `/v1/admin/invites` | POST | `note` | the invite plus its raw code, once | admin session | 403 `admin_required` |
| `/v1/admin/invites/{id}` | DELETE | | 204 | admin session | 403 `admin_required`, 404 `not_found` |

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| Issue an invite | the raw code | `identity.NewSecret`, the same generator the verify and reset links use |
| Issue an invite | the link shown to the admin | `Options.PublicURL` plus `/register?invite=`, built by the same rule as `linkURL`, because a person's browser cannot resolve cluster DNS |
| Issue an invite | `expires_at` | `Clock.Now()` plus a new `InviteLifetime` constant of seven days, beside `LinkLifetime` |
| Issue an invite | `created_by` | the admin's account id from the session, or null on the bootstrap path |
| List invites | each row's state | derived from `consumed_at`, `revoked_at`, `expires_at` against `Clock.Now()`, never a stored status column |
| List invites | "spent by" | `consumed_by` joined to `accounts.email`; an unverified account shows its address, since an admin already sees every address on the accounts page |
| List invites | the issuer's name | `created_by` joined to `accounts.display_name`, showing "the platform" when null |
| Register | the invite id to spend | looked up by `HashSecret(submitted code)` under the live predicate |
| Register | `consumed_by` | the account id, generated before the transaction opens so the guarded update and the insert can name each other |
| Register | which of the two refusals happened | the typed error from `SpendInviteAndCreateAccount`: `ErrInviteInvalid` when the guarded update touches no row, `ErrEmailTaken` when the account insert loses. Both roll the whole transaction back, and only the first is visible to the caller |
| Register | the refusal message | the one sentence on `CodeInviteInvalid`, from `internal/identity` for the JSON surface and `internal/web` for the page, the existing split |
| Bootstrap | whether to mint | two store reads, run once at startup: any account with a non null email, and any live invite with a null `created_by`. Either one being present means no mint |
| Audit rows | actor, target, action | `auth.Record` with `ActionAdmin` for issue and revoke; the spend records the new account as actor with the invite id as target |

**Key invariants**:
- The invite lookup is the first statement in `Service.Register`, ahead of `CheckEmail` and `CheckPassword`, not merely ahead of the hash. A caller with no valid invite gets `invite_invalid` whatever else is wrong with their submission, so the gate is never spoken past by a validation message (AC-11, AC-1).
- The spend is a conditional `UPDATE ... WHERE id = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > @now` returning a row count, inside the same transaction as the account insert. The predicate is the full live one, so nothing rests on the earlier check still being true by the time the transaction runs. Zero rows updated rolls the whole transaction back and the caller sees `invite_invalid`. This is what makes AC-4 and AC-5 true rather than probable.
- One store method, `SpendInviteAndCreateAccount`, owns both writes. It takes the account id its caller generated, runs the guarded update then the account insert, and returns one of two typed errors, `ErrInviteInvalid` for a guard that touched no row and `ErrEmailTaken` for a losing insert, rolling everything back either way. `Service.Register` is what turns those into a refusal or into the identical check your mail answer, which is why the distinction has to survive the store boundary rather than collapsing into one error.
- A raw code is never written to the database, a platform log line, an audit row, or a mailed message. The two exceptions are deliberate and named in AC-14.
- `GET /register` never touches the invites table. The code is copied from the query into a hidden field and nothing else, so the page cannot become a second oracle telling a holder which kind of bad code they have.
- A taken email address does not spend the invite, because no account was created. This preserves spec 0007's equal answer property: the caller cannot tell the two cases apart, and their link still works (AC-10).
- The browser and the JSON surface refuse identically. `Service.Register` holds the check, so neither handler can be the weaker door.
- Expiry is computed, never swept. No background job, and a clock change cannot leave a stale stored status behind.

**Security model**:
- Minting and revoking are admin only, through a cookie session, exactly as the accounts admin surface is, and both run `checkCSRF` as `adminDisable` and `adminEnable` do. An API bearer token cannot reach them, because these handlers read a session and a token is not one.
- The code travels in a query string, which means it reaches whatever sits in front of the platform. The ingress in slice 13 will log request lines by default, so the raw code lands in a log the platform does not own. This is accepted rather than designed around: the register link has to be one thing a person clicks, the code is single use and dies in seven days, and anyone reading the ingress logs is already inside the cluster. The two mitigations that are cheap are taken, `Referrer-Policy: no-referrer` so the browser does not forward it, and a follow up to look at what the tunnel and the ingress actually record before the address goes public.
- The invite code is a full strength random secret with the same generator and hashing as the verify and reset links, so guessing is not a practical path and the existing register rate limit bucket is the whole of the brute force answer (AC-17).
- Possession of the link is the authorisation. That is the accepted tradeoff: it is chosen deliberately over binding to an address, and the mitigations are the short lifetime, single use, and revocation.
- No new regulated data. The invite holds a note an admin typed and two account references, nothing about the invitee beyond what the admin chose to write.
- The first account created on a fresh database becomes admin through the existing `CreateIdentityAccount` rule, so the bootstrap invite makes exactly one admin and no more.

**Configuration required**:

None. The seven day lifetime is a Go constant beside `LinkLifetime`, because it is a product decision about how long a link stays good rather than an operational knob, the same reasoning that keeps the log bounds out of `DEPLOYER_*`.

**Critical test scenarios**:
- Happy path: mint an invite, register through the link, land on check your mail, and see the invite listed as spent by that account, verifies **AC-3**, **AC-6**, **AC-8**.
- Failure case: two goroutines register concurrently on one code against a real SQLite file; exactly one account exists afterwards and the loser sees `invite_invalid`, verifies **AC-4**.
- Failure case: no code, an unknown code, an expired one, a revoked one, and a spent one each produce the identical response body and status on both surfaces, verifies **AC-1**, **AC-2**.
- Failure case: register a taken address with a live invite, then use the same invite on a fresh address and succeed, verifies **AC-10**.
- Auth/permission: an ordinary session gets `admin_required` on every invite route, a bearer token gets no further, and a mint or revoke without the synchroniser token is refused, verifies **AC-9**, **AC-19**.
- Failure case: `GET /register` with an unknown, expired, revoked, and spent code each render the identical form, verifies **AC-18**.
- Bootstrap: an empty database mints one invite and logs the link; restarting three times against that same empty database mints nothing further; and a database with one human account mints none, verifies **AC-13**.
- Leak: the existing `internal/web` leak crawl extended over the invite pages, plus a log assertion that no raw code appears outside the bootstrap line, verifies **AC-14**.

## Build plan

Tracer Bullet, read from `AGENTS.md`. The thin thread here is one invite going end to end from an admin's click to a created account, because that is the path that proves the schema, the transaction, the service check, and both surfaces at once. The admin list, the bootstrap, and the polish thicken it afterwards.

1. The `00003_invites.sql` migration and the store layer: the table, the index, the sqlc queries in a new `invites.sql`, and the `IdentityStore` methods, purely additive, satisfies **AC-12**.
2. The thin thread: `InviteLifetime`, `CodeInviteInvalid` and its 403 mapping, the lookup as the first statement in `Service.Register`, `SpendInviteAndCreateAccount` with its two typed errors, and a mint path an admin can drive, proving one invite from mint to account, satisfies **AC-1**, **AC-3**, **AC-5**, **AC-11**.
3. The race and the taken address: the conditional update carrying the full live predicate, the rollback on either failure, and leaving the invite untouched when the address is spoken for, satisfies **AC-4**, **AC-10**.
4. Both doors and the refusal shape: `invite` on `POST /v1/auth/register`, the hidden field, the `Referrer-Policy` header and the bare page sentence on `/register`, a GET that never validates, and every failure reading identically on both, satisfies **AC-2**, **AC-16**, **AC-17**, **AC-18**.
5. The admin surface: `/admin/invites` and its three JSON equivalents, mint with a bounded note, the link shown once, revoke guarded on the live predicate, the CSRF check on both mutations, and the derived state column, satisfies **AC-6**, **AC-7**, **AC-8**, **AC-9**, **AC-19**.
6. The bootstrap and the closing pass: the startup mint gated on both conditions, the audit rows for issue, revoke, refused revoke, and spend, and the leak crawl extended over the new pages, satisfies **AC-13**, **AC-14**, **AC-15**.

## Consequences

**Positive**:
- The front door becomes a real control before the address becomes publicly resolvable in slice 13, which is the ordering the slice was written for.
- Who invited whom is now a fact on a row, so an incident has a trail rather than a guess.
- One bad link is revocable on its own, without touching anyone else's access or shipping a deploy.
- A restore into an empty database now recovers on its own, which was not true before this spec: the bootstrap path is a small gift to the backup feature in slice 12.

**Negative / tradeoffs**:
- Registration is no longer self service. Every new person is now an action you personally have to take, which is the point, but it is also a chore that scales with people.
- Possession of the link is the authorisation, so a forwarded link is a working account until it is spent or revoked. Chosen knowingly over binding to an address.
- The bootstrap invite writes a working credential into the pod logs. It fires only on an empty database with no live bootstrap invite already outstanding, and anyone reading your pod logs already has the cluster, but it is a real secret in a log line and it should be understood as one.
- The code appears in a URL, so it reaches the ingress access log once the public edge lands. Accepted for a single use secret with a seven day life, and mitigated only as far as the browser side goes.
- The invite list grows with every registration ever made rather than with headcount, because spent and revoked invites stay on it. At the stated scale that is tens of rows for years, so it is accepted unpaginated, unlike the accounts list where the bound is the number of people.
- One more table, one more page, and one more refusal code to keep coherent across two surfaces, which is a permanent small tax on the identity area.

**Neutral**:
- The migration is additive, so a rollback to the previous image keeps working: the old binary ignores the table and registration goes back to open. That is the rollback path, and it is worth knowing that rolling back reopens the door.
- Nothing changes for accounts that already exist, and no existing session or token is affected.
- The MCP surface is untouched. Registration has never been an agent action and is not becoming one.

## Migration plan

**Strategy**: additive schema, single deployment.

**Phases**:
1. Ship the migration and the code together. The table is empty, every column is nullable or defaulted, and no existing row needs a value, so there is no backfill and no coordination window.
2. Mint yourself an invite from the admin page and register a throwaway account through it, on the tailnet, before slice 13 makes the address public.

**Rollback**: revert the image. The old binary does not read `invites`, registration returns to open, and the table sits unused until you roll forward again. No data is lost in either direction. The one thing to hold in mind is that a rollback after the public edge lands in slice 13 reopens registration to the internet, so the two are ordered deliberately: this ships first and is proven on the tailnet.

**Risks**:
- Registering yourself out of the platform on a fresh database is the real failure mode, and the bootstrap path in AC-13 exists to close it. It is worth actually testing against an empty database rather than assuming, because it fires exactly once and only when you least want to debug it.
- A half applied deploy where the code is new and the migration has not run would refuse every registration. Goose runs at startup before the server serves, so this is not reachable, but it is why the migration stays in the same commit as the check.

## Follow-up

- [ ] Before slice 13 makes the address public, look at what the tunnel and the ingress actually record for a request line, and decide whether `/register` needs its query string dropped from those logs. The invite code rides in it.
- [ ] Feature 23 in the scope, the copyable configuration block for a new person, will want to sit right after registration. Worth checking whether the invite link can carry through to it later, without turning the invite into a credential path of its own.
- [ ] Consider whether an admin should be able to see how many live invites exist without opening the page, once there are enough people that the answer is not obviously small.
