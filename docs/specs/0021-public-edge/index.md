# 0021. Public edge: a Cloudflare tunnel, a console hostname, and the visitor's real address

**Date**: 2026-08-15
**Status**: Accepted

## Summary

Everything the platform serves is reachable today only from your tailnet or your LAN. This opens two of those surfaces to the open internet and leaves the rest where they are. A Cloudflare Tunnel (a small process in the cluster that dials out to Cloudflare, so no port is ever opened on your router) carries the app wildcard and a new console hostname, `console.<app domain>`. App traffic goes on through `ingress-nginx` as it does today; console traffic goes straight to the control plane Service, so the tunnel is the only thing that can reach it and network policy can say so. The MCP endpoint, the tarball upload, and every admin page stay on the tailnet, refused on the console hostname by the router itself. Behind a proxy the platform can no longer see who is calling, so the visitor's real address is read from Cloudflare's own header, keyed into the rate limiter, and written to a new nullable column on `audit_log` that a daily sweep clears after the retention window.

## Requirements

**User stories**:
- As a person invited to the platform, I want to sign in and manage my apps from any machine, so that joining does not start with installing a VPN.
- As a person who deployed an app, I want to send its link to someone, so that the thing I built is a thing they can open.
- As the platform operator, I want my home address to appear in no DNS record and no certificate, so that publishing an app does not publish where I live.
- As the platform operator, I want the deploy path and every administrative page to stay off the open internet, so that the surface that changes the cluster is not the surface a stranger can reach.
- As the platform operator, I want the rate limiter and the audit trail to see the visitor rather than the proxy, so that one abuser is one bucket and one audit row rather than everybody.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

The console hostname and the split

- **AC-1**: `DEPLOYER_CONSOLE_HOST` is a new required setting, validated at startup in `internal/config` like every other. It must be exactly one DNS-1123 label, then `.`, then the value of `DEPLOYER_APP_DOMAIN`, and nothing deeper. `console.deploy.example.org` against `deploy.example.org` passes; `console.staging.deploy.example.org` and a bare label do not. This keeps it inside the existing wildcard certificate and makes the label it occupies derivable. The console's own base address is derived as `https://` plus that host and is never configured separately, so the two cannot disagree.
- **AC-2**: A request whose `Host` matches `DEPLOYER_CONSOLE_HOST` is answered only by the public page routes. Every admin page, `/mcp`, and every `/v1/` path answers `404` on that host, changes nothing, and writes no audit row, because no caller has been identified at that point.
- **AC-2a**: The split is expressed in the `http.ServeMux` itself, using the host qualified pattern form the standard mux has carried since Go 1.22. Every public route is registered twice, once bare and once prefixed with the console host, and a single `<console host>/` pattern catches everything else on that host and answers `404`. The mux's own most specific match rule then decides, so there is one routing table rather than a table plus a classifier that can drift from it. A future route that nobody remembers to register twice is private, not public, which is the direction this should fail in.
- **AC-3**: A request whose `Host` matches the host in `DEPLOYER_PUBLIC_URL`, the tailnet name, is answered by every route the platform has, public pages included. The tailnet stays a complete door, so a tunnel outage never costs you access to your own platform.
- **AC-4**: A request whose `Host` matches neither is answered by every route, unchanged from today. This keeps in cluster callers, health probes, and a port forward working, and it is why AC-2 is written as a rule about one named host rather than as a default deny.
- **AC-5**: The public console renders no link, no navigation entry, and no button to any page it would answer `404` for. The split is visible in the interface rather than discovered as an error.

The reserved name

- **AC-6**: `internal/domain` carries a reserved label set as a package constant, config free, holding at least `console`, `www`, `api`, `admin`, `mcp`, `app`, `deployer`, and `registry`. App creation refuses a derived slug in that set with the new closed reason code `app_name_reserved`, and the refusal reaches a caller as that code over a real client and server session, not only through a direct handler call.
- **AC-7**: The check runs on the derived slug, so a display name that derives to a reserved label is refused just as a literal one is. It runs on the create path only. An app that already holds one of these slugs keeps deploying, rolling back and being listed exactly as before, and no migration runs.

