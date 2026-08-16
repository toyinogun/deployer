# 0022. Publishing the deploy path

**Date**: 2026-08-16
**Status**: In Progress

## Summary

An agent still needs Tailscale to deploy, because spec 0021 published the console and deliberately left the tarball upload and the MCP endpoint on the tailnet. This puts them on a third public hostname, `mcp.deploy.toyintest.org`, carried by the tunnel you already run, and retires the tailnet deploy path once the public one is proved. The tailnet was doing real work as an outer gate, so the controls it was silently providing are replaced in the platform: the upload ceiling drops to 90 MB so the platform always refuses before Cloudflare does, the visitor's real address is read on this hostname too, a rate limit and a bad token lockout bound the two routes, and an account may hold at most three unclaimed uploads with a sweep that clears the expired ones.

## Requirements

**User stories**:

- As an agent deploying an app, I want to upload source and drive a deploy from any machine, so that installing Tailscale is not a step between someone and their first deploy.
- As the operator, I want every control the tailnet was providing to be named and either held elsewhere or accepted in writing, so that publishing the surface that runs code on my cluster is a decision rather than a side effect.
- As an agent, I want to learn the upload ceiling from the tool description and be refused by the platform with a reason code, so that an oversized tarball never produces an error page I cannot interpret.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

- **AC-1**: `POST /v1/uploads` and `/mcp` answer on `mcp.deploy.toyintest.org`, and a deploy driven end to end through that hostname from a machine with no Tailscale reaches a healthy app hostname.
- **AC-2**: Registration under the deploy host is opt in, exactly as it is for the console: every route that answers there is registered a second time under the deploy host pattern, and one catch all answers 404 for everything else on that hostname. A route added to the mux later is absent from the deploy host until somebody registers it there. The plumbing does not exist yet and is part of this work: `httpapi.API.Register` takes only a mux today, so it gains a deploy host the way `web.Options.ConsoleHost` and `withHost` already work, and `/mcp` moves out of its ad hoc `mux.Handle` in `cmd/deployer/main.go` behind the same shape. The deploy host catch all is registered once, beside the console's.
- **AC-3**: The console hostname still carries no route that changes cluster state. Adding the deploy host registers no `/v1` route on the console host, and `internal/web`'s console host test still passes unchanged.
- **AC-4**: `GET /v1/uploads/{id}`, the single use fetch a build's init container reads, stays on the default pattern and is not registered on the deploy host. The init container keeps reaching it on `DEPLOYER_INTERNAL_URL` over cluster DNS.
- **AC-5**: After the cutover, `POST /v1/uploads` and `/mcp` are absent from the tailnet name and answer a plain 404 there, the same as any unregistered route.
- **AC-6**: The cloudflared rule for `mcp.deploy.toyintest.org` points straight at the `deployer` Service and is listed above the `*.deploy.toyintest.org` wildcard, because a leading `*.` matches exactly one label and `mcp` is one label. A test reads the rules by name and asserts the deploy host rule precedes the wildcard.
- **AC-7**: Regression guard on existing behaviour: the catch all still refuses every name that is not one of the three, so adding a rule does not make anything else public by accident.
- **AC-8**: `DEPLOYER_MCP_HOST` is validated at startup: present, exactly one label under `DEPLOYER_APP_DOMAIN`, and not equal to `DEPLOYER_CONSOLE_HOST`. Its base URL is derived as `https://` plus the host and is never configured separately, so the two cannot disagree.
- **AC-9**: `DEPLOYER_PUBLIC_URL` is removed from the code and from every manifest, and all four of its consumers are given a named replacement rather than dropped. Startup fails on the old variable being set rather than silently ignoring it.
  - `internal/mcp/mcp.go`, `deploy_app`'s description, becomes the derived MCP URL.
  - `internal/web/apps.go`, the MCP and upload endpoints shown on a person's page, become the derived MCP URL. This is the value feature 23 hands out.
  - `internal/web/web.go` and `internal/httpapi/identity.go`, where the parsed scheme decides the session cookie's `Secure` attribute, become `ConsoleURL`. The cookie belongs to the console surface, so it must follow the console's own address and not the machine surface's.
