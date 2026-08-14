# Verify: invite only registration · spec 0015 · updated 2026-08-14
_Steps derived from spec 0015 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

Two accounts make this readable: an **admin** (the first account the platform ever registered) and an **ordinary** one. `$BASE` is the platform's public address.

## UI / manual

- [ ] Sign in as the admin → the sidebar shows **Invites** under Administration → open it → AC-9
- [ ] On `/admin/invites`, issue an invite with the note `for Sam` → the link is shown once in a highlighted panel → AC-6
- [ ] Reload `/admin/invites` → the link is gone from the page, the row is still there showing `for Sam`, the admin's name as issuer, an expiry seven days out, and state **live** → AC-6, AC-8, AC-14
- [ ] Open the invite link in a private window → an ordinary registration form, with the code in a hidden field → AC-3
- [ ] Register through it → land on check your mail → back on `/admin/invites` the row now reads **spent by** that address → AC-3, AC-8
- [ ] Visit `/register` with no code at all → the page renders normally and says registration is by invitation → AC-16
- [ ] Visit `/register?invite=` with each of: the spent code, a revoked code, an expired code, a made up string, and a live code → all five render the identical form, none of them says anything about the code → AC-18
- [ ] Submit `/register` with no invite, then with each dead code → every one is refused with the same sentence, and no account appears on `/admin/accounts` → AC-1, AC-2
- [ ] Issue an invite, then revoke it → it stays on the list marked **revoked** rather than disappearing → registering through its link is refused → AC-7
- [ ] Revoke that same invite again, and revoke a spent one → both refused, nothing on the list changes → AC-7
- [ ] Issue an invite with a note longer than 200 characters → refused, nothing minted → AC-6
- [ ] Sign in as the ordinary account → `/admin/invites` is refused, and so is a hand posted mint and revoke → AC-9
- [ ] Register on an address that already has an account, using a live invite → the answer is identical to a fresh registration, no account is created, and that same link still works on a free address → AC-10
- [ ] Spend a rate limit bucket with refused registrations carrying no invite → the endpoint starts answering `rate_limited`, the same bucket a refused registration already spent → AC-17

## Commands

- [ ] `curl -si $BASE/register?invite=anything | grep -i referrer-policy` → `Referrer-Policy: no-referrer` → AC-14
- [ ] `curl -si -XPOST $BASE/v1/auth/register -d '{"email":"x@y.z","password":"a long enough password"}'` → 403 `invite_invalid` → AC-1
- [ ] Repeat the above with an unknown, a spent, a revoked and an expired code → all four answer 403 `invite_invalid` in the same words as the missing one, and identically to what the page surface answers → AC-2
- [ ] `curl -si -XPOST $BASE/v1/auth/register -d '{"invite":"","email":"not-an-address","password":"short"}'` → 403 `invite_invalid`, not a validation code, so the gate was not spoken past → AC-11
- [ ] `curl -s -XPOST $BASE/v1/admin/invites -d '{"note":"for Sam"}' -b "$ADMIN_COOKIE"` → 201 with a `link` field → AC-6
- [ ] `curl -s $BASE/v1/admin/invites -b "$ADMIN_COOKIE"` → the list carries no raw code anywhere in the body → AC-8, AC-14
- [ ] `curl -si $BASE/v1/admin/invites -H "Authorization: Bearer $API_TOKEN"` → 401, because these read a session and a token is not one → AC-9
- [ ] `kubectl logs -n deployer deploy/deployer | grep -c "$RAW_CODE"` for a code minted through the admin surface → 0 → AC-14
- [ ] `SELECT * FROM audit_log WHERE target_type = 'invite' ORDER BY created_at DESC LIMIT 10` → one row each for issue, revoke, and a refused revoke carrying its reason; the spend names the invite and the account it created → AC-15
- [ ] `SELECT code_hash FROM invites` → every value is a hash, no raw code is stored → AC-14
- [ ] Against a **fresh empty** database: start the platform → one info line carrying a `/register?invite=` link → restart it three times → no further line, and `SELECT count(*) FROM invites` is 1 → AC-13
- [ ] Register through that bootstrap link → `SELECT is_admin FROM accounts WHERE email IS NOT NULL` is 1, so the first account is the admin → AC-13
- [ ] Roll the image back to the previous digest against a database carrying `invites` → the platform starts, existing accounts, sessions and tokens still work, and registration returns to open → AC-12

## Value sourcing checks

One per row of the spec's Value sourcing table, each varying the input that breaks if the source is wrong.

- [ ] The link an admin is shown starts with `DEPLOYER_PUBLIC_URL`, never `DEPLOYER_INTERNAL_URL`: change the public URL, mint again, and the link follows it → a browser cannot resolve cluster DNS
- [ ] `expires_at` is exactly seven days past the mint, not 24 hours: `SELECT created_at, expires_at FROM invites ORDER BY created_at DESC LIMIT 1`
- [ ] An invite minted by an admin lists that admin's display name; the boot time one lists **the platform**: mint one of each and compare the issuer column
- [ ] State is derived, never stored: there is no status column, and an invite one second past its expiry lists as **expired** with no write having happened to it
- [ ] "Spent by" is the address of the account the invite actually created: register two people through two invites and confirm each row names the right one
- [ ] The invite id spent is the one the submitted code hashes to: mint two invites, register through the second, and confirm the first is still live
- [ ] A refused registration and a refused revoke each show the closed reason code, never a wrapped error string, on both the page and the JSON surface

## Acceptance-criteria coverage

- AC-1 · no invite refused on both surfaces, no account row · covered by the manual submit step and the first curl
- AC-2 · four dead codes read identically · covered by the manual sweep and the repeat curl
- AC-3 · a valid invite registers and is stamped · covered by the invite link walk through
- AC-4 · one invite makes at most one account under a race · covered by `TestOneInviteMakesOneAccountUnderRace` (store, real SQLite, `-race`); not manually reproducible
- AC-5 · seven day expiry enforced in the spend guard too · covered by the expired code steps and `TestEveryDeadInviteReadsTheSame`
- AC-6 · issue with a bounded note, link shown once · covered by the issue, reload and over long note steps
- AC-7 · revoke, and a refused revoke on a spent or expired one · covered by the two revoke steps
- AC-8 · the list shows note, issuer, expiry and one state · covered by the reload step and the JSON list
- AC-9 · admin session only, no bearer token · covered by the ordinary account steps and the bearer curl
- AC-10 · a taken address answers identically and leaves the invite live · covered by the taken address step
- AC-11 · checked before the password hash · covered by the bad everything curl
- AC-12 · existing accounts untouched, additive migration, old binary reads it · covered by the rollback step
- AC-13 · bootstrap mints once, only on empty · covered by the fresh database and first admin steps
- AC-14 · a raw code exists in exactly two places, and `Referrer-Policy` · covered by the log grep, the `code_hash` query, the list body and the header curl
- AC-15 · audit rows for issue, revoke, refused revoke and spend · covered by the `audit_log` query
- AC-16 · a bare `/register` says it is invite only · covered by the no code visit
- AC-17 · a refused invite spends the register bucket · covered by the rate limit step
- AC-18 · `GET /register` never validates · covered by the five code render comparison
- AC-19 · the synchroniser token on both mutations · covered by the hand posted mint and revoke
