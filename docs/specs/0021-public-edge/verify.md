# Verify: public edge · spec 0021 · updated 2026-08-15
_Steps derived from spec 0021 acceptance criteria and its Value sourcing table. `/check verify` runs these; `/test` locks the durable ones._

Read this in three passes. The **suite** section is already green and is here so a
reader knows what is covered without a cluster. The **tailnet** section is the one
to run before the flip, with the tunnel live and public DNS still pointing nowhere
near it. The **after the flip** section is the only part that cannot be undone
quickly.

## Commands (already green in the suite)

- [x] `go test -race ./...` → passes                                             → AC-1, AC-2, AC-2a, AC-3, AC-4, AC-5, AC-6, AC-7, AC-15, AC-15b, AC-16, AC-17, AC-18, AC-18a, AC-19, AC-21, AC-21a, AC-23, AC-23a
- [x] `go test ./internal/config/ -run Tunnel` → the tunnel routes exactly two hostnames, two distinct origins, a refusing catch all, and its namespace is fenced both ways → AC-9, AC-11, AC-12, AC-22a
- [x] `go test ./internal/config/ -run ControlPlane` → the fence carries four peers including `deployer-edge` on 8080, names no `ingress-nginx` peer, and still has no `Egress` in `policyTypes` → AC-22
- [x] `kustomize build deploy/ > /dev/null` → builds, so every new manifest is listed in the kustomization and ArgoCD will see it

## Before the flip: from a tailnet device, tunnel live, DNS unchanged

The console hostname does not resolve publicly yet, so every step here overrides
the Host header or resolves the name by hand. That is the whole point of this
pass: it proves the split without publishing anything.

### The tunnel is up

- [x] `kubectl -n deployer-edge get deploy cloudflared` → 2/2 ready, so a node drain never leaves zero connectors → AC-10
- [x] `kubectl -n deployer-edge get pods -o wide` → the two are on different nodes
- [x] `kubectl -n deployer-edge get secret cloudflared-credentials` → exists, and `git grep -i 'credentials.json:' deploy/` shows only the sealed value, never plaintext → AC-10
- [x] `kubectl -n deployer-edge logs deploy/cloudflared | grep -i 'registered tunnel connection'` → connectors registered
- [x] `kubectl -n deployer-system run probe --rm -it --image=curlimages/curl --restart=Never -- curl -s http://cloudflared.deployer-edge.svc.cluster.local:2000/ready` → 200, which is the same endpoint the health check reads → AC-23

### The route split, by Host header override

Run these against the platform's tailnet address with the console Host forced, so
the mux sees a console request without any DNS involved.

- [x] `curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: console.deploy.toyintest.org' https://deployer.tail62ceef.ts.net/login` → 200 → AC-2a
- [x] the same with `/admin/accounts` → 404 → AC-2
- [x] the same with `-X POST .../mcp` → 404 → AC-2
- [x] the same with `/v1/auth/me` → 404 → AC-2
- [x] the same three **without** the Host override, on the tailnet name → not 404: the tailnet stays a complete door, so a tunnel outage never costs you access to your own platform → AC-3
- [x] `curl -s -o /dev/null -w '%{http_code}\n' http://<control plane pod IP>:8080/login` from inside the cluster → 200, so an in cluster caller and a health probe still work → AC-4

### The console hostname through the tunnel, before DNS

- [x] `curl -sv --resolve console.deploy.toyintest.org:443:<a Cloudflare edge address> https://console.deploy.toyintest.org/login` → the sign in page, over a certificate the client accepts → AC-9 · run against real public DNS after the flip rather than with `--resolve`: `200` with `<title>Sign in · Deployer</title>` and a clean verify, and the same page rendered in a real browser through Playwright
- [x] the same for `/admin/accounts` → 404, proving the split holds through the real path and not only in the mux → AC-2 · `404` through the edge, together with `/admin/invites`, `/mcp` and `/v1/uploads`
- [x] `curl -sv --resolve <slug>.deploy.toyintest.org:443:<the same edge address> https://<slug>.deploy.toyintest.org/` → the app answers, so the wildcard route reaches `ingress-nginx` with `originServerName` accepted → AC-9, AC-9a · `hello-4dfssb`, `gohello-df28mf` and `verify-go-7rmxp3` each `200` through the edge
- [x] the same for a hostname the tunnel does not route, such as `nothing.deploy.toyintest.org` → 404, and nothing of the platform → AC-12 · **the step as written asks for the wrong thing and cannot be run.** A single label name under the app domain matches the wildcard rule, so it is routed to `ingress-nginx` and 404s there: `nothing.deploy.toyintest.org` answers `404` from nginx's default backend, which is the cluster, not the catch all. The catch all is unreachable from outside altogether, because Cloudflare refuses first: a name outside the zone answers `530` error 1016 at the edge, and a two label name such as `a.b.deploy.toyintest.org` resolves but fails TLS, since the wildcard certificate covers one label. So AC-12 holds in the only form it can be observed in, which is stronger than the step's wording, and no name reaches the cluster through this tunnel except the two the rules name