- **AC-10**: `deploy_app`'s tool description carries the new upload endpoint, derived from `DEPLOYER_MCP_HOST`, and states the upload ceiling. The stated ceiling equals the configured `MaxUploadBytes` rather than a literal in the text.
- **AC-11**: The default `MaxUploadBytes` is 90 MB, under Cloudflare's 100 MB body cap, so a body the platform accepts can never be one the edge refuses.
- **AC-12**: A declared `Content-Length` over the ceiling is refused before a single byte is read, and a body that declared nothing or lied is stopped at the socket. Both refusals answer a closed reason code and write an audit row.
- **AC-13**: `CF-Connecting-IP` is trusted on the console host and the deploy host and on no other host. The derivation lives once, in `auth.ClientAddress`, which now takes the trusted host set rather than a single host.
- **AC-14**: `uploadAddress` no longer passes an empty host. The upload route, the MCP endpoint and the page surface derive the visitor's address identically, so they spend from one bucket per address rather than three.
- **AC-15**: A per address token bucket bounds `POST /v1/uploads` and `/mcp`, from its own `Limiter` instance sized for the deploy path. `identity.Limiter` holds its five numbers as package constants today, so `NewLimiter` takes them as a settings value instead: the sign in limiter keeps exactly today's values and the deploy path gets its own. A burst on the deploy path never spends the sign in budget and never locks a person out of the console.
- **AC-16**: Repeated bad bearer tokens from one address earn a growing penalty, answered 429 with a closed reason code. The rule lives inside `auth.Authenticator`, so both routes inherit it, and neither handler contains a copy of it.
- **AC-17**: An account may hold at most `DEPLOYER_MAX_UNCLAIMED_UPLOADS` unclaimed uploads, default 3. A further upload is refused with a closed reason code before any byte is written to the volume, and the refusal is audited.
- **AC-18**: A sweep deletes expired uploads that no deployment references, the file and the row together, and leaves a referenced upload's row alone so deploy history stays intact.
- **AC-19**: Every refusal on the deploy path is one of the closed `domain.Reason` codes and is audited with the visitor's real address. The two new ones are `upload_limit_reached` and `too_many_attempts`, both in `internal/domain/reason.go` beside `account_suspended`, which the upload route already answers. Neither reuses `identity.CodeRateLimited`: the two closed sets deliberately share no values, so a rate limit refusal on the machine surface needs its own name rather than the identity surface's. No wrapped error string and no build output crosses the boundary.
- **AC-20**: Every MCP tool returns in under 30 seconds through the public hostname, a margin against Cloudflare's 125 second origin timeout, observed rather than assumed. The timeout is recorded as a bound the design depends on.
- **AC-21**: The tailnet registrations are removed last and alone, in their own commit, after AC-1 has been observed live.
- **AC-22**: Regression guard on existing policy: the `deployer-system` policy still names the tunnel namespace as the only outside peer on 8080, and the tunnel policy still permits only the two origins it needs. A second public hostname landing on the same Service must add no new peer, and a test asserts that rather than the build assuming it.
- **AC-23**: The limiter stays in memory and is lost on restart. Its comment stops claiming the perimeter is a tailnet, and the spec records what that costs, so the assumption is decided rather than inherited.

## Decision

**Chosen option**: Option 1: A third hostname on the existing tunnel, with the platform bounding itself under the edge's limits.

