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
- [ ] the same for a hostname the tunnel does not route, such as `nothing.deploy.toyintest.org` → 404 from the tunnel's catch all, not from the cluster → AC-12

### The visitor's address

- [ ] sign in through the console path above, then `sqlite3 /data/deployer.db "SELECT action, client_address FROM audit_log ORDER BY id DESC LIMIT 5"` → the login row carries your public address, not `172.16.70.40` and not a `10.42.x` pod address → AC-15, AC-17
- [ ] do the same on the tailnet name → `client_address` is the tailnet hop, never a `CF-Connecting-IP` value, because the header is read on one hostname only → AC-15
- [ ] repeat the console sign in with `-H 'CF-Connecting-IP: 1.2.3.4' -H 'CF-Connecting-IP: 5.6.7.8'` → the row carries the real address rather than either, because more than one value is treated as absent → AC-15b
- [ ] wait for a scheduled backup run, or press **Back up now** → its audit row has `client_address` null on the scheduled one → AC-17
- [ ] `sqlite3 /data/deployer.db "SELECT COUNT(*) FROM audit_log WHERE client_address IS NOT NULL AND occurred_at < datetime('now', '-90 days')"` → 0, and the total row count is unchanged, so the sweep nulls without deleting → AC-18, AC-18a

### The rate limiter sees one visitor, not the tunnel

- [x] from one machine, fail sign in through the console repeatedly until it answers 429 → AC-16
- [x] immediately post to `/v1/auth/login` from the same machine → also refused, because both surfaces spend from one bucket → AC-16
- [x] from a second machine on a different address, sign in through the console → not refused, so one abuser is one bucket rather than everybody → AC-16
- [x] re confirmed against the live deployment on 2026-08-16, after the flip, using synthetic `CF-Connecting-IP` values so no real address entered a bucket or a lockout. Posting `/forgot` with `CF-Connecting-IP: 203.0.113.10` on the console host gave ten `200`s and then `429`, matching `bucketCapacity = 10` exactly; `203.0.113.20` on the same host was still `200`, so buckets are per visitor rather than shared; `203.0.113.10` again was `429`, so the key really is the header value; and the same header on the tailnet host was `200`, so it is read on the console hostname and nowhere else → AC-15, AC-16

  Worth stating because it is the question the `X-Forwarded-For` finding under AC-25 raises: the limiter is unaffected by nginx rewriting that header, because the console never passes through nginx. The tunnel sends console traffic straight to the `deployer` Service, so the platform reads `CF-Connecting-IP` as Cloudflare set it. Nothing on the cluster uses nginx `limit-rps`, `limit-connections` or a source allowlist annotation, so no app is currently bucketing on the rewritten value either

### Cookies and origins

- [ ] sign in on the console, read the `Set-Cookie` → `__Host-deployer_session`, with `Secure`, `Path=/`, and no `Domain` → AC-19
- [ ] everyone who was signed in before this deploy is signed out exactly once → AC-20
- [x] post a form with `Origin: https://console.deploy.toyintest.org` → accepted; with the tailnet origin → accepted; with a third → 403 → AC-21
- [ ] take the `__Host-deployer_csrf` value minted on the tailnet host and post it to the console → refused, because the cookie is host scoped and each hostname mints its own → AC-21a

### The reserved name

- [ ] `deploy_app` with `name: "console"` over a real MCP session → refused with `app_name_reserved`, and no app row is written → AC-6
- [ ] the same with `name: "Console"` and `name: "console!"` → refused, because the check runs on the derived base → AC-7
- [ ] the same with `name: "console-shop"` → accepted → AC-7
- [ ] an app that already holds a now reserved slug still deploys, rolls back and lists → AC-7

### Network policy, walked rather than parsed

The parse tests cannot tell you a peer is **missing**, because a shorter policy is
still a valid policy. Only this walk can, which is the lesson spec 0019 recorded.

- [x] from a pod in `deployer-edge`, reach `deployer.deployer-system.svc:80` → succeeds → AC-22
- [x] from a pod in `default` or any unrelated namespace, reach the control plane on 8080 → refused, which is what makes `CF-Connecting-IP` trustworthy on the console hostname → AC-15a
- [x] from a pod in `ingress-nginx`, reach the control plane on 8080 → refused: the console is not behind that controller and must not be reachable from it → AC-15a, AC-22
- [ ] from a pod in `deployer-edge`, reach anything other than DNS, `ingress-nginx:443`, `deployer-system:8080` and the public internet on 443/7844 → refused → AC-22a
- [x] from a pod in `deployer-system`, reach `cloudflared.deployer-edge.svc:2000` → succeeds; from anywhere else → refused → AC-22a
- [ ] the tailnet peer, the two build namespaces, and the control plane pod on 5000 all still work, so adding a peer took none away → AC-22

### The certificate (still owed, in k3sprox-gitops)

- [x] move the `wildcard-apps` `Certificate` from the staging `ClusterIssuer` to the production one → AC-8
- [x] `kubectl -n ingress-nginx get certificate wildcard-apps` → `Ready`, covering both `*.deploy.toyintest.org` and the bare domain → AC-8
- [x] every path, tailnet and LAN included, serves a certificate a browser accepts with no warning → AC-8
- [ ] expect the shared `ingress-nginx` controller to restart, which briefly interrupts TLS for the twelve other apps on the cluster. Do this deliberately, not as a side effect of another change.

### Failure

- [ ] `kubectl -n deployer-edge scale deploy/cloudflared --replicas=0` → within a few minutes exactly one mail arrives naming which thing broke and nothing else → AC-23
- [ ] leave it down → no further mail, because the flag dedupes the notification → AC-23a
- [ ] scale back to 2 → exactly one recovery mail → AC-23
- [ ] confirm both mails arrived while the tunnel was down, which proves the telling does not depend on the thing it reports on → AC-24
- [ ] restart the control plane pod while the edge is down → at most one extra mail, which is the whole cost of the in memory flag → AC-23a

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

- AC-1 … config tests, and the mail link check after the flip · AC-2, AC-2a … the route split by Host override and through the real path · AC-3, AC-4 … the tailnet and other host steps · AC-5 … suite · AC-6, AC-7 … the reserved name steps over a real session · AC-8 … the certificate section, still owed · AC-9, AC-9a … the app and console hostnames through the tunnel · AC-10 … the connector steps · AC-11, AC-12 … parse tests plus the unrouted hostname · AC-13, AC-14 … after the flip · AC-15, AC-15b … the address derivation steps · AC-15a … the network policy walk · AC-16 … the limiter steps · AC-17, AC-18, AC-18a … the audit and sweep reads · AC-19, AC-20, AC-21, AC-21a … cookies and origins · AC-22, AC-22a … the policy walk · AC-23, AC-23a, AC-24 … the failure section · AC-25 … the app header step · AC-26 … the administration step after the flip