### The visitor's address

- [x] a request through the real console path, then read `audit_log` back → the row carries your public address, not `172.16.70.40` and not a `10.42.x` pod address → AC-15, AC-17 · **done 2026-08-16.** A sign in was not needed and a failed one writes no row anyway: a refused form on the console host writes a `page_csrf` row, and the one from a real browserless request through the tunnel carries `client_address = 77.164.220.19`, the real visitor. Read with a read only ephemeral container beside the control plane, copying the three SQLite files out of `/proc/1/root/data` and querying the copy, since the pod is distroless and the platform exposes no audit surface
- [x] do the same on the tailnet name → `client_address` is the tailnet hop, never a `CF-Connecting-IP` value, because the header is read on one hostname only → AC-15 · driven in cluster against the control plane pod with the tailnet `Host` and `CF-Connecting-IP: 198.51.100.44` set: the row carries `10.42.2.145`, the calling pod, and never the header value
- [x] repeat the console request with two `CF-Connecting-IP` values → the row carries the real address rather than either, because more than one value is treated as absent → AC-15b · with the console `Host` and one value the row carries `198.51.100.11`; with `198.51.100.22` and `198.51.100.33` together the same shape of request carries `10.42.2.145`, the caller. Note that this can no longer be driven through the real edge: Cloudflare now refuses a client that sets `CF-Connecting-IP` itself with a `403` error 1000 before the request reaches the tunnel, so the earlier synthetic address technique is gone and this is the in cluster equivalent
- [ ] a platform written row leaves `client_address` null → AC-17 · **the step as written cannot be run.** A backup run writes to `backup_runs`, not to `audit_log`, so there is no backup audit row to read: nothing in `internal/backup` calls `auth.Record`. What is observable is the same property on the other platform paths, since `internal/mcp` and `internal/suspend` build their `auth.Audit` without a `ClientAddress` at all: the five `deploy` rows written since the migration all carry null while the request sourced rows beside them carry an address. Either fix the step to name a path that really writes one, or accept the unit test for this half
- [ ] `SELECT COUNT(*) FROM audit_log WHERE client_address IS NOT NULL AND occurred_at < datetime('now','-90 days')` → 0, and the total row count is unchanged, so the sweep nulls without deleting → AC-18, AC-18a · the query answers `0` against 1246 rows, but **vacuously**: the oldest row in the table is 2026-08-12, four days old, so nothing is inside the window yet and the sweep has had nothing to do. The sweep itself is confirmed running, `audit address sweep started interval=24h retention=2160h`. This step is not provable on real data until the table is three months old, and the unit test is what covers it until then

### The rate limiter sees one visitor, not the tunnel

- [x] both sign in surfaces apply the account lockout, so the browser is not a softer way in → AC-5 · **this failed when it was first driven, on 2026-08-16, and the fix is `4bd0258`.** The lockout lived in the JSON handler alone and `svc.Login` touched the limiter nowhere, so the browser counted no failures and checked no penalty. Spec 0021 is what made it matter: the JSON surface is 404 on the console hostname, so the surface with no lockout was the one on the open internet. Before: JSON gave five `401`s then `429`, while the browser gave eight `401`s for the same address, including while that address was already locked out. After, on the deployed digest `sha256:991fa8c7`: through the real public edge, five `401`s then `429`; on a fresh address with a **different** `CF-Connecting-IP` per attempt, so the ten token bucket cannot account for it, five `401`s then `429`; and an address locked out by the JSON surface is refused `429` by the browser on its first attempt, which is the shared state the criterion asks for