Publish `POST /v1/uploads` and `/mcp` on `mcp.deploy.toyintest.org`, routed by the existing tunnel straight to the `deployer` Service, keep the opt in registration shape spec 0021 established, drop the upload ceiling to 90 MB so the platform refuses before Cloudflare can, replace the controls the tailnet was providing with a rate limit, a bad token lockout, an unclaimed upload cap and a sweep, and retire the tailnet deploy path in its own commit once a real deploy has run through the public one.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`)

## Rationale

Reasoning, the options weighed, and the sources: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

No schema change. Nothing new is stored and no migration runs.

- The unclaimed upload cap is a count over the existing `uploads` table: rows for the account with `redeemed_at IS NULL` and `expires_at` in the future. The count and the insert belong in one transaction in `internal/store`, the same shape `CreateApp` uses for the per account app cap, so a second caller cannot walk past the cap by racing.
- The sweep deletes rows from `uploads` where `expires_at` has passed and no `deployments` row references the id. `deployments.upload_id` carries `ON DELETE RESTRICT`, so a referenced row can never be deleted and the sweep must exclude it by query rather than discover it by error.
- The rate limit and the lockout hold no rows at all. They live in `identity.Limiter`, in memory, lost on restart (AC-23).

**State transitions**:

None new. An upload is unclaimed until a deploy redeems it, and this spec adds only a cap on how many unclaimed ones an account may hold at once and a sweep that removes expired unclaimed ones.

**API surface**:

| Endpoint | Method | Host pattern | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|---|
| `/v1/uploads` | POST | `mcp.<app domain>` only | gzipped tar body | `upload_id`, `expires_at` | bearer token | 401 unauthorized · 403 `account_suspended` · 413 `upload_too_large` · 400 `upload_not_gzip` · 429 `upload_limit_reached` · 429 `too_many_attempts` |
| `/mcp` | POST | `mcp.<app domain>` only | MCP tool calls | tool results | bearer token | 401 unauthorized · 429 `too_many_attempts` · the existing closed reason codes per tool |
| `/v1/uploads/{id}` | GET | default pattern only | single use fetch token | the tarball | fetch token | 401 · 404 |

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| `deploy_app` description | the upload endpoint URL | derived as `https://` plus `DEPLOYER_MCP_HOST`, never configured (AC-8, AC-10) |
| `deploy_app` description | the upload ceiling in bytes | `cfg.MaxUploadBytes`, formatted at description build time, never a literal in the text (AC-10) |
| `POST /v1/uploads` | the visitor's address on the audit row | `auth.ClientAddress(r, trustedHosts)`, `CF-Connecting-IP` when the request host is the console host or the deploy host, else `X-Forwarded-For`'s last hop, else the socket peer (AC-13, AC-14) |
| `POST /v1/uploads` | the refusal a caller sees | the closed set in `internal/domain/reason.go`, two codes added by this spec (AC-19) |
| `POST /v1/uploads` | the account's current unclaimed upload count | a count over `uploads` inside the same transaction as the insert |
| `POST /v1/uploads` | the cap that count is judged against | `DEPLOYER_MAX_UNCLAIMED_UPLOADS`, default 3, validated at startup |
| `/mcp` and `/v1/uploads` | whether this caller has budget left | the deploy path `Limiter` instance, keyed on the same derived address |
| the deploy path `Limiter` | its bucket capacity and refill | a new settings value passed to `NewLimiter`: capacity 30, one token back every 2 seconds. Sized so an agent polling `deployment_status` through a build never trips it, while a flood is still bounded at 30 a minute sustained |
| the deploy path `Limiter` | its lockout thresholds | the same settings value: 5 failures for free, a 30 second first penalty doubling to a 15 minute ceiling, matching the sign in values because the shape of the attack is the same |
| the sign in `Limiter` | its five numbers | the same settings value, carrying exactly today's constants, so this refactor changes no sign in behaviour |
| `auth.Authenticator` | whether this address is inside a penalty window | the same `Limiter`'s lockout, keyed on the derived address rather than on an email |
| the sweep | which uploads may be deleted | a query for expired rows with no referencing deployment (AC-18) |
| cloudflared | which name reaches which origin | `deploy/cloudflared-configmap.yaml`, reviewed in the repo, read by nothing at run time |