The tunnel and its two origins

- **AC-8**: The wildcard `Certificate` named `wildcard-apps` is issued by the production Let's Encrypt `ClusterIssuer` rather than the staging one, reports `Ready`, and still covers both `*.<app domain>` and the bare domain. Every path, tailnet and LAN included, then serves a certificate a browser trusts.
- **AC-9**: The tunnel carries two ingress rules with two different origins. `*.<app domain>` goes to `ingress-nginx` over HTTPS with `originServerName` set to the bare `<app domain>`, a name the wildcard certificate covers, and with origin verification left on. `DEPLOYER_CONSOLE_HOST` goes straight to the `deployer` Service on port `8080` over plain HTTP, skipping `ingress-nginx` entirely.
- **AC-9a**: Which backend serves a request is decided by the `Host` header, which `cloudflared` passes through unchanged, and is a separate mechanism from `originServerName`, which only proves to `cloudflared` that its TLS peer holds a certificate for that name. The spec states both because conflating them is how an origin looks verified while serving the wrong thing.
- **AC-10**: `cloudflared` runs in its own namespace, `deployer-edge`, with two replicas, so a node drain or a rolling update never leaves zero connectors. The tunnel credential exists in the cluster only as a `SealedSecret` and appears in plain text in no repository.
- **AC-11**: The tunnel is locally managed: its ingress rules live in a `ConfigMap` under `deploy/` in this repository, reviewed like every other manifest, and pinned by a parse test in `internal/config` the way the static network policies already are. Nothing at run time reads that file.
- **AC-12**: Those rules route exactly two hostnames and end in a catch all that returns a refusal. No other name reaches the cluster through the tunnel.

DNS

- **AC-13**: The wildcard record and the console record are proxied Cloudflare records pointing at the tunnel. After the flip, no record in the zone resolves to your home address or to `172.16.70.40`, and the `/32` host route and the tailnet path for apps are retired.
- **AC-14**: From a machine with no Tailscale, the console answers on `https://console.<app domain>` with a certificate a browser accepts and no warning, and a deployed app answers on `https://<slug>.<app domain>` the same way.

The visitor's real address

- **AC-15**: `clientAddress` reads `CF-Connecting-IP` only when the request arrived on `DEPLOYER_CONSOLE_HOST`. On every other host it behaves exactly as it does today, the last hop of `X-Forwarded-For` and then the connection address.
- **AC-15a**: That host gate is backed by network policy rather than trusted on its own. Because the console origin is the `deployer` Service directly, the only pods that may open a connection to `deployer-system` on `8080` are the tunnel namespace, the tailscale proxy, and the two build namespaces, so a request arriving on the console host really did come through the tunnel. This is the reason AC-9 sends the console around `ingress-nginx`: a shared controller is reachable from most of the cluster, and a header trusted behind it is trusted from most of the cluster.
- **AC-15b**: If more than one `CF-Connecting-IP` value arrives, the header is treated as absent and the ordinary derivation is used. Cloudflare sends exactly one, so more than one means something else wrote it.
- **AC-16**: Both surfaces that rate limit, `internal/web` and `internal/httpapi`, derive the address the same way and spend from the same bucket, which is the property the current code already holds and must keep.
- **AC-17**: Migration `00005` adds a nullable `client_address TEXT` column to `audit_log`. The address is carried as a new field on the existing `auth.Audit` value, so every current call site still compiles and each one sets the address where it builds the struct. A platform written row, a suspension sweep, a reconcile drive, a scheduled backup run, leaves it unset, which is written as null.
- **AC-18**: A daily sweep, running on the same in process scheduler `internal/backup` already uses and in the same process, sets `client_address` back to null on every row older than `DEPLOYER_RETENTION_DAYS`, which stays at its existing default of 90. The audit row itself is never deleted. The sweep is one `UPDATE` that scans the whole table once a day with no supporting index, which is the right cost for a table this size and is stated rather than discovered.
- **AC-18a**: After the window a nulled row and a platform written row are indistinguishable. This is accepted, and it is written into the spec so a person reading an old denial knows why the address is missing.

