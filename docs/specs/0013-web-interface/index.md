# 0013. Web interface: server rendered pages over the identity and app surfaces

**Date**: 2026-08-13
**Status**: In Progress

## Summary

Deployer has people, tokens, apps, releases and logs, and no way to see any of it without curl or an agent. This spec adds the browser: pages rendered by the Go binary itself from `html/template`, with the templates, one stylesheet and one small script embedded in the binary, so nothing about the build or the deploy changes. The pages are read only for apps: you can see everything, and every action that changes an app stays with your coding agent over MCP. Identity is fully drivable in the browser, because register, verify, sign in, reset and token minting are things a person does and a machine does not.

## Requirements

**User stories**:
- As a person who registered, I want to click the link in my email and land on a real page so that the first thing your platform shows me is not a JSON blob.
- As an app owner, I want to open a browser and see my apps, their state, their releases and their recent output so that I can tell what my agent did without asking it.
- As an app owner, I want to mint an API token in a browser and copy it once so that pointing a new agent at the platform does not need curl.
- As the platform owner, I want to see who registered and disable an account from a page so that the admin endpoints I already built are usable.
- As a person on my phone on the tailnet, I want to check whether a deploy went green so that I do not need a laptop to know.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

Shell, access and rendering
- **AC-1**: Every page is rendered server side by the deployer binary from `html/template`. Templates, the stylesheet and the single script are embedded with `go:embed`. No node toolchain enters the repo, `ko build` is unchanged, and the image gains no layer beyond the binary.
- **AC-2**: `GET /` redirects to `/login` without a live session and to `/apps` with one. Every page under `/apps`, `/tokens` and `/admin` requires a live session. Without one it answers `302` to `/login?next=<path>`, and a successful sign in follows `next` only when it is a local path beginning with exactly one `/`; anything else lands on `/apps`.
- **AC-3**: Pages authenticate with the session cookie only. A request carrying a bearer API token instead of a session is treated as signed out, matching AC-20 of spec 0007. The `/v1` and `/mcp` surfaces are untouched by this feature and keep answering exactly as their specs define.
- **AC-4**: Every page handler is a thin wrapper over the same `internal/identity` service calls and the same `internal/store` methods the JSON and MCP surfaces already use. No page reaches the database or Kubernetes through a path of its own, so a rule, a rate limit or an audit row cannot differ between the browser and the agent.

Identity pages
- **AC-5**: `GET /login` renders the form and `POST /login` signs in. Wrong credentials re render the form with the single generic message that matches `credentials_invalid`, and the lockout of AC-23 and the shared bucket of AC-24 in spec 0007 apply identically, because the same service call runs.
- **AC-6**: `GET /register` and `POST /register` register. A successful post renders a check your email page, and a duplicate address renders that identical page, so the browser leaks no more than AC-2 of spec 0007 does.
- **AC-7**: `GET /verify?token=…` consumes the link and renders a verified page with a sign in action. A consumed, expired, unknown or wrong purpose token renders one shared link invalid page carrying a resend action, in the same words for all four.
- **AC-8**: Signing in to a registered but unverified account renders a dedicated unverified page showing the address, a resend button, and a note that resend is limited to three an hour, so hitting that limit is not a mystery. `POST /resend` from that page issues a fresh link.
- **AC-9**: `GET /forgot`, `POST /forgot`, `GET /reset?token=…` and `POST /reset` complete the password reset flow in the browser. `POST /forgot` renders the same confirmation whether or not the address exists.
- **AC-10**: The verification and password reset emails link to the page URLs, not to `/v1/auth/…`. The `/v1` JSON endpoints keep answering exactly as spec 0007 defined them and stay drivable with curl.
- **AC-11**: `POST /logout` revokes the current session, clears the cookie, and redirects to `/login`.