**Key invariants**:

- A route that changes cluster state is absent from a hostname's mux rather than refused by a check inside it. Two host patterns now carry opt in registrations, the console and the deploy host, and the default pattern carries everything. A new route is private on both public hostnames until somebody registers it twice.
- `CF-Connecting-IP` is trusted only on hostnames whose tunnel origin is the `deployer` Service directly. That is what makes the header safe, not the header itself: `ingress-nginx` is reachable from most of the cluster, so a request arriving through it proves nothing about where it came from. Moving either public hostname behind `ingress-nginx` makes the header writable from most of the cluster and nothing in the suite would notice.
- The platform's ceiling stays strictly under the edge's, so the platform is always the thing that refuses. A ceiling raised to or past 100 MB silently hands one failure mode to Cloudflare, where it produces no reason code and no audit row.
- Every rule that judges a credential lives in `auth.Authenticator`, never in a handler. The lockout added here is exactly the shape that was wrong in `Service.Login` until 2026-08-16, so it goes in once and is not copied into either route. The token bucket stays with the handlers, because it bounds the call rate rather than judging the credentials.
- A quota tracked count is read and written in one transaction. The unclaimed upload cap follows `CreateApp`: the count and the insert are one store call, so a new caller reaches the cap by using that call rather than by repeating the check.
- The tunnel rule order is load bearing. A leading `*.` matches exactly one label, so `*.deploy.toyintest.org` matches `mcp`, and the deploy host rule listed after the wildcard is unreachable. That already shipped once, for the console, and survived every test because the tests read the rules by position and the positions agreed with each other.

**Security model**:

- The account bearer token is the whole credential on the deploy path. No outer gate replaces the tailnet, accepted deliberately and recorded here rather than left implicit: a second secret in an MCP client's config is exactly the step feature 23 exists to remove.
- What the tailnet was providing, and what now holds each half: reachability, replaced by nothing and accepted; a bound on unauthenticated call volume, replaced by the per address token bucket (AC-15); a bound on credential guessing, replaced by the lockout inside the authenticator (AC-16); a bound on what one valid token can cost you in disk, replaced by the unclaimed upload cap and the sweep (AC-17, AC-18); attribution of who called, replaced by `CF-Connecting-IP` on this hostname and the audit rows already written (AC-13, AC-19).
- Both new bounds are in memory and reset on every pod restart, and ArgoCD restarts the pod on each sync. Against a random token that is not a feasible brute force window, so their real job is slowing a script and bounding cost. Recorded as a bound, not hidden (AC-23).
- The single use fetch route stays internal and is guarded by a token minted per build that expires with the upload, so reaching it is worth nothing without that token (AC-4).
- No compliance scope applies. The address column and its retention sweep already exist from spec 0021 and are unchanged.

**Configuration required**:

- `DEPLOYER_MCP_HOST`: the public deploy hostname, exactly one label under `DEPLOYER_APP_DOMAIN` and different from `DEPLOYER_CONSOLE_HOST`. Its base URL is derived, never configured.
- `DEPLOYER_MAX_UNCLAIMED_UPLOADS`: how many unclaimed uploads one account may hold. Default 3.
- `DEPLOYER_MAX_UPLOAD_BYTES`: unchanged variable, new default of 90 MB.
- `DEPLOYER_PUBLIC_URL`: removed.

**Critical test scenarios**:

- Happy path: a deploy driven end to end through `mcp.deploy.toyintest.org` from a machine with no Tailscale reaches a healthy app hostname, verifies **AC-1**.
- Registration shape: a route added to the mux and not registered under the deploy host answers 404 there, and every `/v1` route answers 404 on the console host, verifies **AC-2**, **AC-3**.
- Rule order: the cloudflared rules are read by hostname, not by position, and the deploy host rule precedes the wildcard, verifies **AC-6**.
- Failure case: a body declaring more than the ceiling is refused before a byte is read, with the closed code and an audit row, verifies **AC-11**, **AC-12**.
- Failure case: a fourth unclaimed upload is refused, nothing lands on the volume, and the third still redeems normally, verifies **AC-17**.
- Failure case: the sweep deletes an expired unreferenced upload's file and row, and leaves an expired upload a deployment references untouched, verifies **AC-18**.
- Auth/permission: repeated bad bearer tokens from one address earn a 429 with the closed code on both the upload route and the MCP endpoint, proving the rule is in the authenticator rather than in one handler, verifies **AC-16**.
- Address derivation: `CF-Connecting-IP` is honoured on the console host and the deploy host and ignored on every other host, including the in cluster Service address, verifies **AC-13**, **AC-14**.
- Config: startup fails on a missing `DEPLOYER_MCP_HOST`, on one carrying more than one label, on one equal to the console host, and the derived URL matches the host, verifies **AC-8**.
- Wire level: the two new reason codes reach a caller through a real client and server session in `internal/mcp/wire_test.go`, not only through a handler call, verifies **AC-19**.

## Build plan

Ordered as a Tracer Bullet: get one real deploy running through the public hostname before any of the bounds are thickened, because the routing is the part that has already bitten once and the bounds are worthless if the route is wrong. The cutover is last and alone.

1. [x] Add `DEPLOYER_MCP_HOST` with its startup validation and derived URL, remove `DEPLOYER_PUBLIC_URL`, and point `deploy_app`'s description at the derived endpoint, satisfies **AC-8**, **AC-9**, **AC-10**.
2. [x] Register `POST /v1/uploads` and `/mcp` a second time under the deploy host pattern, add the catch all 404 for that hostname, and leave the tailnet registrations in place for now. The console host's existing test must pass unchanged, satisfies **AC-2**, **AC-3**, **AC-4**.
3. [x] Add the cloudflared rule for the deploy host above the app wildcard, with a test that reads the rules by hostname rather than by position, satisfies **AC-6**, **AC-7**.
4. [x] Confirm the `deployer-system` and tunnel policies need no new peer, with a test asserting it, satisfies **AC-22**.
5. [ ] Add the DNS record and drive one real deploy through the public hostname from a machine with no Tailscale, satisfies **AC-1**.
6. [x] Drop the default `MaxUploadBytes` to 90 MB and prove the platform refuses before the edge does, satisfies **AC-11**, **AC-12**.
7. [x] Widen `auth.ClientAddress` to a trusted host set, stop `uploadAddress` passing an empty host, and assert the header is ignored on every other host, satisfies **AC-13**, **AC-14**.
8. [x] Move `identity.Limiter`'s five constants into a settings value taken by `NewLimiter`, with the sign in limiter keeping today's numbers unchanged, then add the deploy path instance and spend from it on both routes, satisfies **AC-15**.
9. [x] Add the two reason codes, and the bad token lockout inside `auth.Authenticator`, with the wire level test on both routes, satisfies **AC-16**, **AC-19**.
10. [x] Add `DEPLOYER_MAX_UNCLAIMED_UPLOADS`, the transactional count and insert in `internal/store`, and the refusal, satisfies **AC-17**.
11. [x] Add the expired unclaimed upload sweep beside the existing daily audit sweep, satisfies **AC-18**.
12. [x] Correct the limiter's comment about the perimeter and record the restart bound, satisfies **AC-23**.
13. [ ] Drive every MCP tool through the public hostname and record the observed timings against the 125 second bound, satisfies **AC-20**.
14. [x] Last and alone, in its own commit: remove the tailnet registrations for both routes and confirm they answer 404 on the tailnet name, satisfies **AC-5**, **AC-21**.

## Migration plan

