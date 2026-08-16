# 0025. Emailed invites bound to an address

**Date**: 2026-08-16
**Status**: Accepted

The decision record (context, options considered, rationale) is in [rationale.md](rationale.md).

## Summary

Today an admin mints an invite and copies the link out of the admin page by hand, so getting it to the person is still a manual message. This adds an optional address to the mint form: fill it in and the platform stores the address on the invite, mails the link to it, and refuses any registration that uses a different address. Leave it blank and everything behaves exactly as it does now, which is what keeps the platform's own boot time invite working. One nullable column, one new refusal code, one new message, and one comparison inside the existing register path.

## Requirements

**User stories**:
- As the platform owner, I want to type a person's address when I mint an invite and have the platform send it, so that inviting someone is one action rather than a mint followed by a message I write myself.
- As the platform owner, I want an invite to authorise the one person I sent it to, so that a forwarded or intercepted link cannot be used by whoever ends up holding it.
- As an invited person, I want the message to arrive with a link I can click straight into registration, so that joining needs no further help from the person who invited me.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

- **AC-1**: The mint form on `/admin/invites` takes an optional address alongside the existing note. An empty address mints exactly as today: the invite is unbound, no message is sent, and the link is shown once on the page that minted it.
- **AC-2**: A filled address is validated by `CheckEmail` and normalized by `NormalizeEmail` before anything is written. A malformed address is refused `email_invalid`, mints nothing, and sends nothing.
- **AC-3**: A mint carrying an address that already has an account is refused with the new closed code `address_registered` at 409, mints nothing, and sends nothing. The account check runs inside the same transaction as the insert, so a registration landing between the two cannot produce a bound invite for an address that now has an account.
- **AC-4**: A successful bound mint writes the normalized address on the invite row and sends exactly one plain text message to it, carrying the register link, the display name of the admin who minted it, and the seven day expiry.
- **AC-5**: The send runs inline within the mint request, bounded by the mail sender's existing timeout. On success the page renders the link exactly as an unbound mint does, plus one line naming the address it was sent to.
- **AC-6**: A send failure leaves the invite minted, bound and live. The page renders the link and says the send failed so the admin can hand it over another way. The failure is logged with neither the address nor the link in the line.
- **AC-7**: With no mail sender configured, a mint carrying an address is refused `mail_unavailable` and writes nothing. A mint with an empty address still works, so the platform remains usable with no mail configured.
- **AC-8**: Registration presenting a bound invite with any address other than the bound one is refused `invite_invalid` by the same lookup, carrying the same error, the same status and the same words as an unknown, spent, revoked or expired invite. The address is part of the lookup's own predicate, so the two cases are one query and cost the same work, and no ordering of checks can tell them apart. No account is created and the invite stays live and usable.
- **AC-9**: The submitted address is normalized by `NormalizeEmail` before the lookup, and the bound address was normalized at mint, so an invite minted to `Alice@Example.com` is satisfied by a registration as `alice@example.com`.
- **AC-10**: The match happens inside the invite lookup, which is still the first statement in `Register`, ahead of `CheckEmail`, `CheckPassword` and the password hash. Normalizing an address is free, so spec 0015 AC-11 holds unchanged and a mismatched caller costs the platform no key derivation.
- **AC-11**: A registration refused for a mismatched address spends the same rate limit bucket a refused registration already spends, exactly as spec 0015 AC-17 requires of a refused invite.
- **AC-12**: A raw invite code exists in exactly three places the platform controls: the admin response that minted it, the bootstrap log line, and the one message sent to the bound address. It is still never in the database, never in another platform log line, and never in an audit row. This amends spec 0015 AC-14, which named only the first two.
- **AC-13**: No audit row carries the bound address. The issue, revoke and spend rows keep the shape spec 0015 gave them, and the invite id is the link to the address.
- **AC-14**: The invite list shows the bound address in its own column. An unbound invite renders that cell as `not sent`, and the note column is unchanged in meaning and content.
- **AC-15**: The JSON mint route accepts the same optional address, applies the same validation, the same refusals and the same inline send, so neither surface can mint an invite the other cannot.
- **AC-16**: Every existing invite keeps working unchanged, including the platform's own bootstrap invite, which is unbound forever. The migration is additive and the previous binary reads the resulting schema without harm.
- **AC-17**: There is no resend. Once the mint response is rendered the platform cannot reproduce the link, and recovering from a lost message means revoking the invite and minting a fresh one.
- **AC-18**: The register page is unchanged. `GET /register` still never validates the code and never prefills the address, so an unknown, expired, revoked, spent and valid code all render the identical form and every distinction is still made on the post (spec 0015 AC-18 holds).

## Decision

**Chosen option**: Option 1: A nullable address column on the existing invite row, compared inside `Register`.

