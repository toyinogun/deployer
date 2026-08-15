# 0019. Open internet hardening: login CSRF and control plane policy

**Date**: 2026-08-15
**Status**: In Progress

## Summary

Two protections were left out earlier because the tailnet stood in front of everything, and both specs say so out loud. This closes them. The forms you use before signing in (sign in, register, forgot, reset, resend) get a token tied to a cookie, so another site cannot make your browser post them. And the `deployer-system` namespace stops accepting connections from anything on the cluster except the four things that genuinely need it. Neither half changes what the platform does; both change what a stranger can reach.

## Requirements

**User stories**:
- As the platform operator, I want a page on another site to be unable to submit the sign in or reset form in a visitor's browser, so that opening the console to the internet does not hand an attacker a way to act as a visitor.
- As the platform operator, I want a workload elsewhere on the cluster to be unable to open a connection to the platform API or the registry, so that the token check is not the only thing between a stray pod and the control plane.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

Login CSRF

- **AC-1**: A GET of any of the five pages carrying a pre authentication form (`/login`, `/register`, `/forgot`, `/reset`, and `/unverified`, which is where the resend form lives) sets a nonce cookie holding a fresh random value when the request carries no valid one, and reuses the existing nonce when it does. No other page sets this cookie. The five guarded posts are a different set from the five pages: `/resend` has no GET route of its own, and its post is guarded by the cookie `/unverified` set.
- **AC-2**: The cookie is named `__Host-deployer_csrf` and is `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, carries no `Domain` attribute, and no `Max-Age` or `Expires`, so it lives as long as the browser session. The `__Host-` prefix is part of the name, not decoration: it is what stops an app on a sibling subdomain writing this cookie.
- **AC-2a**: When `s.secure` is false, meaning the platform is being served over plain HTTP, the cookie is named `deployer_csrf` without the prefix and without the `Secure` flag. A browser refuses a `Secure` cookie over HTTP, so keeping the prefix there would make signing in locally impossible. This mirrors how the session cookie already gates its own `Secure` flag, and it carries a comment saying that a plain HTTP deployment loses the sibling subdomain guarantee. The cluster is served over HTTPS, so the prefix is always present in production.
- **AC-3**: Each of those forms renders a hidden `csrf` field whose value is the hex HMAC SHA256 of the cookie's nonce under `DEPLOYER_CSRF_KEY`, the same key and the same field name the signed in forms already use. The nonce itself is 32 bytes from `crypto/rand` hex encoded, so both halves are hex and neither needs cookie value escaping.
- **AC-4**: A POST to any of the five guarded paths is refused unless the request carries the cookie and a hidden field that matches the HMAC of that cookie's nonce, compared in constant time. The existing `Origin` and `Sec-Fetch-Site` check still runs first and still refuses independently.
- **AC-5**: A refused post changes nothing, answers `403`, and writes an audit row under the existing page CSRF action with one of two new reasons, `csrf_pretoken_missing` when no usable cookie arrived and `csrf_pretoken_mismatch` when one did but the field did not match. Both stay visually distinct from the signed in path's `csrf_invalid`, so the audit log says which mechanism fired.
- **AC-6**: The refusal is not a bare error page. The form is re rendered with a plain sentence saying it expired and to try again, a fresh cookie is set, and the fields a person can safely keep (the email address) are still filled in. The password fields are not. This needs its own refusal function rather than the existing `refuseCSRF`, which always renders the standalone message page and has no way back to a form.
- **AC-7**: A successful sign in clears the pre authentication cookie in the same response that sets the session cookie, so exactly one CSRF mechanism is live at a time.
- **AC-8**: The JSON identity endpoints in `internal/httpapi` are unchanged. The guard is on the page paths only, and a curl caller against the JSON surface is unaffected.
- **AC-9**: The nonce never appears in a page body, a log line, or an audit row. Only its HMAC does, which keeps the existing leak boundary intact.
- **AC-10**: Two tabs open on the same form both submit successfully, because the nonce is reused rather than rotated per render.

Control plane policy

- **AC-11**: `deployer-system` carries a NetworkPolicy pair delivered as static YAML in `deploy/` and applied by ArgoCD: a default deny on ingress only, and an allow policy listing every permitted source. `policyTypes` names `Ingress` alone, so egress from the namespace is untouched.
- **AC-12**: The allow policy permits ingress from exactly four kinds of source, and nothing else, each with its own port list rather than a bare peer, because a peer with no `ports` clause means every port in the namespace:
  - the `tailscale` namespace, on TCP 8080 only. Console traffic never has business reaching the registry.
  - the four node addresses `172.16.70.20`, `.21`, `.22` and `.23` as individual `/32` ipBlock entries, on TCP 8080 and TCP 5000. Both are needed: 8080 for kubelet probes on the control plane pod, 5000 for probes on the registry pod and for every containerd image pull.
  - the two build namespaces `deployer-builds` and `deployer-builds-dockerfile`, on TCP 5000 and TCP 8080.
  - the control plane pod itself, matched by `podSelector` on `app: deployer` with no `namespaceSelector` beside it so it means this namespace only, on TCP 5000. This peer is not an optimisation and leaving it out breaks every deploy: `resolveImage` in `internal/reconcile` asks the registry over HTTP what the build pushed, and that call leaves the control plane pod and arrives at the registry pod as ordinary pod to pod traffic carrying a `10.42.x` source. A namespace fence that lists only outside callers denies the namespace's own loudest conversation.

  Every port named is the pod's own container port, never the Service port. The control plane Service listens on 80 and forwards to 8080, and a policy written against 80 permits nothing, which is the same trap the build policy comments already flag.
- **AC-13**: A pod in an `app-<slug>` namespace cannot open a connection to the platform API on TCP 8080 or to the registry on TCP 5000, proved against the real cluster rather than the fake clientset.
- **AC-14**: With the policy applied, every in cluster caller of this namespace still works, checked one by one rather than as a single happy path, because each is a separate peer and a happy path exercises whichever one happens to come first. The list is closed and it is this: the console over the tailnet, the kubelet's probes against both pods, a build completing through each of the two build namespaces, the control plane resolving a pushed image from the registry, and containerd pulling an app image on a node. A deploy that reaches `healthy` covers the last three at once, which is why the whole deploy is the check rather than the build alone.
- **AC-15**: A Go test in `internal/config` parses both policy files and pins their whole shape, the way `builds-networkpolicy.yaml` is already pinned: ingress only, the exact peer list including the in namespace peer, the exact ports, and no egress rule. Widening any of it fails the suite. The test pins shape and cannot tell you a peer is missing, since a shorter list is a valid policy: only AC-14's live walk catches an absent peer, which is why that criterion names its callers rather than saying "nothing broke".
- **AC-16**: The node addresses carry a comment in the YAML saying that adding a node to the cluster means adding an entry here, because nothing at run time reads this file and nothing will warn.
- **AC-17**: No new `DEPLOYER_*` setting, no migration, and no change to `rbac.yaml` or `admission-policy.yaml`.
- **AC-18**: Before the policy is applied, the `tailscale` namespace is confirmed live to be where the Ingress proxy pod actually runs. A ProxyClass can place it elsewhere, and the namespace selector is written from that assumption, so it is checked rather than believed.
- **AC-19**: Whether Cilium presents the four node addresses as the source it matches for kubelet probes and containerd pulls is confirmed by the live proof, not assumed. If it does not, the fallback is a CiliumNetworkPolicy using the `remote-node` entity, which a portable `networking.k8s.io/v1` policy cannot express, and that is a bigger change than this spec covers: it comes back through `/architect` rather than being improvised in the build.

## Decision

**Chosen option**: Option 1: Fix both in place, a signed double submit cookie for the forms and an ingress only policy pair for the namespace.

The pre authentication forms get a `__Host-` prefixed nonce cookie whose HMAC travels in the existing `csrf` field, and `deployer-system` gets a static NetworkPolicy pair that denies ingress and allows the tailnet proxy, the four nodes, and the two build namespaces.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`)