**Strategy**: strangler. The public path is built beside the tailnet one and proved with a real deploy before the tailnet one is retired.

**Phases**:

1. Build plan steps 1 to 5. Both paths answer. Nothing that works today stops working, and the public path is proved by a real deploy rather than by a host header override alone.
2. Build plan steps 6 to 13. The bounds land on a path that is already known to route correctly, so a failure in one of them is unambiguous.
3. Build plan step 14, on its own. The tailnet registrations go, and the only way to deploy is the public hostname.

**Rollback**: phases 1 and 2 revert by reverting their commits, since both leave the tailnet path working. Phase 3 reverts by reverting one commit, which restores the tailnet registrations without touching DNS or the tunnel. The DNS record and the tunnel rule are additive and can stay through any rollback.

**Risks**:

- The tunnel rule order. Listed after the wildcard, the deploy host is unreachable and every request is handed to `ingress-nginx`, which answers its own 404. This is exactly what happened to the console on 2026-08-16 and the tests did not catch it, which is why AC-6 asserts by name rather than by position.
- Phase 3 is the point of no easy return in practice rather than in theory: after it, a tunnel outage stops every deploy including yours, with no second way in.

## Consequences

**Positive**:

- An agent deploys from anywhere. Joining drops from four steps to three, and feature 23 can hand someone one block to paste with a real public endpoint in it.
- The platform refuses oversized bodies itself, with a reason code and an audit row, instead of handing one failure mode to an edge that answers an error page.
- Three controls the tailnet was providing invisibly become controls the platform states, tests and can reason about.
- Every address handed to a caller is now derived from a host setting, so the last configured URL is gone and two of them can no longer disagree.

**Negative / tradeoffs**:

- A Cloudflare or tunnel outage stops every deploy, including yours, with no way in. Accepted knowingly.
- The deploy path is now reachable by anyone on the internet holding a valid token, and nothing but that token stands in front of it.
- The rate limit and the lockout reset on every pod restart, and ArgoCD restarts the pod on each sync, so both are softer than they read.
- The upload ceiling drops by 10 MB, so a source tarball between 90 and 100 MB that works today stops working.
- A third public hostname is a third thing to keep in step across the mux, the tunnel config, DNS and the network policies, and the rule ordering trap now applies to two names rather than one.

**Neutral**:

- No migration and no schema change. The cap and the sweep are queries over tables that already exist.
- The wildcard certificate already covers a single label under the app domain, so `mcp.deploy.toyintest.org` needs no new certificate and no cert-manager change.
- `api`, `mcp`, `deployer` and `deploy` are already in the reserved label set, so no app slug can ever claim this name and `internal/domain/reserved.go` is unchanged.

## Follow-up

- [ ] Feature 23's ready to paste block depends on `DEPLOYER_MCP_HOST` existing and on the derived endpoint. Build this first, or feature 23 hands out a tailnet address.
- [ ] The deferred item "Publishing the deploy path" in `docs/scope/scope.md` is answered by this spec and should be closed out when feature 24 is done.
- [ ] Nothing tests that `deploy_app`'s description matches its behaviour, and this spec makes the description carry two values that can drift, the endpoint and the ceiling. The existing deferred item for that test is now eight tools wide plus these two values.
- [ ] Cloudflare's documentation does not state whether the 125 second origin timeout is reset by bytes flowing, so the streamable HTTP transport's standalone stream is an unbounded assumption rather than a checked one. Worth reopening if a tool call ever fails with no platform side trace.
- [ ] The registry has no garbage collection and the upload volume now has a sweep. The two are the same class of problem and only one of them is solved.
- [ ] The unclaimed upload cap bounds one account at three times the ceiling. Nothing bounds accounts multiplied by that cap against the size of the PVC, so enough valid tokens still fill the volume. Invisible while there is one account, and it is the same shape as the app cap question feature 17 answered per account rather than per cluster.