CSRF and origin
- **AC-12**: Every state changing POST made by a signed in person carries a hidden `csrf` field whose value is the hex HMAC SHA256 of the session id under `DEPLOYER_CSRF_KEY`. The server recomputes and compares it in constant time. A missing or wrong value answers `403`, changes nothing, and writes an audit row.
- **AC-13**: Every POST to a page path is refused `403` when the `Origin` header is present and is not the platform's own origin, and when `Sec-Fetch-Site` is present and is neither `same-origin` nor `none`. A request carrying neither header passes this check, and that is correct rather than a hole: every browser that can perform a cross site form post sends `Sec-Fetch-Site: cross-site`, and a scripted client that omits both headers carries no session cookie either, so it is not a cross site request forgery, it is an unauthenticated request that the session check refuses anyway. Pre authentication posts (`/login`, `/register`, `/forgot`, `/reset`, `/resend`) have no session to bind a synchroniser token to and are guarded by this check alone, which is stated knowingly in the security model.

Apps, read only
- **AC-14**: `GET /apps` lists only the signed in account's apps, newest first, twenty at a time over the existing `store.Page` cursor. Each row shows the app name, its hostname, the release it is serving, and how its last deploy ended, kept as the two independent facts spec 0012 AC-5 defined rather than blurred into one state. A **Load more** control appends the next page. No app belonging to another account ever appears, and a decommissioned app does not, because every app query already filters `deleted_at IS NULL`.
- **AC-15**: `GET /apps/{slug}` shows the app's name, hostname, the release it is currently serving with that release's image digest, and its last deployment with state, reason and timestamps. **Serving and last deploy are read as two independent facts**, per spec 0012 AC-5: an app whose most recent deploy failed still shows the release it is serving, because a failed deployment has no release row of its own. An app that has never been healthy shows a never deployed state rather than an error. A slug the signed in account does not own renders the same not found page an unknown slug renders, and writes an audit row, so the page surface preserves AC-17 of spec 0007.
- **AC-16**: While the last deployment is in a non terminal state, the embedded script re fetches `GET /apps/{slug}?partial=status` every three seconds and swaps only the status region. It stops on a terminal state, stops after fifteen minutes regardless, and never starts when the deployment is already terminal. If a poll answers with a redirect or any non success status, meaning the session expired underneath it, polling stops silently and the page is left as it stands, so the next navigation lands on sign in rather than the page breaking visibly. With JavaScript disabled the page still renders the correct current state on load. The fifteen minute ceiling is a client behaviour only, stated knowingly: the fragment is a session gated read and a client ignoring the ceiling costs one indexed query.
- **AC-17**: A deployment that did not succeed shows one plain sentence written for its closed reason code, with the raw code shown small beside it. `superseded` renders as cancelled and not as a failure, because a deploy that a newer deploy replaced is not a fault. The sentences are:

  | Reason | Sentence shown |
  |---|---|
  | `upload_invalid` | The uploaded source could not be read. |
  | `upload_expired` | The uploaded source expired before the build started. |
  | `source_rejected` | The source was refused before building. Check it meets the rules in the deploy tool's description. |
  | `build_failed` | The build failed. Its output is in the build pod's logs, not here. |
  | `build_no_digest` | The build finished but produced no image digest. |
  | `image_runs_as_root` | The built image runs as root, which is not allowed. |
  | `app_never_ready` | The app started but never became ready. Check its logs. |
  | `timeout` | The deploy ran out of time. |
  | `internal` | The platform failed while deploying. |
  | `superseded` | Cancelled because a newer deploy replaced it. |

  A reason code with no sentence written for it falls back to showing the code alone, so a code added later to `internal/domain/reason.go` degrades rather than breaks.
- **AC-18**: `GET /apps/{slug}/releases` lists that app's releases newest first, twenty at a time over the existing cursor with the same **Load more** control, showing release number, image digest, created time, and which one the app is currently serving, marked from the same serving release fact AC-15 uses.
- **AC-19**: `GET /apps/{slug}/logs` renders the app's most recent output in a dark monospace pane, read from Kubernetes at the moment of the request and redacted by `internal/logs`, bounded by browser sized constants declared beside the existing ones. Nothing is written to any table, no file is offered for download, and no log line is written to the platform log. A namespace that is not readable yet (`logs.ErrNoNamespace`) renders an explanatory empty state, not an error.
- **AC-20**: `GET /apps/{slug}/config` lists every configuration key with its set state and its secret flag, values masked. `POST /apps/{slug}/config/{key}/reveal` reveals the value of a key that is **not** flagged secret and writes an audit row naming the account, the app and the key. A secret flagged key renders no reveal control at all and has no route that returns its value, so `store.ListConfigForDeploy` gains no third caller and the browser is never a weaker door than MCP.

