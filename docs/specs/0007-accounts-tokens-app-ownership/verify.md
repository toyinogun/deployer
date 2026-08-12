# Verify: accounts, API tokens and app ownership · spec 0007 · updated 2026-08-12
_Steps derived from spec 0007 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

Every step below is a curl against the running platform. Set these once:

```bash
export BASE=https://deployer.tail62ceef.ts.net
export JAR=/tmp/deployer-cookies.txt
```

## Commands

### Registration and verification
- [x] `curl -sS -o /dev/null -w '%{http_code}\n' -X POST $BASE/v1/auth/register -H 'content-type: application/json' -d '{"email":"you@yourdomain.example","password":"a long enough password"}'` → `202`, and the verification mail arrives → AC-1
- [x] Register the same address again and diff both responses byte for byte → identical status and body; the mailbox receives a "someone tried to register" message with no link in it → AC-2
- [x] `-d '{"email":"you@yourdomain.example","password":"tooshort"}'` → `422` with code `password_too_short`; `-d '{"email":"not-an-address","password":"a long enough password"}'` → `422` with code `email_invalid`; a 12 character all lower case password is accepted → AC-3
- [x] On a database holding only the bootstrap account, the first registration comes back `is_admin: true` from `/v1/auth/me` and the second comes back `false` → AC-4
- [x] `curl -sS "$BASE/v1/auth/verify?token=<from the mail>"` → `200 {"verified":true}`. Repeat it, try a made up token, and try a reset link on the same endpoint → all three answer `400 link_invalid` in identical words → AC-5
- [x] `POST /v1/auth/resend` then click the older link → `400 link_invalid`; the newer one → `200` → AC-6

### Sessions
- [x] `curl -sS -c $JAR -X POST $BASE/v1/auth/login -d '{"email":"you@yourdomain.example","password":"a long enough password"}'` → `200` with email, name and admin flag; inspect `$JAR` and the `Set-Cookie` header for `HttpOnly`, `SameSite=Lax` and `Secure` (Secure only because `DEPLOYER_PUBLIC_URL` is https) → AC-7
- [x] Wrong password, an address nobody registered, and a disabled account → all `401 credentials_invalid` with identical bodies. A registered but unverified account → `403 email_unverified` → AC-8
- [x] `curl -sS -b $JAR -X POST $BASE/v1/auth/logout` → `204` with a `Max-Age=0` cookie; the next `/v1/auth/me` on the same jar → `401` → AC-9
- [x] Sign in, then disable that account from an admin session, then reuse the cookie → `401` → AC-10
- [x] `POST /v1/auth/login -d '{"email":"bootstrap","password":"anything at all"}'` → `401 credentials_invalid`, and time it against a wrong password on a real account: the two are comparable, not one fast and one slow → AC-11

### Tokens
- [x] `curl -sS -b $JAR -X POST $BASE/v1/tokens -d '{"name":"agent"}'` → `201` carrying `token` once, prefixed `dpl_`. `expires_in_days: 1` and `365` are accepted; `0` means never; `366` and `-1` → `422 invalid_expiry` → AC-12
- [x] `curl -sS -b $JAR $BASE/v1/tokens` → newest first, with prefix, created, last used and expiry, and no raw value or hash anywhere in the body. Sign in as a second account: its list is empty → AC-13
- [x] `DELETE /v1/tokens/{id}` on your own → `204`, and the token is refused on the very next MCP call. The same call against another account's token id and against a made up id → identical `404` bodies → AC-14
- [x] Mint from an unverified account → `403 email_unverified`. Registering and stopping there cannot reach this, because sign in is refused first, so the route is: verify, sign in, reverse the verification in the database, then mint on the session you still hold → AC-15

### Ownership and the verified gate
- [x] With a live token, `UPDATE accounts SET email_verified_at = NULL` for that account, then call any MCP tool and the upload endpoint → both refused exactly as an invalid token is. The bootstrap account, which has no email, keeps working → AC-16
- [x] With two registered accounts A and B: B calls `deploy_app` on A's app name, `deployment_status` on A's deployment id, and `get_logs` on A's app → refused with `app_unknown`, `deployment_unknown` and `upload_invalid` as appropriate, and `SELECT * FROM audit_log WHERE account_id = '<B>'` has a row for each → AC-17
- [x] A and B each deploy an app called `checkout` → both succeed and the two hostnames differ, because the slug carries a random suffix → AC-18

### Admin
- [x] `curl -sS -b $ADMIN_JAR $BASE/v1/admin/accounts` → every account, newest first, with email, name, verified, admin and disabled. `POST .../{id}/disable` and `/enable` → `204` each → AC-19
- [x] The same list from an ordinary account's session → `403 admin_required`. The same list with `Authorization: Bearer <API token>` and no cookie → `401` → AC-20
- [x] From the admin's own session and token, read another account's app status and logs → refused, the same as any other account → AC-21
- [ ] After a mint, a revoke, an admin disable and a failed sign in, `SELECT DISTINCT action FROM audit_log` contains `token_mint`, `token_revoke`, `admin` and `login` → AC-22