## Rationale

Reasoning, the options weighed, and the cluster facts this rests on: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**: none. Neither half touches the database. The nonce lives in a cookie and is never stored, exactly as the session derived token is never stored.

**State transitions**: none.

**API surface**: no new paths. The five existing page posts gain a guard.

| Path | Method | New input | Auth | New error |
|---|---|---|---|---|
| `/login` | POST | `csrf` hidden field | none | 403, form re rendered |
| `/register` | POST | `csrf` hidden field | none | 403, form re rendered |
| `/forgot` | POST | `csrf` hidden field | none | 403, form re rendered |
| `/reset` | POST | `csrf` hidden field | none | 403, form re rendered |
| `/resend` | POST | `csrf` hidden field | none | 403, form re rendered |

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| Rendering a pre authentication form | the nonce | 32 bytes from `crypto/rand`, hex encoded, generated at render when the cookie is absent or malformed, otherwise read back from the cookie |
| Rendering a pre authentication form | the hidden `csrf` value | HMAC SHA256 of the nonce under `DEPLOYER_CSRF_KEY`, the key already validated in `internal/config` and delivered by `deploy/web-sealedsecret.yaml` |
| Rendering a pre authentication form | the `Secure` flag on the cookie | `s.secure` on the web server, the same value the session cookie already uses |
| Rendering a pre authentication form | the cookie name | derived from `s.secure`: `__Host-deployer_csrf` when true, `deployer_csrf` when false, per AC-2a |
| Refusing a post | the audit reason string | `csrf_pretoken_missing` or `csrf_pretoken_mismatch`, two new constants beside the existing `csrf_invalid`, `origin_cross_site` and `origin_mismatch` in `internal/web/csrf.go` |
| Refusing a post | the email refilled into the form | the submitted form value, carried through the existing `formPage` shape |
| The policy's node peers | `172.16.70.20/32` to `.23/32` | the four k3s node addresses recorded in `docs/scope/scope.md`, written literally into the YAML |
| The policy's tailnet peer | the `tailscale` namespace | the namespace the Tailscale operator runs its ingress proxy in, matched by `kubernetes.io/metadata.name` |
| The policy's build peers | `deployer-builds`, `deployer-builds-dockerfile` on TCP 5000 and 8080 | the mirror of the egress rules already in `deploy/builds-networkpolicy.yaml` and `deploy/builds-dockerfile-networkpolicy.yaml` |
| The policy's in namespace peer | `podSelector` on `app: deployer`, TCP 5000 | the label the control plane Deployment already puts on its pods, read live rather than assumed (`app=deployer` and `app=deployer-registry` are the only two in the namespace). A bare `podSelector: {}` would work as well, since only the registry listens on 5000, but naming the caller keeps the rule readable as a statement about who talks to whom |

