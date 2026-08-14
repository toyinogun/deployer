# Verify: web interface · spec 0013 · updated 2026-08-14
_Steps derived from spec 0013 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

## UI / manual

- [ ] Seal `deploy/web-sealedsecret.yaml` against the cluster, sync, and confirm the pod starts. Remove the Secret and confirm it refuses to start rather than serving unverifiable forms → AC-1, AC-12
- [x] Visit `/` signed out → redirected to `/login`. Sign in, visit `/` again → redirected to `/apps` → AC-2
- [x] Visit `/apps` signed out → `302`/`303` to `/login?next=/apps`, and signing in lands back on `/apps` → AC-2
- [x] Sign in with `next=//evil.test` and with `next=https://evil.test` → both land on `/apps`, never off site → AC-2
- [x] Send a request to `/apps` carrying a valid bearer API token and no session cookie → treated as signed out → AC-3
- [x] Register a new address, click the link in the real email → lands on a page, not JSON, and the URL is `/verify`, not `/v1/auth/verify` → AC-6, AC-7, AC-10
- [x] Click the same verification link twice → the second renders the shared link invalid page with a resend action; an expired and an unknown token render it in the same words → AC-7
- [x] Sign in as a registered but unverified account → the unverified page shows the address, a resend button, and the three an hour note. Press resend → a fresh link arrives → AC-8
- [x] Complete forgot and reset in the browser. `POST /forgot` with an address that does not exist renders the same confirmation as one that does → AC-9
- [x] Sign out → cookie cleared, redirected to `/login`, and the back button does not show the previous page's data → AC-11
- [x] Open a form, edit the hidden `csrf` value in devtools, submit → `403`, nothing changed, and an audit row is written → AC-12
- [x] Submit a page POST from a page on another origin → `403` before the handler runs → AC-13
- [ ] With 21 or more apps on the account, `/apps` shows twenty and a **Load more** control that appends the next page. Confirm no app of another account appears and a deleted app does not → AC-14
- [x] On an app whose most recent deploy failed: the list row and the overview both show the release still being served **and** the failure → AC-14, AC-15
- [x] Open another account's app slug on all four app pages → the same not found page an unknown slug gives, with an audit row → AC-15
- [x] Start a real deploy and watch the overview → the status region refreshes itself every three seconds and stops when the deploy ends, without a reload → AC-16
- [x] Revoke the session in the database while the overview is polling → polling stops, the page is left as it stands, and the next navigation lands on sign in → AC-16
- [x] Load the overview with JavaScript disabled → the correct current state renders → AC-16, AC-30
- [ ] Check a `superseded` deployment → renders as cancelled, not as a failure. Check each reason code renders its written sentence with the raw code beside it → AC-17
- [x] After a rollback, `/apps/{slug}/releases` marks the release actually being served, not the newest row → AC-18
- [x] Open `/apps/{slug}/logs` for a running app → recent output in the dark pane, redacted. For an app whose namespace is not readable yet → the explanatory empty state, not an error → AC-19
- [x] On `/apps/{slug}/config`: a secret key shows no reveal control; a non secret key reveals on demand and writes an audit row naming account, app and key → AC-20
- [x] Mint a token → shown once with a copy control and a warning. Reload → it is gone and never reappears → AC-21, AC-22
- [x] Revoke a token → it stops authenticating on the next request. Try another account's token id → not found → AC-23
- [x] As a non admin, open `/admin/accounts` → the `403` page. Signed out → redirected to `/login` → AC-24
- [x] As an admin: disable requires typing the target's address, enable works, and a foreign token revoke works. Each writes an audit row naming both accounts → AC-25
- [x] A verified account with zero apps sees the onboarding panel with the MCP and upload endpoints and a mint a token link. Check every list has a written empty state → AC-26
- [x] Compare the shell against `design2.webp` and row density against `design.webp` → AC-27
- [x] Navigate between pages → cross document transitions, lists enter staggered. Turn on reduce motion at the OS level → every transition and animation is off and nothing shifts → AC-28
- [x] At 375px: the sidebar collapses behind the toggle, tables become stacked cards, and nothing overflows sideways at any width → AC-29
- [x] Tab through every page → focus always visible and every action reachable. With JavaScript off every page renders and every form submits → AC-30

## Commands

- [x] `go test -race ./...` → passes → AC-4
- [x] `golangci-lint run` → clean → AC-4
- [ ] `ko build ./cmd/deployer` → builds with no node toolchain and no extra layer → AC-1
- [x] `grep -rn "ListConfigForDeploy" internal/ --include='*.go' | grep -v _test` → exactly two callers, neither in `internal/web` → AC-20
- [x] Crawl every page as a signed in account and assert no response body carries the session cookie value, a raw token, or a secret configuration value; check the same against the platform log at info level → AC-31

## Value sourcing

- [x] The CSRF token changes when the session changes and stops verifying the moment that session is revoked → AC-12
- [x] The hostname on the list and overview is the slug joined with `DEPLOYER_APP_DOMAIN`; change the domain and both follow → AC-14, AC-15
- [x] The onboarding endpoints are built from `DEPLOYER_PUBLIC_URL`; change it and both follow → AC-26
- [x] The serving release comes from the app's own current release, not the latest deployment's: on an app whose last deploy failed, the digest shown is the one actually running → AC-15
- [x] The never deployed state appears only for an app nothing has ever deployed, not for one whose only deploy failed → AC-15
- [x] Polling starts only when the last deployment is non terminal, decided from the same terminal set `internal/domain` defines → AC-16
- [x] The releases page marks current from the serving release, so after a rollback the marked row is not the newest → AC-18
- [x] Logs redaction matches on the running release's configuration for keys that are secret today: rotate a secret, then confirm the old value is still redacted out of the running pod's output → AC-19
- [x] The reveal returns a value only for a key `ListConfigForResponse` returns one for → AC-20
- [x] The token panel's raw value comes from the mint call's return and appears in that one response body only → AC-22

## Acceptance-criteria coverage

- AC-1 sealed secret and ko build steps · AC-2 root, gated path and next steps · AC-3 bearer step · AC-4 test, lint and layering steps · AC-5 sign in step, plus lockout and bucket through the same service call · AC-6, AC-7 register and link steps · AC-8 unverified step · AC-9 forgot and reset step · AC-10 mailed link step · AC-11 sign out step · AC-12 csrf steps · AC-13 origin step · AC-14 paging and failed deploy steps · AC-15 overview, ownership and sourcing steps · AC-16 polling, revoked session and no script steps · AC-17 reason sentence step · AC-18 releases steps · AC-19 logs and redaction steps · AC-20 config, reveal and two caller steps · AC-21, AC-22, AC-23 token steps · AC-24, AC-25 admin steps · AC-26 onboarding step · AC-27 design comparison step · AC-28 motion step · AC-29 375px step · AC-30 keyboard and no script steps · AC-31 crawl step