Cookies and origin checking

- **AC-19**: The session cookie is renamed `__Host-deployer_session` and carries `Secure`, `Path=/`, and no `Domain`, so an app on a sibling subdomain can neither read nor shadow it. When `s.secure` is false, meaning plain HTTP, it falls back to `deployer_session` without the prefix and without `Secure`, exactly as `__Host-deployer_csrf` already does, with the same comment saying what a plain HTTP deployment gives up.
- **AC-20**: Every session in existence ends when this deploys, because the cookie name changed. This is one sign in for everybody, and it is the accepted cost rather than a bug.
- **AC-21**: The `Origin` and `Sec-Fetch-Site` check accepts both the console origin and the tailnet origin, and refuses everything else. `s.origin` becomes a set of two rather than one precomputed string, and a post carrying a third origin is still refused.
- **AC-21a**: The pre authentication CSRF cookie from spec 0019 needs no change and gets none. It already carries the `__Host-` prefix, so it is host scoped: each hostname mints its own nonce, and a post still has to carry the cookie that its own host set. Widening the accepted origin set does not let a token minted on one host satisfy a post to the other.

Network policy

- **AC-22**: The `deployer-system` policy gains the tunnel namespace as a peer on container port `8080`, keeps the `tailscale` peer, keeps every existing peer, and stays ingress only. `ingress-nginx` is not a peer, because the control plane is not behind it: apps are, and the console reaches the Service directly. The parse test in `internal/config` is extended to pin the new peer.
- **AC-22a**: The tunnel namespace carries its own policy: outbound to Cloudflare, to `ingress-nginx` on `443`, and to `deployer-system` on `8080`, and inbound from `deployer-system` on the health port only. Nothing else reaches it and it reaches nothing else.

Failure

- **AC-23**: When the tunnel has no ready connectors, the platform sends one mail through the existing Resend path carrying which thing broke and nothing else, and one more when it recovers. It does not send one per check. The check reads `cloudflared`'s own ready endpoint over HTTP through a Service in the tunnel namespace, so this needs no Kubernetes API read and no new `Role` or `RoleBinding` anywhere.
- **AC-23a**: The already told flag lives in memory, not in the database. This is a deliberate exception to the rule that a state transition is a database write first, and it holds because the flag dedupes a notification rather than recording a transition: nothing reads it, nothing branches on it, and the worst a pod restart costs is one extra mail. The exception is written into the code as a comment saying so.
- **AC-24**: That mail leaves the cluster over the platform's ordinary outbound path and does not depend on the tunnel, so a tunnel outage is a thing you are told about rather than a thing that silences the telling.

What does not change

- **AC-25**: A deployed app receives the forwarded chain unchanged. `ingress-nginx` forwards what it received, so an app reads `X-Forwarded-For` and, when it cares, `CF-Connecting-IP`. No app manifest, no controller wide setting, and no Go code changes for this, and the header is documented rather than rewritten. An app has no reason to trust it, and the documentation says so.
- **AC-26**: Cluster administration stays on the tailnet. The Kubernetes API, ArgoCD, Longhorn, and the registry are published by nothing in this feature, and the tunnel's two routes are the proof.

## Options considered

Reasoning and options: see [rationale.md](rationale.md).

## Decision

**Chosen option**: Option 1: A locally managed Cloudflare Tunnel with two origins, and the split enforced in the router.

`cloudflared` runs as a two replica Deployment in its own namespace and dials out to Cloudflare, which fronts exactly two hostnames. The app wildcard goes on through `ingress-nginx` over verified HTTPS, exactly as tailnet traffic does today. The new `console.<app domain>` goes straight to the `deployer` Service, which is what lets network policy prove that a request on that hostname came through the tunnel, and therefore what lets `CF-Connecting-IP` be trusted there. Everything that changes the cluster, the MCP endpoint, the upload endpoint, and every admin page, is absent from the mux on the console host and stays reachable only on the tailnet name. The visitor's address lands in a nullable `audit_log` column a daily sweep clears after the retention window.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `cloudflare` (`~/.claude/skills/cloudflare/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `database-migrations` (`~/.claude/skills/database-migrations/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`)