An invite may carry one normalized address. When it does, the platform mails the link to that address at mint time and `Register` refuses any address that does not match it, using the existing `invite_invalid` refusal.

**Implementation skills**: `security-patterns` (`~/.claude/skills/security-patterns/`) · `database-migrations` (`~/.claude/skills/database-migrations/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`)

## Rationale

Reasoning and options: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

`invites` (existing table, spec 0015, migration `00003`). One additive change.

| Column | Type | Null | Change | Meaning |
|---|---|---|---|---|
| `id` | TEXT PRIMARY KEY | no | unchanged | |
| `code_hash` | TEXT NOT NULL UNIQUE | no | unchanged | the only place the code exists after the mint response |
| `note` | TEXT | yes | unchanged | the admin's own words. Still the only record of intent for an unbound invite |
| `email` | TEXT | yes | **new** | the normalized address this invite is bound to. Null means unbound, which is today's behaviour and what the bootstrap invite carries permanently |
| `created_by` | TEXT REFERENCES accounts(id) ON DELETE RESTRICT | yes | unchanged | |
| `expires_at` | TEXT NOT NULL | no | unchanged | |
| `consumed_at` | TEXT | yes | unchanged | |
| `consumed_by` | TEXT REFERENCES accounts(id) ON DELETE RESTRICT | yes | unchanged | |

No index on `email`: nothing looks an invite up by address, the lookup is still by `code_hash` and stays exactly as spec 0015 wrote it. No uniqueness on it either, so the same address may hold two live invites. That is harmless: whichever link is used first spends its own row and the other expires unspent.

`accounts` is untouched. The taken address check at mint reuses the existing `AccountByEmail`.

New migration: `internal/store/migrations/00008_invite_email.sql`, one `ALTER TABLE invites ADD COLUMN email TEXT`.

**Function and store surface** (pinned here so the build settles none of it by guesswork):

| Thing | Change |
|---|---|
| `LiveInviteByCodeHash` (sqlc query) | gains `AND (email IS NULL OR email = @candidate)` in its `WHERE`, and takes the candidate address as a second parameter. This is what makes AC-8 true: a dead code and a live code with the wrong address are one query returning one error |
| `Store.LiveInvite` / `identity.Store.LiveInvite` | takes `(ctx, codeHash, candidateEmail string)`, returns the invite id as it does now. The bound address is never returned, because nothing above needs to read it |
| `CreateInvite` | takes the address on `NewInvite` as a new `Email` field, and becomes a transaction: it reads the accounts table for that address at the top of the same `BEGIN IMMEDIATE` that inserts the row, and answers `ErrAddressRegistered` rather than inserting. This follows `CreateApp`, per the guard inside the transaction convention in `internal/store/AGENTS.md`. An empty address skips the read |
| `ListInvites` | its row and `InviteRow` gain `Email string`, empty when null. `InviteView` gains `Email string` too, and the JSON list and the page template both key it `email` |
| `Service.IssueInvite` | takes `(ctx, adminID, rawNote, rawEmail string)` |
| `IssuedInvite` | gains `Email string` (the bound address, empty when unbound) and `Sent bool` (false when unbound or when the send failed). No error text, so nothing carries the provider's words toward a page |
| `Service.sendNow` | a second send path beside the existing `send`. It calls the same `Mailer` and returns the error instead of swallowing it, and it is the only path that does. `send` is unchanged and stays the one every other message uses |
| `identity.Code` | gains `CodeAddressRegistered Code = "address_registered"`, mapped to 409 in `statusFor` on both surfaces |

**Refusal precedence at the mint**, when several apply at once, cheapest and most caller specific first: `note_too_long`, then `email_invalid`, then `mail_unavailable`, then `address_registered`. The nil mailer check precedes the account read deliberately, so a platform with no sender configured never reads the accounts table to answer a question it cannot act on.

**State transitions**: unchanged. `InviteState` is still derived from the three timestamps against the clock, and binding adds no state. A bound invite is live, spent, revoked or expired exactly as an unbound one is.