- [x] from one machine, fail sign in through the console repeatedly until it answers 429 → AC-16
- [x] immediately post to `/v1/auth/login` from the same machine → also refused, because both surfaces spend from one bucket → AC-16
- [x] from a second machine on a different address, sign in through the console → not refused, so one abuser is one bucket rather than everybody → AC-16
- [x] re confirmed against the live deployment on 2026-08-16, after the flip, using synthetic `CF-Connecting-IP` values so no real address entered a bucket or a lockout. Posting `/forgot` with `CF-Connecting-IP: 203.0.113.10` on the console host gave ten `200`s and then `429`, matching `bucketCapacity = 10` exactly; `203.0.113.20` on the same host was still `200`, so buckets are per visitor rather than shared; `203.0.113.10` again was `429`, so the key really is the header value; and the same header on the tailnet host was `200`, so it is read on the console hostname and nowhere else → AC-15, AC-16

  Worth stating because it is the question the `X-Forwarded-For` finding under AC-25 raises: the limiter is unaffected by nginx rewriting that header, because the console never passes through nginx. The tunnel sends console traffic straight to the `deployer` Service, so the platform reads `CF-Connecting-IP` as Cloudflare set it. Nothing on the cluster uses nginx `limit-rps`, `limit-connections` or a source allowlist annotation, so no app is currently bucketing on the rewritten value either

### Cookies and origins

- [x] sign in on the console, read the `Set-Cookie` → `__Host-deployer_session`, with `Secure`, `Path=/`, and no `Domain` → AC-19 · **done 2026-08-16**, a real sign in through the public edge in a real browser. The jar holds `__Host-deployer_session`, host only on `console.deploy.toyintest.org`, `Path=/`, `Secure`, `HttpOnly`, `SameSite=Lax`, and `document.cookie` is empty, so the prefix, the flags and the absence of a `Domain` are all confirmed by the browser having accepted it at all. But read the next line before treating this criterion as delivering what it promises
- [x] everyone who was signed in before this deploy is signed out exactly once → AC-20 · **failed when it was first driven, on 2026-08-16, and the fix is `ae2a7350`, verified live on the deployed digest `sha256:9a1e1a39`.** Four checks through the real public edge, in clean browser contexts. A live session id replayed under the plain name scoped to `.deploy.toyintest.org`, which is what a stranger's app under the wildcard can set, answers the sign in page. The same id under the plain name host only on the console answers the sign in page. The tailnet host, still holding its pre rename `deployer_session` and nothing else, answers the sign in page, so the one sign in for everybody finally happened. And the control that makes those three mean anything: **the same id under `__Host-deployer_session` still answers `/apps`**, so what is being refused is the name and not a dead session. The account's own `__Host-` session survived the deploy, which is correct, since only sessions riding the plain name were ever at risk.

  What was wrong, kept because the shape of it is the lesson:

  **It took AC-19's protection with it.** `auth.SessionID` in `internal/auth/session.go` read **both** cookie names, secure first then plain, so a session minted before the rename still authenticated. Observed: the tailnet host served `/apps` signed in while the only cookie it held was a pre rename `deployer_session`, which that binary cannot have written, since the same binary wrote `__Host-deployer_session` on the console minutes earlier. Nobody had been signed out.

  The security half is worse than the criterion. `deployer_session` carries no `__Host-` prefix, so unlike the name the platform now writes, it **can** be set with a `Domain` attribute. An app deployed by a stranger on `<slug>.deploy.toyintest.org` can set `deployer_session` scoped to `.deploy.toyintest.org`, and the console reads it. Driven on 2026-08-16 in a clean browser context holding no `__Host-` cookie at all, only `deployer_session` at `Domain=.deploy.toyintest.org`: `https://console.deploy.toyintest.org/apps` answered `200` with the signed in page. That is precisely the sibling subdomain session fixation the prefix was added to close, reopened by the read side.

  The ordering limits it rather than saving it: the prefixed name is tried first, so a victim currently holding a good `__Host-` cookie is not overridden. It lands on a victim who is signed out, or whose cookie has expired, who then works inside the attacker's account.

  **The fix.** `SessionID` takes `secure` and reads the one name `SessionCookieName` selects, exactly as the write does, and both callers pass the value they already held. Pinned by `TestASessionIsReadUnderOneNameOnly` and `TestAParentScopedPlainCookieIsNotASession` in `internal/auth/session_test.go`, and the assertion in `internal/web/cookieorigin_test.go` that used to require both names to resolve is now the opposite assertion, since that test was the bug written down as an expectation. The comment on `SessionID` argued that reading one name "would sign everybody out on every request rather than once", and signing everybody out once is what AC-20 asks for, so that reasoning was the bug.

  Two things to keep. A criterion whose whole content is "this old thing stops working" is invisible to a suite: every test here wrote its own cookie and read it back, so both names resolving looked correct from inside and only a browser holding a cookie older than the deploy could see it. And a fallback that exists because a reader "does not hold the scheme" is worth suspecting whenever the two names it falls back between are not equally protected: the prefix on one of them was doing real work that the other silently undid