Tokens
- **AC-21**: `GET /tokens` lists the signed in account's live tokens with name, eight character prefix, created, last used and expiry. No raw value and no hash ever appears in the page, its markup, or a log line.
- **AC-22**: `POST /tokens` mints a token and renders a one time panel containing the raw value, a copy to clipboard control, and an explicit warning that it will not be shown again, plus a dismiss that returns to the list. The raw value never appears in a URL, is never re rendered on any later request, and is never logged.
- **AC-23**: `POST /tokens/{id}/revoke` revokes a token the caller owns and the token stops authenticating on the next request. An id belonging to another account renders not found, matching AC-14 of spec 0007.

Admin
- **AC-24**: `GET /admin/accounts` lists every account newest first with email, display name, verified state, admin flag and disabled state. A signed in account without `is_admin` renders a `403` page; a signed out visitor is redirected to `/login`.
- **AC-25**: Admin can disable, enable, and revoke another account's token from that page. Disable requires typing the target account's email address into a confirmation field before the button submits, because it revokes every session and link that account holds. Every one of the three writes an audit row naming the acting and target account, per AC-22 of spec 0007.

Onboarding and empty states
- **AC-26**: A verified account with zero apps sees an onboarding panel rather than an empty list: the MCP endpoint built from `DEPLOYER_PUBLIC_URL`, the upload endpoint, and a mint a token call to action linking to `/tokens`. Every list surface (apps, releases, tokens, accounts, logs, config) has a written empty state that is distinguishable from something being broken.

Design, motion and reach
- **AC-27**: A single embedded stylesheet defines the design tokens as CSS custom properties: a deep green accent, a neutral shell with white surfaces, one radius scale for chrome and cards and a tighter one for table rows, one type scale and one spacing scale. The shell is a left sidebar in labelled groups beside an inset content panel, matching `design2.webp`, with table row density matching `design.webp`. Light theme only, with the log pane dark regardless.
- **AC-28**: Navigation between pages uses cross document View Transitions, and lists and cards enter with a staggered transition. Under `prefers-reduced-motion: reduce`, every transition and animation is disabled and no content shifts.
- **AC-29**: Every page is usable down to a 375px viewport: the sidebar collapses, and tables reflow to stacked cards below the breakpoint. Nothing overflows the viewport horizontally at any width.
- **AC-30**: Pages use semantic HTML, every input has a real label, focus is always visible, and every action is reachable by keyboard. With JavaScript disabled every page renders and every form submits; only live polling and copy to clipboard are lost.

Leak boundary
- **AC-31**: No rendered page, no template, no redirect URL and no platform log line at info level ever contains a raw API token, a raw session id, a raw verification or reset link token, a password, or a secret configuration value.

## Decision

**Chosen option**: Option 1: server rendered Go templates embedded in the existing binary, reading the existing service and store layer directly, read only for apps.

The deployer binary grows a page surface at the root paths of the host it already serves. Pages are `html/template` rendered, the stylesheet and one small script are `go:embed` assets, page handlers call the same `internal/identity` service and `internal/store` methods the JSON and MCP surfaces call, and no new HTTP API, no new table, and no frontend toolchain is introduced.