**API surface**:

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/admin/invites` | POST | `note` (opt), `email` (opt) | the page, with the link, the note, and the send outcome when an address was given | admin session plus the existing synchroniser token | 400 `email_invalid`, 400 `note_too_long`, 409 `address_registered`, 503 `mail_unavailable`, 403 `admin_required` |
| `/v1/admin/invites` | POST | `note` (opt), `email` (opt) | invite id, note, expiry, link, bound address, sent true or false | admin session | the same set |
| `/v1/admin/invites` | GET | none | the list, each row carrying its bound address or empty | admin session | 403 `admin_required` |
| `/admin/invites` | GET | none | the list page, with an address column | admin session | 403 `admin_required` |
| `/register`, `/v1/auth/register` | POST | `invite`, `email`, `password`, `name` | unchanged | none | 403 `invite_invalid` now also covers a mismatched address |

Revoke is unchanged on both surfaces.

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| mint | the stored bound address | the `email` form field, through `CheckEmail` then `NormalizeEmail` |
| mint | whether the address already has an account | a read of the accounts table inside the `CreateInvite` transaction, answering `ErrAddressRegistered` |
| mint | the invite link in the message | `inviteURL(ConsoleURL, rawCode)`, the same derivation the page already renders, so the mailed address and the shown one cannot disagree |
| mint | the inviter's name in the message | the minting admin's `DisplayName`, already loaded by `adminSession` |
| mint | the expiry sentence in the message | `InviteLifetime`, the existing seven day constant |
| mint | the send outcome shown on the page | the return value of the inline `Mailer.Send` call |
| mint | the sender address of the message | `DEPLOYER_MAIL_FROM`, unchanged, the same From every message uses |
| mint | the send outcome shown on the JSON surface | `IssuedInvite.Sent`, keyed `sent` |
| list | the address column | the `email` column, empty string when null |
| register | the match against the bound address | the `LiveInviteByCodeHash` predicate, so no address is ever returned upward to compare |
| register | the candidate address in that predicate | the request, through `NormalizeEmail` only. Full validation by `CheckEmail` still runs afterwards, for format |

**Key invariants**:
- A bound invite creates an account only for its bound address. Enforced by the lookup's own predicate, reached through `Service.Register`, the one path both register surfaces share.
- The bound address never leaves the database. Nothing reads it upward on the register path, so no handler can accidentally answer with it or branch on it.
- The bound address is immutable. It is written once at mint and no path edits it, which is why the match lives only in the lookup and not also in the spend guard, where spec 0015 put expiry because expiry can change underneath a request.
- A refused mint writes nothing and sends nothing. Note validation, address validation and the nil mailer check run before `CreateInvite`, and the taken address check runs inside it.
- Exactly one send path reports its failure, `sendNow`, and it is used only here. Every other message keeps the best effort `send`.
- A minted invite is committed before the send is attempted, so no mail failure can lose an invite. The reverse, a send with no invite behind it, is impossible for the same ordering.
- Every refusal a caller sees is still one of the closed codes in `internal/identity/identity.go`. `address_registered` joins that set; nothing else does.

**Security model**:
- Minting, listing and revoking stay admin session only, exactly as spec 0015 AC-9 set them. An API bearer token still never reaches the invite surface.
- The mismatch refusal is deliberately indistinguishable from every other bad invite. Telling a holder that the code is live but the address is wrong tells them the code is live, which is what spec 0015 AC-2 exists to prevent. This adds a sixth member to that set rather than a sixth message.
- `address_registered` is safe to say plainly because the surface is admin only. It must never leak onto the register path, where saying the same thing would be an account enumeration oracle.
- The address is personal data and lives in exactly one place, the invite row. It is not in an audit row, not in a log line, and not in an error string, which matches how `internal/mail` already keeps the recipient out of every failure it returns.
- The message carries a live credential to an address that has no account yet, which is the property this feature trades for delivery. It is bounded by the seven day lifetime, single use, and revocable from the admin page.

**Configuration required**: none. `DEPLOYER_RESEND_API_KEY`, `DEPLOYER_MAIL_FROM` and `DEPLOYER_CONSOLE_HOST` already exist and already carry everything this needs.

**Critical test scenarios**:
- Happy path: an admin mints with an address, the fake mailer records one message to it carrying the link, and registering from that link with that address creates the account and spends the invite, verifies **AC-1**, **AC-4**, **AC-5**.
- Binding: the same invite presented with a different address is refused `invite_invalid`, byte for byte identical to the refusal an expired invite gets, creates no account, and leaves the invite live and then usable by the right address, verifies **AC-8**.
- Case: an invite minted to a mixed case address is satisfied by the lowercase registration, verifies **AC-9**.
- Failure case: the mailer returns an error, the invite is still live, the page carries the link, and the log line holds neither the address nor the code, verifies **AC-6**, **AC-12**.
- Failure case: a nil mailer refuses a bound mint `mail_unavailable` and writes no row, while an unbound mint on the same platform still succeeds, verifies **AC-7**.
- Refusal: minting to an address that already has an account answers `address_registered` and writes no row, verifies **AC-3**.
- Compatibility: an invite row with a null `email`, including the bootstrap one, registers any valid address exactly as before, verifies **AC-16**.
- Ordering: a mismatched registration is refused without a password hash being computed, verifies **AC-10**.
- Both surfaces: the JSON mint accepts an address and refuses the same four ways the page does, verifies **AC-15**.
- Unchanged: `GET /register` renders the identical form for a bound live code and an unknown one, verifies **AC-18**.

## Build plan

Tracer Bullet, so the second task is one bound invite travelling the whole distance, from a typed address through a real message to a registration the binding decides. The refusals and both surfaces thicken it afterwards.

1. The migration and the store: `00008_invite_email.sql`, the sqlc regeneration, `CreateInvite` carrying an address, `LiveInviteByCodeHash` gaining the candidate address in its predicate and `LiveInvite` its second parameter, and `ListInvites` carrying the address up. Additive only, satisfies **AC-16**.
2. The thin thread: `IssueInvite` takes an address, binds it, composes the message and sends it through `sendNow`; `Register` normalizes the submitted address and passes it to the lookup; one bound invite proven from mint to account, and one proven refused for the wrong address by the same query, satisfies **AC-1**, **AC-4**, **AC-8**, **AC-9**, **AC-10**.
3. The refusals: `address_registered` added to the `Code` set with its 409, the account read inside the `CreateInvite` transaction, the nil mailer refusal, `CheckEmail` at the mint, the documented precedence between the four, and the send failure path that keeps the invite and reports the failure, satisfies **AC-2**, **AC-3**, **AC-6**, **AC-7**.
4. Both surfaces: the address field and the send outcome line on the admin page, the address column in the list, and the same address parameter and outcome on the JSON mint route, satisfies **AC-5**, **AC-14**, **AC-15**.
5. The properties that must not regress: the shared rate limit bucket, the audit rows staying free of the address, the raw code appearing in exactly three places, the register page rendering identically for every code, and no resend path existing, satisfies **AC-11**, **AC-12**, **AC-13**, **AC-17**, **AC-18**.

## Migration plan

**Strategy**: additive, one deployment, no backfill.
**Phases**:
1. `ALTER TABLE invites ADD COLUMN email TEXT` runs at startup with the rest of the goose migrations. Every existing row reads as unbound, which is exactly what those invites are.
2. The new binary writes the column only when an admin types an address.

**Rollback**: revert the commit. The previous binary never selects `email`, so it reads the migrated schema unharmed, and any invite bound by the new binary degrades to an ordinary unbound invite rather than breaking.
**Risks**: low. The only live data change is a nullable column with no default and no constraint. The behavioural risk sits in the lookup predicate, which every registration now runs: `email IS NULL OR email = @candidate` must keep unbound invites matching every address, and a predicate written as a plain equality would silently break every invite that already exists. That is what the compatibility test scenario exists to catch, and it is the one line in this build worth reading twice.

## Consequences

**Positive**:
- Inviting someone becomes one action, and the person needs nothing from the admin afterwards.
- A leaked or forwarded link stops being an account. The invite authorises a named person, so the blast radius of a mistyped or intercepted message is one refused registration rather than a stranger on the platform.
- The unbound path survives intact, so the boot time bootstrap invite and any hand delivered link keep working with no special case.

**Negative / tradeoffs**:
- A live credential now travels by mail, which spec 0015 AC-14 deliberately avoided. Anyone who can read the mailbox can take the account, and mail is not a confidential channel.
- The mint request now blocks on an outside service. A slow Resend makes the admin page slow, bounded only by the ten second send timeout, which no other page has ever waited on.
- Personal data enters the invite table. The address of someone who never registered sits there until the row is deleted, and nothing deletes invite rows today.
- No resend means a message lost to a spam filter costs a revoke and a fresh mint, and the admin cannot tell a message that never arrived from one that arrived and was ignored.
- A sixth reason answers `invite_invalid`, so the message an invited person gets when they mistype their own address is unhelpful by design and they have no way to work out why.

**Neutral**:
- The mail path gains its first message that is not about an existing account, so `messages.go` grows a body whose recipient is a stranger.
- The mint path gains its first read of the accounts table, for the taken address check.
- Two mail behaviours now exist: verify and reset stay best effort and swallow a failure into the log, this one waits and reports. The difference is that a person is on the page and can act on the answer.

## Follow-up

- [ ] Spec 0015 AC-14 is amended by AC-12 here. A pointer is added to that spec's criterion, and the wording in `internal/identity/invites.go` that says the raw code is never mailed needs updating in the same commit as this build.
- [ ] A liveness oracle already exists on the register path and this spec does not close it. A malformed address submitted with a live unbound invite answers `email_invalid`, while the same submission with a dead code answers `invite_invalid`, so a holder can learn whether their code is live without a valid address. It predates this feature, it is unchanged by it, and closing it means moving `CheckEmail` behind the invite lookup, which is its own decision about spec 0015 AC-1. Worth its own pass.
- [ ] Nothing deletes invite rows, so a spent or expired invite keeps a person's address indefinitely. Worth a retention decision once the platform holds more than a handful of accounts; out of scope here.
- [ ] The invite message is the platform's first mail to someone with no account. If deliverability to strangers turns out to be poor, that is a Resend domain and reputation question rather than a change to this design.