- [x] post a form with `Origin: https://console.deploy.toyintest.org` → accepted; with the tailnet origin → accepted; with a third → 403 → AC-21
- [x] take the `__Host-deployer_csrf` value minted on the tailnet host and post it to the console → refused, because the cookie is host scoped and each hostname mints its own → AC-21a · **done 2026-08-16, and worth reading before trusting the wording.** The two hosts do mint different nonces, confirmed in one pair of `GET /login` calls, and a post carrying one host's nonce with the other host's token is refused `403` with "That form expired before it was submitted". But a **whole** pair carried across by hand is accepted: the token is `HMAC(nonce)` under one key with no host in it, so the server cannot tell where the pair came from, and the tailnet pair posted to the console reached the credential check and answered `401`. That is the design, not a gap. The protection is the `__Host-` prefix, which stops a **browser** ever sending one host's cookie to the other, so an attacker cannot obtain a matching pair for the victim's host. curl is not the threat model, and nothing here changes if the two hosts are widened further

### The reserved name

- [x] `deploy_app` with `name: "console"` over a real MCP session → refused with `app_name_reserved`, and no app row is written → AC-6 · **done 2026-08-16**, over a real streamable HTTP session against the tailnet address on the deployed digest `sha256:714c06dd`. `app_name_reserved: that name is reserved by the platform, so pick another one`, and `list_apps` before and after carries the same nine slugs, so nothing was written
- [x] the same with `name: "Console"` and `name: "console!"` → refused, because the check runs on the derived base → AC-7 · both answered `app_name_reserved` in the same session, so the refusal survives a capital and a character the slug derivation strips
- [x] the same with `name: "console-shop"` → accepted → AC-7 · accepted in the same session as `console-shop-n2tq6b`, built through the dockerfile path, reached `healthy` at release 1 on digest `sha256:b10cc2fe`, and was deleted afterwards
- [ ] an app that already holds a now reserved slug still deploys, rolls back and lists → AC-7 · **no such app exists to run it against.** The check has been live since this feature deployed and every one of the nine apps on the account derives a base outside the reserved set, so there is no grandfathered row to exercise. The unit test in `internal/domain/reserved_test.go` is what covers it, and this step only becomes runnable if a label is added to `reservedLabels` that an existing app already holds

  **Read this before running the three steps above.** They are unreachable on an account at its app ceiling, and that is ordering rather than a defect. `resolveApp` in [internal/mcp/mcp.go](../../../internal/mcp/mcp.go) reads the account's app count and refuses with `app_limit_reached` **before** it calls `apps.Create`, and the reserved check lives inside `CreateApp` in [internal/store/apps.go](../../../internal/store/apps.go). Driven first on 2026-08-16 with the account holding 15 apps against a limit of 10, all four calls answered `app_limit_reached (15 of 10 used)` and the reserved check never ran. Six throwaway probe apps from earlier verification runs were deleted (`probeone`, `probetwo`, `noisy2`, `budget-hold`, `budget-probe-a`, `slowbuild`), taking the account to nine, and the steps then ran as written. Both refusals are honest, so nothing here is worth changing in the code, but a future run of these steps has to check the app count first or it reads a false pass as a pass

### Network policy, walked rather than parsed