**Implementation skills**: `security-patterns` (`~/.claude/skills/security-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `frontend-design` (`frontend-design` plugin) · `impeccable` (`~/.claude/skills/impeccable/`)

## Rationale

Reasoning, the options weighed, and the premise notes: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**

This feature adds **no tables and no columns**. It is a rendering layer over a data model specs 0002, 0007, 0010, 0011 and 0012 already completed.

| Table | Change | Used for |
|---|---|---|
| `accounts` | none | sign in, the admin list, the admin flag |
| `sessions` | none | page authentication, and the CSRF token derives from `sessions.id` |
| `email_tokens` | none | the verify, resend and reset pages |
| `api_tokens` | none | the tokens page |
| `apps`, `deployments`, `deployment_events`, `releases` | none | the apps, overview, releases and status pages, over the existing `store.Page` cursor |
| `app_config` | none | the config page, through `ListConfigForResponse` only |
| `audit_log` | shape unchanged, **new action strings only** | a page sign in, a config value reveal, a CSRF refusal, the admin page actions |
| migrations | **none** | |

One **new sqlc query, no schema change**: `ListAppSummaryPage`, the existing `ListAppSummariesByAccount` projection with the keyset cursor predicate `ListAppsByAccount` already carries, plus a `store.ListAppSummaryPage` wrapper returning `[]AppSummary`. This exists because AC-14 needs the serving release and the last deploy state on a page that also pages, and today one query has the state without a cursor and the other has the cursor without the state. It reads the same tables through the same index and adds no column.

**State transitions**

None introduced. The pages render the existing deployment state machine (spec 0005) and never write to it.

**API surface**

Every path below is a page. `Auth` is `session` unless stated. Every `POST` is CSRF and origin guarded per AC-12 and AC-13.

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/` | GET | none | redirect | none | none |
| `/login` | GET, POST | email, password, next (opt) | session cookie, redirect | none | 401 re rendered form, 403 unverified page, 429 |
| `/register` | GET, POST | email, password, display_name (opt) | check your email page | none | 422 re rendered form, 429, 503 mail unavailable |
| `/verify` | GET | token (query) | verified page | none | link invalid page |
| `/unverified` | GET | none | resend page | none | none |
| `/resend` | POST | none | confirmation | none | 429 |
| `/forgot` | GET, POST | email | confirmation page | none | 429, 503 |
| `/reset` | GET, POST | token (query), password | signed out, redirect to `/login` | none | link invalid page, 422 |
| `/logout` | POST | none | redirect | session | 403 csrf |
| `/apps` | GET | cursor (opt) | app rows, next cursor | session | none |
| `/apps/{slug}` | GET | slug, partial (opt) | overview, or the status fragment | session | 404 page |
| `/apps/{slug}/releases` | GET | slug, cursor (opt) | release rows, next cursor | session | 404 page |
| `/apps/{slug}/logs` | GET | slug | redacted bounded tail | session | 404 page, empty state when unreadable |
| `/apps/{slug}/config` | GET | slug | keys, set state, secret flag | session | 404 page |
| `/apps/{slug}/config/{key}/reveal` | POST | slug, key | the value, non secret keys only | session | 404 page, 403 on a secret key |
| `/tokens` | GET, POST | name, expires_days (opt) | list, or the one time panel | session | 422 invalid expiry |
| `/tokens/{id}/revoke` | POST | id | redirect to list | session | 404 page |
| `/admin/accounts` | GET | none | account rows | session, admin | 403 page |
| `/admin/accounts/{id}/disable` | POST | id, typed email confirmation | redirect | session, admin | 403 page, 422 confirmation mismatch |
| `/admin/accounts/{id}/enable` | POST | id | redirect | session, admin | 403 page |
| `/admin/accounts/{id}/tokens/{tokenId}/revoke` | POST | id, tokenId | redirect | session, admin | 404 page |

**Value sourcing**

| Action | Value produced / displayed | Source |
|---|---|---|
| any page render | the signed in account and its admin flag | `store.ResolveSession`, the existing session middleware |
| any signed in form | the CSRF token | HMAC SHA256 of `sessions.id` under `DEPLOYER_CSRF_KEY`, computed at render, never stored |
| any POST | the expected origin to compare against | `DEPLOYER_PUBLIC_URL`, already validated in `internal/config` |
| apps list | app rows, their serving release and their last deploy state, and the next cursor | a **new paged summary query**: `store.ListAppSummaryPage(accountID, Page{Cursor, Limit: 20})`, the existing `ListAppSummariesByAccount` statement with the same keyset cursor `ListAppsByAccount` already uses. `ListAppSummaries` takes a plain limit and cannot page, and `ListAppsByAccount` returns no state, so neither alone satisfies AC-14 |
| apps list, app overview | the hostname shown per app | `apps.slug` joined with `DEPLOYER_APP_DOMAIN` |
| apps list, empty | the MCP endpoint and upload endpoint shown in onboarding | `DEPLOYER_PUBLIC_URL` plus the literal `/mcp` and `/v1/uploads` paths |
| app overview | the release the app is serving, and its image digest | `AppSummary.ServingRelease` for the number, then `store.GetReleaseByNumber(appID, n)` for the digest. **Not** the latest deployment's release: a failed deployment has no release row, so that chain returns not found exactly when the page matters most |
| app overview | the last deployment's state, reason and timestamps | `AppSummary.LastDeploymentState`, `LastDeploymentReason`, `LastDeployedAt`, read as a fact independent of the serving release |
| app overview | the never deployed state | `AppSummary.LastDeploymentID` being empty, which is what the query projects for an app nothing has ever deployed |
| app overview | whether to start polling | derived from `LastDeploymentState` being non terminal, using the same terminal set `internal/domain` already defines |
| releases page | which release is current | `AppSummary.ServingRelease` compared against each row's release number |
| app overview, failed | the plain sentence shown for a failure | a new lookup table in the page layer keyed by `domain.Reason`, falling back to the code itself |
| app overview, failed | the raw reason code | `deployments.failure_reason` |
| releases page | release rows and next cursor | `store.ListReleasesByApp(appID, Page{Cursor, Limit: 20})` |
| logs page | the log lines | `internal/kube` read at request time, redacted and bounded by `internal/logs` |
| logs page | the line and byte ceilings | new constants in `internal/logs` beside `DefaultTail` and `CurrentBytes`, not configuration, for the same reason spec 0006 gave |
| logs page | the literals redaction matches on | the app's current release configuration, `store.CurrentReleaseConfig`, exactly as `get_logs` does |
| config page | key, set state, secret flag | `store.ListConfigForResponse` |
| config reveal | the revealed value | `store.ListConfigForResponse`, which returns a value only for a key not flagged secret |
| tokens page | name, prefix, created, last used, expiry | `store.ListLiveAPITokens` |
| token mint | the raw token shown once | the return of the existing mint service call, held in the response body only, never persisted or redirected with |
| admin page | the account rows | `store.ListAccounts` |
| every audit row written | acting account, target, action string | the session account, the path parameter, and a new action constant beside the existing ones |

**Key invariants**

- A page never reaches the database or Kubernetes except through a service or store method an existing surface already calls. A page specific query is a signal the invariant broke.
- `store.ListConfigForDeploy` keeps exactly two callers, the deploy path and the release snapshot. The browser is not a third one.
- The browser is never a weaker door than the agent surface: anything MCP refuses to return, a page refuses to render.
- The CSRF token is derived, never stored, so it is valid exactly as long as its session and is revoked by the same act that revokes the session.
- An app's own output is still never stored. The logs page is a read at the moment of the request, redacted, bounded and discarded, per the rule in `AGENTS.md`.
- Every state changing page action writes the same audit row its JSON equivalent writes.

**Security model**

- **Who reads what**: a signed in account reads only its own apps, releases, logs, configuration keys and tokens. Ownership is enforced by the same account scoped store methods the MCP tools use, not by a new check. An admin gets the accounts list and the three admin actions and gets **no** override on app ownership, preserving AC-21 of spec 0007.
- **Session handling**: unchanged from spec 0007. `HttpOnly`, `SameSite=Lax`, `Secure` when `DEPLOYER_PUBLIC_URL` is `https`, thirty day rolling expiry.
- **CSRF**: a per session synchroniser token on every authenticated POST, plus an `Origin` and `Sec-Fetch-Site` check on every POST. Accepted knowingly: the pre authentication posts carry no synchroniser token, because there is no session to bind one to and adding a pre session cookie to defend against login CSRF is more moving parts than the risk earns on a tailnet only internal tool. They keep the header check, the shared rate limit bucket, and `SameSite=Lax`.
- **Sensitive values**: secret configuration values are unreadable through the browser, the same as through MCP. A minted token is shown once in a response body and never again. Log output is redacted by `internal/logs` before it reaches a template.
- **Reach**: the pages inherit the platform's existing exposure. They are reachable wherever `/v1` and `/mcp` already are, which is the LAN or tailnet, and no new ingress, Service or certificate is added.
- **Compliance scope**: none. No regulated data class is introduced by this feature.

**Configuration required**

- `DEPLOYER_CSRF_KEY`: the secret the per session CSRF token is derived under. At least 32 bytes of random data, validated at startup in `internal/config` like every other variable, delivered as a sealed secret beside the existing ones. Rotating it invalidates every outstanding form, which is harmless, since a resubmit succeeds.

**Critical test scenarios** (each maps to an acceptance criterion in `## Requirements`)

- Happy path: register, verify by fetching the link URL, sign in, land on `/apps`, mint a token, see it once, revoke it. Verifies **AC-5**, **AC-6**, **AC-7**, **AC-21**, **AC-22**, **AC-23**.
- Happy path: with an app that has a healthy release, the overview, releases, logs and config pages all render its real data from the store and the fake clientset. Verifies **AC-15**, **AC-18**, **AC-19**, **AC-20**.
- Failure case: a POST with a missing or altered `csrf` field changes nothing and answers `403` with an audit row, and a POST with a foreign `Origin` is refused before the handler runs. Verifies **AC-12**, **AC-13**.
- Failure case: an app with a healthy release followed by a **failed** deployment renders both the release it is still serving and the failure, on the apps list and the overview alike. This is the case the naive latest deployment chain gets wrong, and it is the state a person opens the page in most often. Verifies **AC-14**, **AC-15**.
- Failure case: a deployment in a failed state renders the written sentence for its reason code, a `superseded` deployment renders as cancelled rather than failed, and a reason code with no sentence renders the raw code rather than an empty element. Verifies **AC-17**.
- Failure case: a poll of the status fragment whose session has been revoked underneath it stops polling and leaves the page intact rather than swapping in a redirect body. Verifies **AC-16**.
- Failure case: the logs page for an app whose namespace is not readable yet renders the explanatory empty state, not a 500. Verifies **AC-19**.
- Auth/permission: account B requesting account A's app slug on all four app pages gets the not found page, identical to an unknown slug, with an audit row. Verifies **AC-15**.
- Auth/permission: a non admin session on `/admin/accounts` gets the 403 page, a signed out visitor gets redirected to `/login`, and a request carrying a valid bearer API token instead of a session is treated as signed out on every page. Verifies **AC-3**, **AC-24**.
- Auth/permission: the reveal route refuses a secret flagged key and returns the value for a key that is not flagged, writing an audit row either way. Verifies **AC-20**.
- Leak boundary: a crawl of every page as a signed in account asserts that no response body and no captured log line contains the session cookie value, a raw token, or a secret configuration value. Verifies **AC-31**.

## Build plan

Ordered as a **Tracer Bullet**, because that is the project's approach: task 1 puts a real page in front of real data through every layer this feature introduces (embedded assets, page routing, session gated rendering, the store), before any of those layers is built out fully.

1. [x] Thin thread end to end: an `internal/web` package with the `go:embed` asset set, a base layout, the page session middleware, and three real routes, `GET /login`, `POST /login`, `GET /apps` rendering the account's apps from `store.ListAppsByAccount`, plus `POST /logout` and the `/` redirect. Unstyled. Satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-5**, **AC-11**.
2. [x] `DEPLOYER_CSRF_KEY` added and validated in `internal/config`, the derive and verify helper, the origin and `Sec-Fetch-Site` check, and both applied to `POST /logout` as the first guarded action. Every later POST adopts them. Satisfies **AC-12**, **AC-13**.
3. [x] The design system: the embedded stylesheet with the token set, the sidebar and inset panel shell, table, card and button primitives, cross document View Transitions, the staggered entrance, the `prefers-reduced-motion` block, and the responsive breakpoints. Applied to the thin thread pages so the shell is real before more pages hang off it. Satisfies **AC-27**, **AC-28**, **AC-29**.
4. [x] The rest of the identity flow: register, verify, unverified with resend, forgot and reset pages, and repointing the verification and reset email links at the page URLs. Satisfies **AC-6**, **AC-7**, **AC-8**, **AC-9**, **AC-10**.
5. [x] The `ListAppSummaryPage` sqlc query and its store wrapper, then the apps list paging over it with the **Load more** control, and the zero apps onboarding panel. Satisfies **AC-14**, **AC-26**.
6. [x] The app overview page reading serving release and last deploy as two independent facts, the `?partial=status` fragment, the polling script with its silent stop on a non success answer, and the failure reason sentence table. Satisfies **AC-15**, **AC-16**, **AC-17**.
7. [x] The releases page with the same paging control, marking the serving release. Satisfies **AC-18**.
8. [x] The logs page and the browser sized bounds constants in `internal/logs`. Satisfies **AC-19**.
9. [x] The config page, the reveal route restricted to keys not flagged secret, and its audit action. Satisfies **AC-20**.
10. [x] The tokens page, mint with the one time panel and copy control, and revoke. Satisfies **AC-21**, **AC-22**, **AC-23**.
11. [x] The admin accounts page with disable behind a typed email confirmation, enable, and foreign token revoke. Satisfies **AC-24**, **AC-25**.
12. [x] The accessibility and leak pass: labels, focus states, keyboard reachability, the no JavaScript check, and the crawl asserting nothing sensitive appears in any page or log line. Satisfies **AC-30**, **AC-31**.
13. [ ] Deploy wiring: the `DEPLOYER_CSRF_KEY` sealed secret in `deploy/`, and the `deploy/AGENTS.md` note recording it. Satisfies **AC-1**. _The manifest wiring, the kustomization entry and the `deploy/AGENTS.md` note are in. The sealed value itself is still a placeholder: sealing needs the cluster's own public key, so it has to be produced against the cluster with the `kubeseal` command in `deploy/web-sealedsecret.yaml`. The control plane will not start until it is._

## Consequences

**Positive**:
- The platform stops being agent only. A person can register, verify, sign in, mint a token, and read the state of everything they own without curl.
- The four admin endpoints spec 0007 built stop being dead code.
- The paging both spec 0011 and spec 0012 deferred to this feature lands, over cursors that already exist, with no new store method.
- The artifact does not change shape: one Go binary, one image, one pod, one ingress, no node, no bundler, no second deployable.
- Verification and reset emails stop landing people on JSON.

**Negative / tradeoffs**:
- The pages are a second consumer of the same store and service methods. A change to one of those methods now has two callers to keep honest, and only the MCP one has a wire level test harness today.
- `html/template` gives no component model. Shared page furniture is template partials and a hand written stylesheet, which is fine at this size and is genuinely worse than a component framework at three times this size.
- Read only for apps means the browser cannot fix anything. Seeing a failed deploy and having to go back to the agent to roll it back is a real papercut, and it is the deliberate cost of not building CSRF protected destructive actions in this slice.
- A second surface authenticated by session cookie widens what a stolen session is worth. It is still worth strictly less than a stolen API token, because the pages cannot deploy, delete, roll back, change configuration, or read a secret value.
- The failure reason sentences are prose that can drift from `internal/domain/reason.go`. The fallback keeps it from breaking, but a new code will silently show as a raw code until someone writes its sentence.
- Motion and responsive work are the two things most likely to be judged by eye rather than by test, so they will take a real `/check verify` pass rather than a green suite.

**Neutral**:
- One new environment variable and one new sealed secret.
- One new sqlc query, `ListAppSummaryPage`, and the `sqlc generate` loop that comes with it. No schema change and no new index.
- New audit action strings, no audit schema change.
- `internal/web` is a new package at the edge, in the same position as `internal/httpapi`, and is bound by the same layering rule: it may import the store and the services, and the domain must not import it.

## Follow-up

- [ ] `frontend-design` and `impeccable` are installed and both materially shape this feature, and neither is referenced in root `AGENTS.md`. They are project wide once a UI exists, so their conventions belong in root `AGENTS.md` `## Agent skills` rather than in a nested file.
- [ ] Once `internal/web` exists, it warrants its own nested `internal/web/AGENTS.md` recording the template layout, the asset embed, the design tokens, and the rule that a page never queries on its own.
- [ ] Write actions from the browser (rollback, delete, edit configuration) were deliberately left out. Revisit once the read only pages have been lived with, and note that rollback is the one with a real case behind it.
- [ ] An audited browser reveal of secret configuration values was weighed and refused to keep `ListConfigForDeploy` at two callers. If reading a secret back becomes a real debugging need, it is a decision of its own, not a quiet addition.
- [ ] Dark mode was scoped out. If it is added later it is a token swap under `prefers-color-scheme`, which is cheap only if the stylesheet is written with every colour as a token from the start.
- [ ] Kubernetes events for an app that prints nothing, already deferred in spec 0006, gets a stronger case now: an empty log pane in a browser is a more visible dead end than an empty tool response.