## Rationale

Reasoning and options: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

| Table | Change | Type | Null | Notes |
|---|---|---|---|---|
| `audit_log` | add `client_address` | `TEXT` | yes | The address the action was attributed to. Null on a platform written row, and null again on any row past the retention window. No index: `audit_log_occurred` still serves every read, and an index here would cost a write on every audited action for a query that has no caller. Carried in Go as a new field on `auth.Audit`, so no call site changes shape. |

No new table. No other table changes shape. Migration `00005`, reversible: the down step drops the column.

**State transitions**: none. This feature adds no lifecycle. The tunnel health flag in AC-23a is deliberately not one.

**Traffic paths**:

| From | Hostname | Through | To | Transport |
|---|---|---|---|---|
| Internet | `<slug>.<app domain>` | Cloudflare, `cloudflared` | `ingress-nginx`, then the app Service | HTTPS to nginx, verified against the wildcard certificate |
| Internet | `console.<app domain>` | Cloudflare, `cloudflared` | the `deployer` Service on `8080` | plain HTTP inside the cluster, fenced by network policy |
| Tailnet | the `ts.net` name | the `tailscale` proxy | the `deployer` Service on `8080` | unchanged from today |
| Internet | anything else | Cloudflare, `cloudflared` | nowhere | refused by the tunnel catch all |

**API surface**:

No new endpoint. What changes is which host answers which existing route.

| Route group | Console host | Tailnet host | Any other host |
|---|---|---|---|
| Public pages (`/`, `/login`, `/register`, `/forgot`, `/reset`, `/unverified`, `/resend`, apps, tokens, settings) | answers | answers | answers |
| Admin pages (`/admin/...`, suspension, invites, backups, token administration) | `404` | answers | answers |
| `/mcp` | `404` | answers | answers |
| `/v1/...` (upload, JSON identity) | `404` | answers | answers |
| `/static/...` | answers | answers | answers |

Expressed as mux patterns per AC-2a: every row marked "answers" on the console host is registered a second time under `<console host>/...`, and one `<console host>/` pattern catches the rest.

**Value sourcing**:

| Action | Value produced or displayed | Source |
|---|---|---|
| Any request | which host it arrived on | `r.Host`, matched by the mux's own host qualified patterns rather than by a separate comparison |
| Any request | the visitor's address | the single `CF-Connecting-IP` value when the host is the console host; otherwise the last hop of `X-Forwarded-For`, then `r.RemoteAddr`, exactly as today |
| Any request | whether that header may be trusted | the console origin path, which network policy restricts to the tunnel namespace (AC-15a) |
| Rate limit check | the bucket key | the address above, derived identically in `internal/web` and `internal/httpapi` |
| Any audited action from a request | `audit_log.client_address` | the new field on `auth.Audit`, set where the struct is built |
| Any audited action the platform initiates | `audit_log.client_address` | the field left unset, written as null |
| Verification mail, reset mail, invite mail | the link a person clicks | derived as `https://` plus `DEPLOYER_CONSOLE_HOST`, never `DEPLOYER_PUBLIC_URL` |
| `deploy_app` tool description | the upload endpoint an agent is told about | `DEPLOYER_PUBLIC_URL`, unchanged, still the tailnet name |
| App creation | whether the slug is refused | the reserved label constant in `internal/domain`, applied to the output of `domain.DeriveSlug` |
| Session cookie | its name | `s.secure`, which is derived from the scheme of `DEPLOYER_PUBLIC_URL`, unchanged |
| POST origin check | the accepted origins | the console origin derived from `DEPLOYER_CONSOLE_HOST`, plus the existing origin from `DEPLOYER_PUBLIC_URL` |
| CSRF pre token check | the nonce a post must match | the `__Host-deployer_csrf` cookie set by the same host, unchanged from spec 0019 |
| Tunnel origin verification | `originServerName` for the app route | the bare `DEPLOYER_APP_DOMAIN`, a name the wildcard certificate covers |
| Tunnel health mail | which thing broke | `cloudflared`'s ready endpoint, reached through a Service in the tunnel namespace |
| Tunnel health mail | whether it was already sent | an in memory flag, per AC-23a |