The parse tests cannot tell you a peer is **missing**, because a shorter policy is
still a valid policy. Only this walk can, which is the lesson spec 0019 recorded.

- [x] from a pod in `deployer-edge`, reach `deployer.deployer-system.svc:80` → succeeds → AC-22
- [x] from a pod in `default` or any unrelated namespace, reach the control plane on 8080 → refused, which is what makes `CF-Connecting-IP` trustworthy on the console hostname → AC-15a
- [x] from a pod in `ingress-nginx`, reach the control plane on 8080 → refused: the console is not behind that controller and must not be reachable from it → AC-15a, AC-22
- [x] from a pod in `deployer-edge`, reach anything other than DNS, `ingress-nginx:443`, `deployer-system:8080` and the public internet on 443/7844 → refused → AC-22a · **done 2026-08-16**, walked from a throwaway pod in that namespace carrying `app: cloudflared`, so it picks up the same allow policy the connector does. Permitted and reached: cluster DNS, the control plane pod on 8080, `ingress-nginx` on 443, the public internet on 443. Refused, every one timing out rather than being refused, which is what a dropped packet looks like: `ingress-nginx` on 80, the registry pod on 5000, the control plane pod on 5000, the public internet on 80, the Kubernetes API on `172.16.70.20:6443`, and SSH on two node addresses. One result that looks like a hole and is not: `172.16.70.40:443` connects, because that is the `ingress-nginx` LoadBalancer address and Cilium translates it in eBPF to the controller pod before policy is read, so it lands on the peer the policy already permits. That is the same trap the manifest's own comment names for a ClusterIP, and it means the `172.16.0.0/12` exception on the public rule cannot be tested through a Service address
- [x] from a pod in `deployer-system`, reach `cloudflared.deployer-edge.svc:2000` → succeeds; from anywhere else → refused → AC-22a
- [x] the tailnet peer, the two build namespaces, and the control plane pod on 5000 all still work, so adding a peer took none away → AC-22 · **done 2026-08-16.** The tailnet name answers `200` on `/login`. A probe in `deployer-builds` and one in `deployer-builds-dockerfile` each reach the control plane pod on 8080 (`200`) and the registry pod on 5000 (`401`, so the connection lands and the registry asks for credentials). The control plane's own peering on 5000 was walked with a probe in `deployer-system` labelled `app: deployer`, which is the identity the rule names, and it reaches the registry the same way. That pod needs a readiness probe that never passes, or the `deployer` Service adopts it as an endpoint and console traffic starts landing on a curl container

### The certificate (still owed, in k3sprox-gitops)

- [x] move the `wildcard-apps` `Certificate` from the staging `ClusterIssuer` to the production one → AC-8
- [x] `kubectl -n ingress-nginx get certificate wildcard-apps` → `Ready`, covering both `*.deploy.toyintest.org` and the bare domain → AC-8
- [x] every path, tailnet and LAN included, serves a certificate a browser accepts with no warning → AC-8
- [ ] expect the shared `ingress-nginx` controller to restart, which briefly interrupts TLS for the twelve other apps on the cluster. Do this deliberately, not as a side effect of another change.

### Failure

**Pause three ArgoCD applications before any of this, not one.** Tried on 2026-08-16 pausing only `deployer`: the scale to zero was self healed one second later, the connectors never went down, the console never stopped answering, and no mail was owed. Pausing `deployer` alone is not enough, because **`root` puts `deployer`'s own `automated` block straight back**, within about a minute, and then `deployer` heals the Deployment again. The owner of the `cloudflared` Deployment is the `deployer` application (checked by walking every application's `status.resources`), so `cloudflared-resources` in the gitops repo is not the one to pause, though pausing it too costs nothing. Pause `root` **first**, then the other two:

```bash
for a in root cloudflared-resources deployer; do
  kubectl -n argocd patch application $a --type=merge -p '{"spec":{"syncPolicy":{"automated":null}}}'
done
```

Record each one's whole `syncPolicy` first, because the three differ in their `syncOptions` (`root` carries `ApplyOutOfSyncOnly=true`, `cloudflared-resources` carries `CreateNamespace=true` and `ServerSideApply=true`, `deployer` carries `ServerSideApply=true`), and put each back exactly as it was afterwards.

