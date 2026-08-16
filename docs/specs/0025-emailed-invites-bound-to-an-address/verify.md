# Verify: emailed invites bound to an address · spec 0025 · updated 2026-08-16
_Steps derived from spec 0025 acceptance criteria and its Value sourcing table. `/check verify` runs these; `/test` locks the durable ones._

## UI / manual

- [x] Sign in as an admin, visit `/admin/invites`, submit the mint form with a note and an empty address → the link is shown once, no message is sent, the list row reads `not sent` → AC-1
- [x] Mint with the address `Sam@Example.Test` → the page shows the link plus one line naming `sam@example.test`, and exactly one message arrives at that address → AC-4, AC-5, AC-9
- [x] Read that message → it carries the register link, the minting admin's display name, and the seven day expiry → AC-4
- [x] Open the mailed link in a clean browser and register as `sam@example.test` → the account is created and the invite becomes `spent` → AC-1, AC-4
- [x] Open the same link and register as `mallory@example.test` → refused with the invite only sentence, byte for byte the same page and status an unknown code gets, no account created → AC-8
- [x] Reload `/admin/invites` after that refusal → the invite is still `live` → AC-8
- [x] Mint with `not an address` → refused `email_invalid`, no new row in the list, no message → AC-2
- [x] Mint with an address that already has an account → refused at 409 `address_registered`, no new row, no message → AC-3
- [x] With the Resend key deliberately wrong so sends fail, mint with an address → the page shows the link and says the send failed, the invite is live and bound, and that link still registers that address by hand → AC-6
- [x] Check the platform log for that failed send → the line carries neither the address nor the invite code → AC-6, AC-12
- [x] Visit `/admin/invites` with both a bound and an unbound invite listed → the address has its own column, the unbound row's cell reads `not sent`, the note column is unchanged → AC-14
- [x] Visit `GET /register?invite=<a live bound code>` and `GET /register?invite=nonsense` → both render the identical form, neither validates → AC-18
- [x] Look for a resend control anywhere on `/admin/invites` → there is none; recovering a lost message means revoke plus a fresh mint → AC-17

## Commands

- [x] `curl -X POST /v1/admin/invites -d '{"note":"x","email":"Sam@Example.com"}'` with an admin cookie → 201 carrying `email` normalized to lowercase, `sent: true`, and the link → AC-15, AC-9
- [x] The same call with `"email":""` → 201, `email` empty, `sent: false`, no message → AC-1, AC-15
- [x] The same call with a malformed address → 422 `email_invalid`; with a registered address → 409 `address_registered` → AC-2, AC-3, AC-15
- [x] `curl GET /v1/admin/invites` → each row carries `email`, empty on the unbound ones → AC-14
- [x] Unset `DEPLOYER_RESEND_API_KEY`, restart, then mint with an address → 503 `mail_unavailable` and no row written; mint with an empty address → 201 → AC-7
- [x] Against a database holding invites minted before this deploy, including the bootstrap one, register with any valid address → it still works → AC-16
- [x] `sqlite3 <db> "select email from invites"` after a bound mint → the stored value is the normalized address, and no other table holds it → AC-4, AC-13
- [x] `sqlite3 <db> "select * from audit_log order by occurred_at desc limit 5"` after a bound mint, revoke and spend → no row carries the address; the invite id is the only link to it → AC-13
- [x] Post a mismatched address with a deliberately short password → the answer is the invite refusal at 403, never the password message, so the gate ran first → AC-10
- [x] Post mismatched registrations repeatedly from one address → the rate limit bucket is spent exactly as a refused registration spends it → AC-11
- [x] `grep` the platform logs after a whole bound mint and registration cycle → the raw code appears in no log line other than a bootstrap mint → AC-12
- [x] Roll back to the previous image against the migrated database → it reads the schema and serves invites unharmed → AC-16

## Acceptance-criteria coverage

- AC-1 unbound mint unchanged · AC-2 malformed address · AC-3 registered address at 409 · AC-4 one message with link, inviter and expiry · AC-5 inline send and the address line · AC-6 send failure keeps the invite · AC-7 nil mailer · AC-8 wrong address indistinguishable · AC-9 normalization · AC-10 gate before the hash · AC-11 shared rate limit bucket · AC-12 the raw code in three places · AC-13 audit rows free of the address · AC-14 the address column · AC-15 both surfaces · AC-16 existing invites and rollback · AC-17 no resend · AC-18 the register page unchanged