**Key invariants**:

- A route that changes cluster state is absent from the mux on the console host. Registration is opt in, so a forgotten route is private.
- `CF-Connecting-IP` is read on exactly one hostname, and that hostname is reachable only through the tunnel because network policy says so. The gate and its enforcement live in two different layers on purpose.
- The reserved label set is a constant, not configuration. A scratch instance and the live one refuse the same names.
- `deployer-system` stays ingress only. This feature adds a peer and adds no `Egress` to `policyTypes`, for the reason spec 0019 already gives.
- The tunnel routes exactly two hostnames. Anything else that becomes public is a deliberate edit to a reviewed file.
- The console URL is derived, never configured. There is one place the console's name lives.

**Security model**:

- Everything the open internet can reach is the page surface, and every page that acts still requires the session cookie and the CSRF pair spec 0019 built. Registration stays invite only, which is what keeps an open register page from being an open door.
- The admin surface is unreachable from the internet by routing, and invisible on it by AC-5. Two hostnames means two host scoped sessions, so administering means signing in on the tailnet name. That is the intended consequence.
- The session cookie carries the `__Host-` prefix because the console is now a sibling of subdomains running code the platform did not write. Without it an app can set a parent scoped cookie of the same name and the console reads it, which is session fixation with a deploy as the delivery mechanism.
- The residual risk on `CF-Connecting-IP` is now bounded by policy rather than by hope. Forging it needs a pod in the tunnel namespace, the tailscale namespace, or a build namespace. A build pod is the only one a user influences, it is short lived and it has no session, so the most it could buy is a wrong address on an audit row for an action it cannot take.
- `ingress-nginx` remains a shared controller serving your twelve other cluster apps. An `Ingress` object applied anywhere could still name the `deployer` Service as its backend, which was already true for the tailnet path. It is now stated rather than assumed, and it is one more reason the console does not go through that controller.
- The address in `audit_log` is personal data. It exists for the retention window and is then nulled in place, keeping the trail and dropping the identifier.
- No compliance regime applies. The platform is invite only, under ten people, and holds no payment or health data. The retention rule here is a deliberate choice rather than a requirement imposed on it.

**Configuration required**:

- `DEPLOYER_CONSOLE_HOST`: the public console hostname, required, validated as described in AC-1. The console's base URL is derived from it.
- `DEPLOYER_RETENTION_DAYS`: existing, unchanged, now also governs how long an audit row keeps its address.
- The tunnel credential: a `SealedSecret` in the tunnel namespace, never a value in this repository.

No other new variable. `DEPLOYER_PUBLIC_URL` keeps its meaning and its tailnet value.

**Critical test scenarios**:

- Happy path: from a machine with no Tailscale, the console answers with a trusted certificate and a deployed app answers on its wildcard hostname, verifies **AC-13**, **AC-14**.
- Route split: a request for an admin path, for `/mcp`, and for a `/v1/` path, each with `Host` set to the console host, each answers `404` and changes nothing; the same three on the tailnet host answer normally; a public page answers on both, verifies **AC-2**, **AC-2a**, **AC-3**.
- Address derivation: a request on the console host carrying one `CF-Connecting-IP` is attributed to that address; the same header on the tailnet host is ignored; two values on the console host fall back, verifies **AC-15**, **AC-15b**.
- Bucket sharing: the page surface and the JSON surface derive the same key for the same visitor, verifies **AC-16**.
- Audit: an action from a request writes the address, a scheduled backup run writes null, verifies **AC-17**.
- Retention: a row older than the window is nulled and still present, a row inside it is untouched, verifies **AC-18**.
- Reserved name: creating an app whose derived slug is `console` is refused with `app_name_reserved` over a real client and server session; an app that already holds a now reserved slug still deploys, verifies **AC-6**, **AC-7**.
- Cookie: the session cookie is set as `__Host-deployer_session` with `Secure` and no `Domain` when secure, and unprefixed when not, verifies **AC-19**.
- Origin: a post carrying the console origin is accepted, one carrying the tailnet origin is accepted, one carrying a third is refused; a CSRF nonce minted on one host does not satisfy a post to the other, verifies **AC-21**, **AC-21a**.
- Policy: the parse test reads the new tunnel namespace peer on 8080, finds no `ingress-nginx` peer, and still finds no `Egress` in `policyTypes`, verifies **AC-22**.
- Tunnel config: the parse test reads exactly two hostnames and two distinct origins out of the tunnel `ConfigMap`, verifies **AC-9**, **AC-12**.