- [x] `kubectl -n deployer-edge scale deploy/cloudflared --replicas=0` → within a few minutes exactly one mail arrives naming which thing broke and nothing else → AC-23 · **done 2026-08-16.** Scaled to zero at 06:51:07Z, the watcher logged `the public edge has no ready connectors` at 06:51:19Z, twelve seconds later, and one mail arrived at 06:51:20Z, subject `Deployer: the public edge is down`. The public console and `hello-4dfssb` both answered `530` from Cloudflare while the tailnet name still answered `200`, which is the outage the mail describes
- [x] leave it down → no further mail, because the flag dedupes the notification → AC-23a · left down from 06:51 to 07:02, about eleven minutes and five watcher ticks at the two minute interval. Exactly one ERROR line in the platform log and one mail for the whole window, up to the control plane restart below
- [x] scale back to 2 → exactly one recovery mail → AC-23 · scaled back at 07:02:09Z, connectors ready 2/2 at 07:02:32Z, and one mail at 07:03:03Z, subject `Deployer: the public edge is back`, on the very next tick. The console and the app both answered `200` again
- [x] confirm both mails arrived while the tunnel was down, which proves the telling does not depend on the thing it reports on → AC-24 · **the wording asks for something only half observable, and the observable half is what matters.** A recovery mail cannot arrive while the tunnel is down, since it is sent because the tunnel came back. What is provable is that every mail sent *during* the outage got out: two down mails, 06:51:20Z and 07:01:03Z, both delivered with the connectors at zero and the public edge answering `530`. Mail leaves through Resend over ordinary egress rather than through the tunnel, so the telling is independent of the thing it reports on
- [x] restart the control plane pod while the edge is down → at most one extra mail, which is the whole cost of the in memory flag → AC-23a · restarted at 06:59:01Z with the edge still down, the new pod logged `tunnel health check started` at 06:59:03Z, and its first tick sent one further down mail at 07:01:03Z. Exactly one extra, and the ten minutes of outage either side of it produced none, so the flag is doing its job and a restart is the only thing that resets it

## After the flip: the irreversible step, done last and alone

- [x] change the wildcard record and the console record to proxied Cloudflare records pointing at the tunnel → AC-13
- [x] `dig +short console.deploy.toyintest.org` and `dig +short anything.deploy.toyintest.org` from off network → Cloudflare addresses only → AC-13 · both answer `172.67.153.85` and `104.21.72.176`
- [x] `dig` every record in the zone → none resolves to your home address or to `172.16.70.40` → AC-13 · the bare `deploy.toyintest.org` A record is deleted and now answers empty; mail stays on `send.deploy.toyintest.org`, untouched
- [x] retire the `/32` host route and the tailnet path for apps → AC-13 · pfSense advertises only `172.16.42.0/24` and `172.16.60.0/24`
- [x] **from a machine with no Tailscale**, open `https://console.deploy.toyintest.org` → the sign in page, certificate accepted, no warning → AC-14 · failed first on 2026-08-16 with a `404` from `ingress-nginx`, because the tunnel listed `*.deploy.toyintest.org` before the console rule and a leading `*.` matches the single label `console`, so the console rule was never reached. Fixed by putting the console rule first, and pinned by `TestNoTunnelRuleIsShadowedByAnEarlierOne`. Now answers `200` with `<title>Sign in · Deployer</title>`, certificate Let's Encrypt `CN=console.deploy.toyintest.org` valid to 14 Nov 2026
- [x] the same machine opens `https://<slug>.deploy.toyintest.org` → the app answers the same way → AC-14 · `hello-4dfssb`, `gohello-df28mf` and `verify-go-7rmxp3` each answer `200` with `ssl_verify=0` through the Cloudflare edge
- [x] the same machine tries `/admin/accounts`, `/mcp` and `/v1/uploads` on the console hostname → 404 on all three → AC-2 · through the real edge after the rule order fix: `303 /`, `200 /login`, `200 /register`, `404 /admin/invites`, `404 /admin/accounts`, `404 /mcp`, `404 /v1/uploads`, and the sign in page renders no `/admin` link. These are real refusals now, not the earlier false pass where nothing reached the console at all
- [x] the same machine tries the Kubernetes API, ArgoCD, Longhorn and the registry by every name you can think of → nothing answers: cluster administration stays on the tailnet, and the tunnel's two routes are the proof → AC-26 · `argocd`, `longhorn`, `registry`, `k8s` and `kubernetes` under the app domain all answer `404` from the default backend, and names outside the zone do not resolve
- [x] a deployed app reads `X-Forwarded-For` and `CF-Connecting-IP` and sees the forwarded chain unchanged, with no app manifest, no controller wide setting and no redeploy → AC-25 · proved with a throwaway `traefik/whoami` app hand applied in the shape of `deploy/hello-world.yaml`, then deleted. Through the tunnel it received `CF-Connecting-IP: 77.164.220.19`, the real visitor address, and `Cf-Ipcountry: NL`, with nothing in this feature rewriting either and no controller setting added.

  **Read this before trusting `X-Forwarded-For` on the tunnel path.** The same request arrived with `X-Forwarded-For: 10.42.2.29` and `X-Real-Ip: 10.42.2.29`, which is the `cloudflared` pod, not the visitor. `ingress-nginx` sets both to its own immediate peer, because `use-forwarded-headers` is deliberately not set (that is the "no controller wide setting" the criterion asks for, and setting it would change behaviour for the twelve other apps on the shared controller). The contrast is the proof: the same app hit directly on the LAN, bypassing Cloudflare, saw `X-Forwarded-For: 172.16.50.100`, the real client, because that was nginx's peer that time. So on the public path `CF-Connecting-IP` is the only header carrying the visitor, and the sentence in [deploy/AGENTS.md](../../../deploy/AGENTS.md) that says nginx "forwards what it received, so an app reads `X-Forwarded-For`" is wrong for this path and wants a `/sync` pass