**Key invariants**:

- One CSRF mechanism is live per request. Before sign in the token comes from the cookie nonce, after sign in from the session id, and the sign in response deletes the first as it sets the second. A handler never reads both.
- The nonce is the secret and the HMAC is the proof. The nonce stays in the cookie, the HMAC goes in the body, and neither the page nor a log nor an audit row ever carries the nonce.
- The cookie name is load bearing. `__Host-deployer_csrf` cannot be written by an app on a sibling subdomain of the same registrable domain, and apps deployed by strangers hold exactly such subdomains. Adding a `Domain` attribute reopens the hole this closes. The one place the prefix is dropped is plain HTTP, where a browser would refuse the cookie outright and the alternative is a platform nobody can sign into locally: that carve out is tied to `s.secure` and never reachable on the cluster.
- The refusal path is separate from the signed in one. `refuseCSRF` renders a standalone message page and cannot come back as a form, so AC-6 needs its own function, closer in shape to the existing `formFailure`. Reusing the old helper is the path of least resistance and it silently fails AC-6.
- The origin check is not replaced. It runs first and refuses on its own, so a request that clears one check still has to clear the other.
- The namespace policy is ingress only. Adding an `Egress` entry to `policyTypes` denies the control plane's outbound path to the Kubernetes API server, which sits on node addresses that are not in any policy, and takes the platform down.
- Nothing at run time reads the policy files, so the Go test in `internal/config` is the only thing that notices a hand edit. It has to pin the peer list, not just check the files parse.
- The namespace is a peer of itself. The control plane talks to the registry over the pod network, not over a loopback or a socket, so `deployer-system` appears on both sides of its own fence. Every peer list written for this namespace has to be read as "who may open a connection to a pod here", and the answer includes a pod that already lives here. The parse test cannot catch this and neither can a build: the build pushes successfully and the failure lands one step later, wearing the reason code `build_no_digest`, which blames the build for something it did correctly.

**Security model**:

- Before sign in there is no account, so the guard protects the visitor's browser rather than an account's data. What it stops: a page on another site causing your browser to post credentials, a registration, or a password reset to the console under your cookies. Login CSRF matters here because a forged sign in can plant an attacker's session in your browser, and a forged reset post can consume a real reset token.
- The threat that makes the `__Host-` prefix necessary is specific to this platform: apps deployed by other people sit on sibling hostnames under one wildcard, and a plain cookie set by a sibling can shadow a parent domain cookie. The prefix refuses that write.
- After the namespace policy, reaching the platform API or the registry from the cluster requires being the tailnet proxy, a node, or a build pod. The token check remains the authorisation decision; this is the layer in front of it.
- No compliance scope. No new personal data, no new storage.

**Configuration required**: none. `DEPLOYER_CSRF_KEY` already exists and is reused unchanged.

**Critical test scenarios**:

- Happy path: a fresh browser opens `/login`, receives the cookie, submits, and signs in. Verifies **AC-1**, **AC-3**, **AC-4**, **AC-7**.
- Failure case: a post with the cookie deleted, and a post with the field altered, each answer 403, change nothing, write an audit row with the right reason, and come back as the form with the email intact. Verifies **AC-4**, **AC-5**, **AC-6**.
- Cookie shape: the `Set-Cookie` header is asserted attribute by attribute, including the absence of `Domain` and `Max-Age`, in both the secure and the plain HTTP case so the name swap is pinned rather than incidental. Verifies **AC-2**, **AC-2a**.
- Local development: a server built with `s.secure` false serves the sign in form, sets the unprefixed cookie, and completes a sign in. Verifies **AC-2a**.
- Leak boundary: the existing crawl is extended so no rendered page or audit row contains the nonce. Verifies **AC-9**.
- Two tabs: two GETs of the same form followed by two POSTs both succeed. Verifies **AC-10**.
- Policy shape: the parse test refuses a widened peer list, an added egress rule, or a changed port. Verifies **AC-15**.
- Live refusal: the probe app cannot reach `deployer:8080` or the registry on 5000. Verifies **AC-13**.
- Live proof nothing broke: each caller in AC-14's list in turn, including the control plane resolving a pushed image, which only a deploy carried through to `healthy` exercises. Verifies **AC-14**.

## Build plan

The project builds by Tracer Bullet, and this feature has two threads that never touch. Each is taken end to end on its own, the riskier one first: the namespace policy can take the whole platform down and the CSRF change cannot, so the policy goes first while the tailnet is still the outer fence and a mistake costs nothing but your own afternoon.

1. Confirm live where the Tailscale Ingress proxy pod runs, before a line of YAML is written, since the namespace selector rests on the answer. Satisfies **AC-18**.
2. The policy pair as static YAML in `deploy/deployer-system-networkpolicy.yaml`, ingress only, with the four peer groups each carrying its own port list against container ports, the node comment, and its kustomization entry. Before writing the peer list, walk the code for everything that opens a connection into this namespace rather than listing the callers from outside in: `internal/registry` is reached from `internal/reconcile` on 5000, which is the peer that reads as internal and is not. Satisfies **AC-11**, **AC-12**, **AC-16**.
3. The parse test in `internal/config` pinning the whole shape beside the existing build policy tests: ingress only, the exact peers, the exact ports, no egress rule. The test's own name and doc comment count the peers (`TestTheControlPlaneIngressIsTheTailnetTheNodesAndTheBuilds`, "exactly three ways in") and both go stale with the fourth peer, so they change with the assertion. Pinning the new shape under a name that still says three passes the suite and misleads the next reader, which nothing else catches. Satisfies **AC-15**.
4. Live on the cluster: the probe app refused on 5000 and 8080, then AC-14's caller list walked one by one, ending with a full deploy that reaches `healthy` rather than a build that reaches `Complete`. A build Job going green proves the push and nothing after it. If the node ipBlock turns out not to match what Cilium sees, stop and route back through `/architect` rather than reaching for a CiliumNetworkPolicy here. Satisfies **AC-13**, **AC-14**, **AC-19**.
5. The nonce cookie and the token derive in `internal/web/csrf.go`: generate hex, read back, HMAC, the constant time compare, and the name and flags chosen off `s.secure`, with both cookie shapes pinned. Satisfies **AC-2**, **AC-2a**, **AC-3**, **AC-9**.
6. The form aware refusal function beside the existing `refuseCSRF`: it sets a fresh cookie, records the audit row under the right new reason, and re renders the named page with the email refilled. Satisfies **AC-5**, **AC-6**.
7. The thin thread on `/login` alone: the GET sets the cookie, the form carries the field, the POST is guarded, the refusal comes back as the form, and a successful sign in clears the cookie. Satisfies **AC-1**, **AC-4**, **AC-7**.
8. The other four guarded posts adopting the same helper, with `/resend` taking its cookie from `/unverified`'s GET, plus the two tab case and the confirmation that the JSON endpoints are untouched. Satisfies **AC-1**, **AC-8**, **AC-10**.
9. Update every existing caller that posts to these page paths without carrying a cookie jar: the page tests, and any bootstrap or smoke script that does the same. They pass today and will 403 after this. Satisfies **AC-4**.
10. The closing pass: the leak crawl extended to the nonce, and the `internal/web/AGENTS.md` note recording that two CSRF mechanisms now exist, which applies where, and why the cookie name changes off `s.secure`. Satisfies **AC-9**, **AC-17**.

## Consequences

**Positive**:

- The two protections the earlier specs named as knowingly skipped are now real, so the tailnet can come down without either being an open item.
- The forms surface gains a defence that does not depend on a header a future browser might stop sending, which is the whole of what guards them today.
- A stray or hostile workload elsewhere on the cluster can no longer even open a connection to the platform API, so a stolen token has to be used from somewhere that is allowed as well.
- The policy file pins the node addresses in one place a human can read, which is one more piece of the cluster's shape written down rather than known.

**Negative / tradeoffs**:

- A second CSRF mechanism exists alongside the first, and the two work differently. Whoever reads this code next has to learn which one applies where, which is a real cost and the reason the closing pass writes it down.
- People will hit the refusal. Clearing cookies mid reset, or coming back to a bookmarked form, now fails where it used to work. The re rendered sentence is what keeps that from being a dead end, and it is extra code that exists solely for that case.
- The node addresses are hard coded in YAML with nothing enforcing them. Adding a node and forgetting this file breaks image pulls on that node only, which is the sort of partial failure that is hard to attribute.
- The policy is one more thing that can take the platform down through a mistake, and unlike the app policies it has no run time composer to test. The first attempt proved this: it listed the three outside callers, passed the parse test, passed the console check, passed the probe refusals, and still broke every deploy, because the peer it omitted was the namespace itself. The class of bug is a peer nobody thought of, and the only instrument that finds it is a real deploy carried to `healthy`.
- The `__Host-` prefix is refused by browsers over plain HTTP, so local development runs on a different cookie name from production. That is a real inconsistency: the sibling subdomain guarantee is exactly the thing local development cannot exercise, so the protection is one a test on a laptop can never prove.
- Every existing test or script that posts to a page path now has to carry a cookie jar. That is mechanical work with no user facing value, and skipping a caller shows up as a 403 rather than as a compile error.

**Neutral**:

- Slice 13 puts a tunnel in front of the console, which changes where its inbound traffic comes from. The tailnet peer in this policy will need a sibling or a replacement then, and that is named in Follow-up rather than guessed at now.
- No migration, no new setting, no schema change.

## Follow-up

- [ ] Egress from `deployer-system` is deliberately untouched. Locking it would mean enumerating the Kubernetes API server on node addresses, cluster DNS, the registry, and Resend, and the API server peer changes when a node is added, so a mistake there is a full outage rather than a warning. Worth deciding on its own once the node list stops moving.
- [ ] The public edge in slice 13 moves the console off the `tailscale` ingress class and behind a tunnel. That feature has to add its tunnel's namespace as an ingress peer here, or the console goes dark on the flip. Named in the feature 22 row as well as here.
- [ ] The in namespace peer is written against `app: deployer` because those are the only two pod labels in `deployer-system` today, read live. A third pod added later, a maintenance Job or a debug pod that needs the registry, fails exactly the way the first policy did: parse test green, build green, and the failure one step later wearing a reason code that blames the build. The node list already carries a drift guard in AC-16 and the follow up below; the namespace's pod population carries none. Worth deciding whether a comment on the peer is enough, or whether anything added to this namespace should be required to carry the `app: deployer` label or its own peer.
- [ ] Nothing enforces that the four node addresses in the policy match the cluster. A check comparing them against the live node list, in CI or in the startup sweep, would catch a node added and a file forgotten. Cheap only if there is somewhere sensible to run it.
- [ ] `build_no_digest` is the reason code a person sees when the control plane cannot reach the registry at all, and it reads as "your build produced nothing". It is accurate about the symptom and misleading about the cause, and it cost a verify run an hour. Worth deciding whether a connection failure to the registry deserves its own reason code, separate from a build that genuinely pushed no manifest. That is a change to the closed set in `internal/domain/reason.go`, so it belongs to its own decision rather than this one.
- [ ] The JSON identity endpoints are left unguarded on purpose, on the reasoning that they are not cookie authenticated for these actions. Worth revisiting if a browser surface is ever built on top of them.

## Migration plan

**Strategy**: no migration needed, but two deployments rather than one.

**Phases**:
1. The namespace policy, applied by ArgoCD on its own. It changes no Go code, so it can be reverted by a single revert and a sync.
2. The CSRF change, shipped in the control plane image. It is invisible to every existing session, since signed in forms are untouched.

**Rollback**: phase 1 reverts by removing the file from the kustomization and syncing. Phase 2 reverts by rolling the deployment back to the previous digest, which the release history already keeps. Neither leaves state behind: no schema change, and a cookie left in a browser is ignored by code that does not read it.

**Risks**: the policy is applied to a namespace that is currently serving, so a missed peer is an outage rather than a test failure. The three live proofs in build plan step 3 exist for exactly that, and they are the reason the policy ships before the CSRF work rather than beside it.