The fake clientset resolves no names and the test process is not behind a proxy, so AC-8, AC-9a, AC-10, AC-13, AC-14, AC-15a, AC-22a, AC-23 and AC-26 belong to `/check verify` against the real cluster, not to the suite. Each one that bites is worth a unit test pinning the value afterwards, per the rule in `AGENTS.md`.

## Build plan

The order is the one the scope argued for and the engineer confirmed: everything else is built and proven while the tailnet still carries all traffic, and the DNS change is the single last step. That is a deliberate departure from the usual Tracer Bullet shape for this project, because the thin end to end thread here is the irreversible one.

1. [x] Add `DEPLOYER_CONSOLE_HOST` to `internal/config` with its label validation and the derived console base URL, and thread the derived URL into the mail links, satisfies **AC-1**.
2. [x] Register every public route a second time under the console host pattern and add the `<console host>/` catch all that answers `404`, satisfies **AC-2**, **AC-2a**, **AC-3**, **AC-4**.
3. [x] Hide every admin link, nav entry and button in the templates when the request arrived on the console host, satisfies **AC-5**.
4. [x] Add the reserved label constant and the `app_name_reserved` reason code in `internal/domain`, wire the refusal into the create path only, and add the over the wire test, satisfies **AC-6**, **AC-7**.
5. [x] Change `clientAddress` in both `internal/web` and `internal/httpapi` to read a single `CF-Connecting-IP` on the console host only, keeping one derivation shared by both surfaces, satisfies **AC-15**, **AC-15b**, **AC-16**.
6. [x] Write migration `00005` adding the nullable `client_address` column, regenerate the sqlc queries, add the field to `auth.Audit`, and set it at each request surface call site while platform initiated writes leave it unset, satisfies **AC-17**.
7. [x] Add the daily retention sweep to the existing in process scheduler, nulling addresses past `DEPLOYER_RETENTION_DAYS`, satisfies **AC-18**, **AC-18a**.
8. [x] Rename the session cookie to `__Host-deployer_session` with the plain HTTP fallback, widen the origin check to the two accepted origins, and add the test proving the CSRF pre token stays host scoped, satisfies **AC-19**, **AC-20**, **AC-21**, **AC-21a**.
9. [x] Add the tunnel namespace peer on 8080 to `deployer-system-networkpolicy.yaml` and extend the parse test to pin it and to assert `ingress-nginx` is absent, satisfies **AC-22**.
10. [x] Add the tunnel: namespace, `SealedSecret`, two replica Deployment, the health Service, the routing `ConfigMap` with its two hostnames, its two origins and its catch all, the namespace's own policy, and the parse test pinning the routes, satisfies **AC-9**, **AC-10**, **AC-11**, **AC-12**, **AC-22a**.
11. [x] Move the wildcard `Certificate` from the staging `ClusterIssuer` to the production one in `k3sprox-gitops`, and set `originServerName` on the app route to the bare app domain, satisfies **AC-8**, **AC-9a**. Done 2026-08-11, ahead of this spec: the `Certificate` already names `letsencrypt-prod`, the live secret is signed by a real Let's Encrypt intermediate rather than a staging one, it reports `Ready`, and it covers both SANs. The `ingress-nginx` restart this step warns about has therefore already been paid.
12. [x] Add the tunnel health check reading `cloudflared`'s ready endpoint, its in memory already told flag with the comment explaining the exception, and its failure mail on the existing Resend path, satisfies **AC-23**, **AC-23a**, **AC-24**.
13. [x] Document the forwarded header an app receives, and change nothing in the app path, satisfies **AC-25**.
13a. [x] Move the tunnel out of the `cloudflared` namespace and into `deployer-edge`, and add that namespace as a destination on the `deployer` AppProject. Found during the build rather than designed: the cluster already runs a remotely managed tunnel called `cloudflared` in a namespace of that name, owned by `k3sprox-gitops` and carrying the routes for every other app here. This spec's manifests would have put two ArgoCD applications on one `Deployment` and replaced the connector those apps depend on. The AppProject destination was missing too, which fails the whole `deploy/` sync rather than the one object. Neither is visible to the suite, and neither was in the spec.