## Value sourcing coverage

One step per row of the spec's Value sourcing table, exercising the edge that
breaks if the source is wrong.

- which host a request arrived on → the Host header override steps, on three different hosts
- the visitor's address → the console, tailnet and doubled header cases above
- whether the header may be trusted → the network policy walk from an unrelated namespace
- the rate limit bucket key → the two surface, two machine limiter steps
- `audit_log.client_address` from a request → the sign in then read back step
- the same from a platform initiated write → the scheduled backup run step
- the link in a verification, reset or invite mail → **done 2026-08-16** → AC-1 · driven with Playwright against `https://console.deploy.toyintest.org/forgot` over the public edge, which answered `Check your email`. Registration itself was not usable for this, since it is invite only and minting an invite needs an admin session. The mail that arrived carries `https://console.deploy.toyintest.org/reset?token=HBwF0kde...`, and every earlier reset in the same Gmail thread carries `https://deployer.tail62ceef.ts.net/reset?token=...`, so the switch to the derived console base URL is visible in one thread
- the `deploy_app` upload endpoint → confirm the tool description still names the tailnet `DEPLOYER_PUBLIC_URL`, unchanged
- whether a slug is refused → the reserved name steps
- the session cookie's name → the `Set-Cookie` step, on both a secure and a plain HTTP deployment
- the accepted POST origins → the three origin cases
- the CSRF pre token → the cross host nonce step
- `originServerName` → the app hostname through the tunnel, which fails closed if it is wrong
- which thing broke → the scale to zero step
- whether it was already told → the leave it down step

## Acceptance-criteria coverage

- AC-1 … config tests, and the mail link check after the flip · AC-2, AC-2a … the route split by Host override and through the real path · AC-3, AC-4 … the tailnet and other host steps · AC-5 … the suite, plus the two surface lockout comparison against the live cluster, which is what caught it · AC-6, AC-7 … the reserved name steps over a real session · AC-8 … the certificate section, still owed · AC-9, AC-9a … the app and console hostnames through the tunnel · AC-10 … the connector steps · AC-11, AC-12 … parse tests plus the unrouted hostname · AC-13, AC-14 … after the flip · AC-15, AC-15b … the address derivation steps · AC-15a … the network policy walk · AC-16 … the limiter steps · AC-17, AC-18, AC-18a … the audit and sweep reads · AC-19, AC-20, AC-21, AC-21a … cookies and origins · AC-22, AC-22a … the policy walk · AC-23, AC-23a, AC-24 … the failure section · AC-25 … the app header step · AC-26 … the administration step after the flip