### Rate limiting and failure
- [x] Six wrong passwords in a row for one address → the sixth is `429 rate_limited`; wait 30 seconds and sign in correctly; a fresh wrong password is `401` again, not `429` → AC-23
- [x] Eleven `POST /v1/auth/forgot` calls from one client → the eleventh is `429`; a different client address is still served; roughly 6 seconds later the first client is served again → AC-24
- [x] Break the Resend key so sending fails, then register → still `202`, the account exists in the database, and the platform log carries an error level line → AC-25
- [x] Unset `DEPLOYER_RESEND_API_KEY` and restart → register, resend and forgot answer `503 mail_unavailable`; an upload and a full deploy still work → AC-26
- [x] Grep every response body, the platform log, and `SELECT * FROM audit_log` for the password, the session cookie value, and the raw API token → no hit anywhere except the single mint response → AC-27

### Password reset
- [x] `POST /v1/auth/forgot` for a real address and for one nobody registered → identical `202` bodies; only the real one receives mail → AC-28
- [x] `POST /v1/auth/reset -d '{"token":"<reset link>","password":"an entirely different password"}'` → `204`; every previous session is `401`; the old password no longer signs in; the new one does. Present a `verify_email` token to the same endpoint → `400 link_invalid` → AC-29

## Value sourcing
One step per row of the spec's Value sourcing table, exercising the edge that breaks if the source is wrong.

- [x] `SELECT id, name, display_name FROM accounts WHERE email IS NOT NULL` → `name` equals `id` on every row, and `display_name` is the human label. Register two addresses `info@a.example` and `info@b.example` with no `name` field → both succeed, both get `display_name` `info`, no unique constraint error reaches the caller
- [x] Register with no `name` field → `display_name` is the local part before the `@`. Register with one → it is used verbatim, trimmed
- [x] `SELECT purpose, token_hash FROM email_tokens` → present a live `verify_email` hash to `/v1/auth/reset` and a live `password_reset` hash to `/v1/auth/verify` → both `400 link_invalid`, proving the lookup matches on purpose and not on hash alone
- [x] `SELECT is_admin FROM accounts WHERE email IS NOT NULL ORDER BY created_at` → exactly one `1`, and it is the oldest row. The bootstrap account's row is `0` and has a null email
- [x] Read a verification mail → the link starts with `DEPLOYER_PUBLIC_URL`, not `DEPLOYER_INTERNAL_URL`. Paste it into a browser on the tailnet: it resolves. This is the one that breaks if the wrong variable is used, because cluster DNS is invisible from a laptop
- [x] Check the `From` header on a received message → matches `DEPLOYER_MAIL_FROM`
- [x] `SELECT token_hash FROM sessions` → no row equals the cookie value in `$JAR`; hash the cookie with SHA-256 and it does match
- [x] Point `DEPLOYER_PUBLIC_URL` at an `http://` address and restart → the session cookie comes back without `Secure`. Point it back at `https://` → `Secure` returns
- [x] Call `/v1/auth/me` once a day for three days, then read `SELECT expires_at FROM sessions` → the expiry moved forward each day rather than staying pinned to the sign in
- [x] Mint with `expires_in_days: 1` → `SELECT expires_at FROM api_tokens` is 24 hours on, not 24 days or null
- [x] Use a minted token on an MCP call → `SELECT last_used_at FROM api_tokens` moves; the token list shows it
- [x] Call every session endpoint with a body or query naming another account's id → the answer is always about the caller's own account, never the one they named
- [x] Send `X-Forwarded-For: 10.0.0.1, 100.64.0.5` and then `X-Forwarded-For: 10.0.0.1, 100.64.0.9` → the two draw on separate buckets. Send both with no header at all through the ingress → they share one bucket, which is the failure mode this row exists to catch

## Run notes, 2026-08-12

Run against the live platform on the tailnet, image
`ghcr.io/toyinogun/deployer@sha256:d7828485`, with three throwaway accounts on
mail.tm mailboxes so the links could be read. What the ticks above do not say on
their own:

- **AC-15 failed on the day and was fixed the same session.** Minting from an
  unverified account answered `401 credentials_invalid`. `identity.MintToken`
  carried the `email_unverified` branch, but the session gate in `internal/auth`
  refused first and collapsed every reason into one, so that branch could not be
  reached over HTTP. The gate now splits the two: a disabled account stays
  indistinguishable, an unverified one is told which refusal it is, on every
  session endpoint rather than just the mint route. `/v1/auth/me` on an
  unverified session therefore answers `403 email_unverified` where it used to
  answer `401`, which feature 14 will want to know. Proved by the local suite,
  not yet re run against the cluster: that needs a CI build and a deploy.
- **AC-22 is unfinished.** `token_mint`, `token_revoke` and `login` were read out
  of `audit_log`; the `admin` rows were written after the last database window
  closed and are still unread.
- **AC-24, the "different client" half**, reads 429 through the ingress, because
  nginx appends the real client address and `clientAddress` correctly takes the
  last hop, so a spoofed prefix changes nothing. That is right, not a bug: the
  separate bucket claim is provable only on a direct call, and it was, over a
  port forward (11 calls as `100.64.0.5` ended in 429 while `100.64.0.9` was
  still served). Worth rewording that row.
- **Failed sign ins record a null `account_id`**, even for an address that
  exists. Defensible next to the enumeration rules, but AC-22 says the row names
  the acting account, so it is worth a decision rather than a silent difference.
- Two rows were proved by a shorter route than they ask for: the rolling session
  expiry by watching `expires_at` move forward on use rather than over three
  days, and the verification link by fetching it over the tailnet with curl
  rather than pasting it into a browser.

## Acceptance-criteria coverage
AC-1 through AC-29 are each covered by at least one step above, tagged inline. AC-16, AC-17 and AC-21 additionally need two real registered accounts and a real cluster, so they are the ones worth running by hand even though the suite covers them against a fake.