14. [ ] Prove every step above from a tailnet device with the tunnel already live but no public DNS pointing at it, using a host header override, then change the wildcard and console records to the proxied tunnel records and retire the `/32` host route, satisfies **AC-13**, **AC-14**, **AC-15a**, **AC-26**. The first half is done, 2026-08-16: against the control plane Service on `8080`, the same origin the tunnel uses, `Host: console.deploy.toyintest.org` answers `303` on `/`, `200` on `/login`, `/register` and `/static/`, and `404` on `/admin/invites`, `/mcp` and `/v1/uploads`, while the tailnet host answers all of them; the console login page renders no admin link; `cloudflared` runs `2/2` in `deployer-edge` with four ready connectors; and `wildcard-apps` is issued by `letsencrypt-prod` and reports `Ready` over both names. The DNS change and the `/32` retirement are deliberately not done: the zone still resolves every name to `172.16.70.40`.

## Migration plan

**Strategy**: strangler, with one irreversible cutover at the end.

**Phases**:
1. [x] Build steps 1 to 8. Go and schema changes only. Deployable on their own, and none of them is visible from outside the tailnet. The session cookie rename in step 8 signs everybody out once; do it in the same deploy as the rest rather than on its own.
2. [x] Build steps 9 to 13. The policy peer, the tunnel, the production certificate, and the health mail. The tunnel is live and the console is reachable through it by hostname override, while public DNS still points nowhere near it. Both paths run side by side, which is the whole point of doing it this way.
3. [x] Build step 14. Change the two DNS records. This is the irreversible step and it is one action.

**Rollback**:
- Phase 1 and phase 2 roll back by reverting the commit and letting ArgoCD reconcile. Nothing outside the cluster has changed.
- Phase 3 rolls back by pointing the wildcard record back at `172.16.70.40` and re advertising the `/32` host route. That is a real rollback, but it is slower than the flip was, because DNS is cached and the route has to be approved again in the Tailscale admin console. Treat phase 3 as the point of no easy return.
- The migration rolls back cleanly on its own: the down step drops one nullable column that nothing else reads.

**Risks**:
- The certificate move from staging to production restarts the shared `ingress-nginx` controller, which briefly interrupts TLS for the twelve other apps on your cluster. Spec 0003 flagged this the first time and it is the same edge again. Do it deliberately, not as a side effect of another change.
- Getting AC-2 wrong publishes the deploy path. Putting it in the mux rather than in a middleware means the failure is a missing registration, which makes a route private, rather than a missing condition, which makes it public.
- A wrong `originServerName` makes the app route fail closed. That is the good failure, but it looks like a Cloudflare outage from outside, so check it before blaming the vendor.
- The console origin is plain HTTP inside the cluster. It is a pod to pod hop across the Cilium network, fenced by policy on both ends, and it is a real reduction from the app path's verified TLS. Named here rather than buried.
- The `/32` host route and the tailnet path for apps are retired in phase 3. If the tunnel is down after that, apps are unreachable from everywhere including your own LAN, which is the price of the single path decision.

## Consequences

**Positive**:
- A person can join, sign in, and use the platform without installing anything, which removes the largest of the four steps standing between a new person and their first deploy.
- A deployed app is a link you can send, which is what the platform was for.
- Your home address is in no DNS record and no certificate, and no port is opened on your router.
- The rate limiter finally sees a real address, so one abuser is one bucket rather than everybody sharing the tunnel's.
- The console stops depending on a controller shared with twelve unrelated apps, which is a smaller blast radius than it had on the tailnet.
- Every path, tailnet and LAN included, stops warning on its certificate.

**Negative / tradeoffs**:
- Cloudflare is now in the path of everything public. Their outage is your outage, and you will read about it on their status page rather than in your own logs.
- The console origin hop is unencrypted inside the cluster. Policy on both ends is what makes that acceptable, and policy is a thing that can be edited wrongly.
- The console and the apps now take two different paths into the cluster, so a symptom on one does not tell you anything about the other. Two paths is more to hold in your head than one.
- Reaching your own app from your own sofa now leaves the house and comes back. Accepted deliberately: one record with one behaviour beats two answers for one name that drift.
- The MCP and upload endpoints stay on the tailnet, so an agent still needs Tailscale to deploy. This feature makes the platform usable without a VPN and does not make it deployable without one. That gap is real and belongs to a later decision.
- Two hostnames means two sessions. Administering means signing in again on the tailnet name, every time.
- Everybody is signed out once when the cookie is renamed.
- The tunnel is a new thing to run, a new credential to hold, and a new failure mode to recognise.
- A nulled old audit row and a platform written one are indistinguishable. The alternative was deleting the row, which loses more.
- Every public route has to be registered twice. That is duplication, and it is the price of having one routing table instead of a table plus a classifier.

**Neutral**:
- The tunnel's routing lives in this repository, so what is public is reviewable and testable, at the cost of a deploy to change a route.
- `DEPLOYER_PUBLIC_URL` narrows to one job, the address an agent is told about, and stops being the address a person clicks.
- The app path itself does not change. No app manifest, no controller wide setting, no redeploy.
- The tunnel health check needs no Kubernetes API access, so this feature grants the platform no new cluster rights at all.

## Follow-up

- [ ] The MCP and upload endpoints are still tailnet only, so an agent cannot deploy without Tailscale. Publishing them means deciding what to do about a 100 MB tarball through a proxy that caps a request body at 100 MB on the free plan, which is a body size decision rather than a routing one. Worth its own spec.
- [ ] `deploy/AGENTS.md` says the `tailscale` ingress class is for the control plane's own Ingress and that apps must never use it. That line stays true here, but the same file should also say that the console reaches the control plane through the tunnel rather than through `ingress-nginx`, or the next reader will assume the console is behind nginx like everything else. Worth updating in the same change rather than leaving `/sync` to find the gap.
- [ ] Feature 23, the ready to paste agent configuration, assumed the console is where a newly verified person lands. That is now true, and its page is public, so the token it mints is minted over the public edge. Worth checking that assumption when that feature is designed.
- [ ] The tunnel health check watches whether connectors are ready, which catches the tunnel going away and not the tunnel being misrouted. A route pointing at the wrong backend looks healthy. Worth a real request check later, once there is somewhere for one to run.
- [ ] `DEPLOYER_RETENTION_DAYS` is validated and configured but, at the time of writing, no code reads `cfg.Retention`. Step 7 makes it load bearing for the first time. Worth confirming during the build whether the deployment event pruning it was written for ever shipped, and saying so either way.
- [ ] The `cloudflare` and `security-patterns` skills shaped this design and are not referenced in root `AGENTS.md` under `## Agent skills`. The Cloudflare one is area specific enough to belong in `deploy/AGENTS.md` rather than at root, where it would cost context on every task.
- [ ] Cluster wide alerting is still absent, so the tunnel mail here is the second bespoke notification the platform grows, after the backup one. A third is the point at which this stops being reasonable.
