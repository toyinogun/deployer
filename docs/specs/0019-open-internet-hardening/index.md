# 0019. Open internet hardening: login CSRF and control plane policy

**Date**: 2026-08-15
**Status**: In Progress

## Summary

Two protections were left out earlier because the tailnet stood in front of everything, and both specs say so out loud: the login CSRF half in [spec 0013](../0013-web-interface/index.md), under its security model, and the control plane fence in [spec 0008](../0008-workload-isolation-network-policy/index.md), as the first of its follow ups. This closes them. The forms you use before signing in (sign in, register, forgot, reset, resend) get a token tied to a cookie, so another site cannot make your browser post them. And the `deployer-system` namespace stops accepting connections from anything on the cluster except the handful of callers that genuinely need it: the tailnet proxy, the two build namespaces, the control plane pod itself, and the cluster's own nodes. Neither half changes what the platform does; both change what a stranger can reach.

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

- **AC-11**: `deployer-system` carries three policy objects delivered as static YAML in `deploy/` and applied by ArgoCD, across two files so each parse test reads one kind. In `deployer-system-networkpolicy.yaml`, a `networking.k8s.io/v1` pair: a default deny on ingress only, and an allow policy listing every pod sourced caller. In `deployer-system-cilium-networkpolicy.yaml`, one `cilium.io/v2` CiliumNetworkPolicy carrying the node sourced callers. Every object names `Ingress` alone, so egress from the namespace is untouched. The Cilium object is namespaced, never a `CiliumClusterwideNetworkPolicy`: the `deployer` AppProject leaves `namespaceResourceWhitelist` unset so any namespaced kind applies, but its `clusterResourceWhitelist` names five kinds and `cilium.io` is not among them, so a cluster scoped object would sit `OutOfSync` and stop everything else in `deploy/` applying with it.
- **AC-12**: The v1 allow policy permits ingress from exactly three kinds of source, and nothing else, each with its own port list rather than a bare peer, because a peer with no `ports` clause means every port in the namespace:
  - the `tailscale` namespace, on TCP 8080 only. Console traffic never has business reaching the registry.
  - the two build namespaces `deployer-builds` and `deployer-builds-dockerfile`, on TCP 5000 and TCP 8080.
  - the control plane pod itself, matched by `podSelector` on `app: deployer` with no `namespaceSelector` beside it so it means this namespace only, on TCP 5000. This peer is not an optimisation and leaving it out breaks every deploy: `resolveImage` in `internal/reconcile` asks the registry over HTTP what the build pushed, and that call leaves the control plane pod and arrives at the registry pod as ordinary pod to pod traffic carrying a `10.42.x` source. A namespace fence that lists only outside callers denies the namespace's own loudest conversation.

  It carries no node addresses at all. Every port named is the pod's own container port, never the Service port. The control plane Service listens on 80 and forwards to 8080, and a policy written against 80 permits nothing, which is the same trap the build policy comments already flag.
- **AC-12a**: Node sourced traffic is admitted by the CiliumNetworkPolicy, by identity rather than by address. Its `endpointSelector` is empty, so it covers every pod in the namespace, and it carries exactly two ingress rules: `host` on TCP 8080 and TCP 5000, and `remote-node` on TCP 5000 alone. `host` covers the kubelet's probes against both pods, which always arrive from the pod's own node. `remote-node` covers containerd pulling an app image from the registry, which arrives from whichever node the new pod landed on. Nothing needs `remote-node` on 8080, so it is not granted there.

  A CIDR peer cannot express this. Cilium settles node sourced traffic onto the reserved `host` and `remote-node` identities before any CIDR rule is evaluated, and the cluster leaves `policy-cidr-match-mode` at its default, so an `ipBlock` listing the node addresses matches nothing. The `host` rule is written out rather than left to Cilium's default allowance for local host traffic, because that default is real but unwritten and disappears if host firewall is ever turned on.

  This rule adds to the v1 pair rather than replacing what it says. Both objects select the same pods and both are ingress deny by default, and Cilium takes the union of every allow across every policy object selecting an endpoint, so a connection is admitted when any one of them permits it. That composition is the load bearing assumption of the split, and unlike the two settings above it is a property of Cilium rather than something read off this cluster, so it is the one thing here taken on knowledge. The cross node pull in the build plan is what settles it either way: if the union does not hold, that pull fails exactly as it did before, which is a loud answer rather than a silent one.
- **AC-13**: A pod in an `app-<slug>` namespace cannot open a connection to the platform API on TCP 8080 or to the registry on TCP 5000, proved against the real cluster rather than the fake clientset. The node rules do not weaken this: a pod carries a pod identity and never the `host` or `remote-node` one, so admitting nodes admits no pod.
- **AC-14**: With every policy applied, every in cluster caller of this namespace still works, checked one by one rather than as a single happy path, because each is a separate peer and a happy path exercises whichever one happens to come first. The list is closed and it is this: the console over the tailnet, the kubelet's probes against both pods, a build completing through each of the two build namespaces, the control plane resolving a pushed image from the registry, and containerd pulling an app image on a node that is not the registry's own. That last qualifier is the criterion, not a detail: a pull on the registry's node is `host` traffic and passes on a rule that has nothing to do with nodes, so a check that lets the scheduler choose proves nothing about `remote-node` and reports success either way.
- **AC-15**: Go tests in `internal/config` parse both policy files and pin their whole shape, the way `builds-networkpolicy.yaml` is already pinned. For the v1 pair: ingress only, the exact three peers including the in namespace one, the exact ports, no node addresses anywhere, and no egress rule. For the Cilium object: the empty `endpointSelector`, the exact two entity rules and their ports, and no `egress` key. It decodes into a minimal local struct declaring only the fields the assertions touch, read with the same `sigs.k8s.io/yaml` call the existing tests use, so no Cilium module enters `go.mod` for one test file. Widening any of it fails the suite. The tests pin shape and cannot tell you a peer is missing, since a shorter list is a valid policy: only AC-14's live walk catches an absent peer, which is why that criterion names its callers rather than saying "nothing broke".
- **AC-16**: _Withdrawn._ It required a comment in the YAML warning that adding a node to the cluster means adding an entry to the node address list. That list no longer exists: AC-12a admits nodes by identity, which covers every node the cluster ever has, so there is nothing left to keep in step and nothing to warn about.
- **AC-17**: No new `DEPLOYER_*` setting, no migration, and no change to `rbac.yaml` or `admission-policy.yaml`.
- **AC-18**: Before the policy is applied, the `tailscale` namespace is confirmed live to be where the Ingress proxy pod actually runs. A ProxyClass can place it elsewhere, and the namespace selector is written from that assumption, so it is checked rather than believed.
- **AC-19**: _Resolved, and the answer was no._ The criterion asked whether Cilium presents the four node addresses as the source it matches, and required that the live proof decide it rather than an assumption. The live proof on 2026-08-15 decided it: a pull on a node other than the registry's timed out with the address list applied and succeeded the moment it was removed. The fallback the criterion named is therefore taken, and it is AC-12a. Nothing here is left open; the criterion stays on the record because it is the reason the build did not improvise, and because the shape of the question is worth keeping in front of whoever writes the next policy.

## Decision

**Chosen option**: Option 1: Fix both in place, a signed double submit cookie for the forms and an ingress only policy pair for the namespace.

The pre authentication forms get a `__Host-` prefixed nonce cookie whose HMAC travels in the existing `csrf` field, and `deployer-system` gets a static ingress fence: a `networking.k8s.io/v1` pair carrying the pod sourced callers, plus one namespaced CiliumNetworkPolicy carrying the node sourced ones by identity.

The node half was originally four `/32` ipBlock entries. The live proof required by AC-19 found that Cilium admits none of them, so the fallback that criterion named is taken and the addresses are gone. Reasoning in [rationale.md](rationale.md).

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
| The policy's node peers | the reserved entities `host` and `remote-node` | Cilium's own identity model, so nothing is read from the cluster and nothing needs keeping in step. The v1 policy carries no node addresses at all |
| The Cilium object's `endpointSelector` | empty, meaning every pod in the namespace | the rule covers probes against both pods and pulls against the registry, and an empty selector is the honest way to say so rather than two objects differing only by label |
| The Cilium object's kind | namespaced `CiliumNetworkPolicy`, never `CiliumClusterwideNetworkPolicy` | the `deployer` AppProject's `clusterResourceWhitelist`, read live: five kinds, none of them `cilium.io` |
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
- Nothing at run time reads the policy files, so the Go tests in `internal/config` are the only thing that notices a hand edit. They have to pin the peer list, not just check the files parse.
- A node is not an address here, it is an identity. Cilium resolves node sourced traffic onto `host` or `remote-node` before any CIDR rule runs, so an `ipBlock` naming a node address permits nothing while looking exactly like it should. This is the invariant that cost the first live run: the rule reads correctly, the parse test passes it, and the only instrument that finds it is a pull on a node that is not the one the destination pod runs on.
- Local and remote node traffic are two different identities, and only one of them was ever broken. `host` traffic is permitted by Cilium's default when host firewall is off, which is why every `app-<slug>` namespace has run a strict ingress deny with no node peer at all and kept its readiness probes. The rule names `host` anyway, so the file states the whole truth about who reaches in rather than resting half of it on a default.
- The Cilium object must stay namespaced. The `deployer` AppProject whitelists five cluster scoped kinds and `cilium.io` is not among them, and ArgoCD validates the whole operation rather than the one object, so reaching for a `CiliumClusterwideNetworkPolicy` does not fail that object alone: it stops everything in `deploy/` from applying while the app still reads as healthy.
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
- Policy shape: the parse tests refuse a widened peer list, an added egress rule, a changed port, a changed entity, or an `ipBlock` reappearing in the v1 file. Verifies **AC-15**.
- Live refusal: the probe app cannot reach `deployer:8080` or the registry on 5000. Verifies **AC-13**.
- Live cross node pull, proved both ways: a pod pinned to a node that is not the registry's, pulling a digest that node has never seen, pulls with the Cilium object applied and times out without it. A pull on the registry's own node is the wrong test and passes either way. Verifies **AC-12a**.
- Live proof nothing broke: each caller in AC-14's list in turn, including the control plane resolving a pushed image, which only a deploy carried through to `healthy` exercises. Verifies **AC-14**.

## Build plan

The project builds by Tracer Bullet, and this feature has two threads that never touch. Each is taken end to end on its own, the riskier one first: the namespace policy can take the whole platform down and the CSRF change cannot, so the policy goes first while the tailnet is still the outer fence and a mistake costs nothing but your own afternoon.

1. Confirm live where the Tailscale Ingress proxy pod runs, before a line of YAML is written, since the namespace selector rests on the answer. Satisfies **AC-18**.
2. The v1 pair as static YAML in `deploy/deployer-system-networkpolicy.yaml`, ingress only, with the three pod sourced peer groups each carrying its own port list against container ports, and its kustomization entry. Before writing the peer list, walk the code for everything that opens a connection into this namespace rather than listing the callers from outside in: `internal/registry` is reached from `internal/reconcile` on 5000, which is the peer that reads as internal and is not. Satisfies **AC-11**, **AC-12**.
3. The CiliumNetworkPolicy `node-ingress-allow` in `deploy/deployer-system-cilium-networkpolicy.yaml`, namespaced, and its own kustomization entry. Its comment carries the reason a CIDR peer is not used here and the reason the object is not cluster scoped, since both are facts about the cluster that the file cannot show on its own. Nothing in the repository has this shape yet, so there is no neighbour to copy from and the whole body is written out here. Note the port: `cilium.io/v2` types it as a string, where `networking.k8s.io/v1` types it as an integer, so the obvious first try of copying the v1 file's `port: 8080` fails to decode.

   ```yaml
   apiVersion: cilium.io/v2
   kind: CiliumNetworkPolicy
   metadata:
     name: node-ingress-allow
     namespace: deployer-system
   spec:
     endpointSelector: {}
     ingress:
       # Kubelet probes against both pods, always from the pod's own node.
       - fromEntities:
           - host
         toPorts:
           - ports:
               - port: "8080"
                 protocol: TCP
               - port: "5000"
                 protocol: TCP
       # Containerd pulling an app image, from whichever node it landed on.
       - fromEntities:
           - remote-node
         toPorts:
           - ports:
               - port: "5000"
                 protocol: TCP
   ```

   Satisfies **AC-11**, **AC-12a**.
4. The parse tests in `internal/config` pinning both files beside the existing build policy tests. The v1 test's own name and doc comment count the peers (`TestTheControlPlaneIngressIsTheTailnetTheNodesTheBuildsAndItself`, and a comment saying four peer groups) and both go stale when the node group leaves, so they change with the assertion: pinning the new shape under a name that still says nodes passes the suite and misleads the next reader, which nothing else catches. Rename it `TestTheControlPlaneIngressIsTheTailnetTheBuildsAndItself` and delete the `controlPlaneNodeCIDRs` fixture with it. Add an assertion that no `ipBlock` appears anywhere in the v1 file, so a well meaning revert to addresses fails rather than silently doing nothing.

   The Cilium assertions go in a sibling file, `internal/config/nodepolicy_test.go`, decoding into a minimal local struct rather than importing the Cilium module for one test. Type its port as a string, matching the CRD: reusing the v1 test's `int32` port assertion is the natural move and it silently decodes to zero. Satisfies **AC-15**.
5. Live on the cluster, in this order. The probe app refused on 5000 and 8080. Then the cross node pull proved both ways, before anything else, because it is the check the last attempt got wrong: a throwaway Pod with `nodeName` pinned to a node that is not the registry's, pulling an app digest that node has never seen, expected to pull with the Cilium object applied and to time out with it deleted. Then the rest of AC-14's caller list one by one, ending with a full deploy carried to `healthy` rather than a build that reaches `Complete`. A build Job going green proves the push and nothing after it. Satisfies **AC-13**, **AC-14**, **AC-12a**, **AC-19**.
6. The nonce cookie and the token derive in `internal/web/csrf.go`: generate hex, read back, HMAC, the constant time compare, and the name and flags chosen off `s.secure`, with both cookie shapes pinned. Satisfies **AC-2**, **AC-2a**, **AC-3**, **AC-9**.
7. The form aware refusal function beside the existing `refuseCSRF`: it sets a fresh cookie, records the audit row under the right new reason, and re renders the named page with the email refilled. Satisfies **AC-5**, **AC-6**.
8. The thin thread on `/login` alone: the GET sets the cookie, the form carries the field, the POST is guarded, the refusal comes back as the form, and a successful sign in clears the cookie. Satisfies **AC-1**, **AC-4**, **AC-7**.
9. The other four guarded posts adopting the same helper, with `/resend` taking its cookie from `/unverified`'s GET, plus the two tab case and the confirmation that the JSON endpoints are untouched. Satisfies **AC-1**, **AC-8**, **AC-10**.
10. Update every existing caller that posts to these page paths without carrying a cookie jar: the page tests, and any bootstrap or smoke script that does the same. They pass today and will 403 after this. Satisfies **AC-4**.
11. The closing pass, two context files rather than one. `internal/web/AGENTS.md` records that two CSRF mechanisms now exist, which applies where, and why the cookie name changes off `s.secure`. `deploy/AGENTS.md` gains the control plane fence in its Layout list, which does not mention it at all today, plus a Conventions line saying the namespace is fenced by two objects of two different kinds, that the Cilium one is the first thing in `deploy/` that is not portable to another CNI, and that it must stay namespaced because of the AppProject's cluster scoped whitelist. That file already carries the pinning convention for the build policies, so it is where a reader will look and currently the one place that would not tell them. The leak crawl is extended to the nonce in the same pass. Satisfies **AC-9**, **AC-17**.

## Consequences

**Positive**:

- The two protections the earlier specs named as knowingly skipped are now real, so the tailnet can come down without either being an open item.
- The forms surface gains a defence that does not depend on a header a future browser might stop sending, which is the whole of what guards them today.
- A stray or hostile workload elsewhere on the cluster can no longer even open a connection to the platform API, so a stolen token has to be used from somewhere that is allowed as well.
- The node peer needs no maintenance ever. Expressed as an identity, it covers a node added tomorrow with no edit, which is strictly better than the address list it replaces: that list would have broken image pulls on a new node only, the sort of partial failure that is hard to attribute.
- The fence now says out loud that node sourced traffic exists and what it is allowed to do, including the local node path that every other namespace on this cluster leaves resting on a Cilium default.

**Negative / tradeoffs**:

- A second CSRF mechanism exists alongside the first, and the two work differently. Whoever reads this code next has to learn which one applies where, which is a real cost and the reason the closing pass writes it down.
- People will hit the refusal. Clearing cookies mid reset, or coming back to a bookmarked form, now fails where it used to work. The re rendered sentence is what keeps that from being a dead end, and it is extra code that exists solely for that case.
- `deploy/` is no longer portable. Every policy here was `networking.k8s.io/v1` until now, and this namespace's fence is the first thing in the repository that only works on Cilium. Moving the platform to another CNI means rewriting this object rather than reapplying it, and the file is the only place that says so.
- The fence is split across two files and two policy models, so a reader has to hold both to know who reaches this namespace. The split buys a parse test that reads one kind per file and keeps the CRD dependency to one small object, but the cost is real and the comments in both files carry the pointer to the other.
- The rule admits `remote-node` from every node, not just the ones running app pods, and `host` from every process in any node's host namespace. That is wider than a pod peer and it is unavoidable: node sourced traffic carries no finer identity to select on. The registry's own htpasswd auth is what stands behind it, and that was always the case.
- The policy is one more thing that can take the platform down through a mistake, and unlike the app policies it has no run time composer to test. Two attempts proved it in two different ways. The first listed the three outside callers, passed the parse test, passed the console check, passed the probe refusals, and still broke every deploy, because the peer it omitted was the namespace itself. The second added that peer, passed everything again including ten minutes of both pods holding `1/1`, and broke image pulls on three nodes out of four, because the node peer it wrote as addresses matched nothing and the checks that looked like they proved otherwise were all local node traffic. The class of bug is a peer that reads correctly and admits nothing, and the only instrument that finds it is a real pull on the far side of the cluster.
- The `__Host-` prefix is refused by browsers over plain HTTP, so local development runs on a different cookie name from production. That is a real inconsistency: the sibling subdomain guarantee is exactly the thing local development cannot exercise, so the protection is one a test on a laptop can never prove.
- Every existing test or script that posts to a page path now has to carry a cookie jar. That is mechanical work with no user facing value, and skipping a caller shows up as a 403 rather than as a compile error.

**Neutral**:

- Slice 13 puts a tunnel in front of the console, which changes where its inbound traffic comes from. The tailnet peer in this policy will need a sibling or a replacement then, and that is named in Follow-up rather than guessed at now.
- No migration, no new setting, no schema change.

## Follow-up

- [ ] Egress from `deployer-system` is deliberately untouched. Locking it would mean enumerating the Kubernetes API server on node addresses, cluster DNS, the registry, and Resend, and the API server peer changes when a node is added, so a mistake there is a full outage rather than a warning. Worth deciding on its own once the node list stops moving.
- [ ] The public edge in slice 13 moves the console off the `tailscale` ingress class and behind a tunnel. That feature has to add its tunnel's namespace as an ingress peer here, or the console goes dark on the flip. Named in the feature 22 row as well as here.
- [ ] The in namespace peer is written against `app: deployer` because those are the only two pod labels in `deployer-system` today, read live. A third pod added later, a maintenance Job or a debug pod that needs the registry, fails exactly the way the first policy did: parse test green, build green, and the failure one step later wearing a reason code that blames the build. This is now the only drift hazard left in the fence, since the node half stopped having one when it stopped naming addresses. Worth deciding whether a comment on the peer is enough, or whether anything added to this namespace should be required to carry the `app: deployer` label or its own peer.
- [x] ~~Nothing enforces that the four node addresses in the policy match the cluster.~~ Closed by AC-12a rather than done: there are no node addresses left to drift. The entity covers every node the cluster ever has.
- [ ] Every `app-<slug>` namespace runs a strict ingress deny with no node rule at all, and its readiness probes work only because Cilium permits local host traffic by default. That is the same unwritten dependency this spec just decided not to accept for `deployer-system`, left standing everywhere else. It costs nothing today and it breaks every app's probes at once on the day host firewall is turned on. Worth deciding whether the app policy composer in `internal/deploy` should compose the same `host` rule, which would make it the first Cilium object the control plane generates at run time rather than a static file, and that is a larger step than adding a rule.
- [ ] The build namespaces reach `deployer-system` as pods and are unaffected, but their own policies carry the same CIDR shaped assumption in their `except` lists for a different purpose. Nothing is known to be broken there. Worth one read against what Cilium actually matches, since the failure mode here was a rule that looked right and admitted nothing.
- [ ] `build_no_digest` is the reason code a person sees when the control plane cannot reach the registry at all, and it reads as "your build produced nothing". It is accurate about the symptom and misleading about the cause, and it cost a verify run an hour. Worth deciding whether a connection failure to the registry deserves its own reason code, separate from a build that genuinely pushed no manifest. That is a change to the closed set in `internal/domain/reason.go`, so it belongs to its own decision rather than this one.
- [ ] The JSON identity endpoints are left unguarded on purpose, on the reasoning that they are not cookie authenticated for these actions. Worth revisiting if a browser surface is ever built on top of them.

## Migration plan

**Strategy**: no migration needed, but two deployments rather than one.

**Phases**:
1. The namespace fence, all three objects applied by ArgoCD together. They change no Go code, so they can be reverted by a single revert and a sync. Applying the v1 pair without the Cilium object is the state the second attempt was in, and it breaks image pulls on every node but the registry's, so the two files land in one sync rather than one at a time.
2. The CSRF change, shipped in the control plane image. It is invisible to every existing session, since signed in forms are untouched.

**Rollback**: phase 1 reverts by removing both files from the kustomization and syncing. A pull already backing off does not recover on its own inside its backoff window, so a revert is followed by deleting the stuck pods to force an immediate retry rather than waiting. Phase 2 reverts by rolling the deployment back to the previous digest, which the release history already keeps. Neither leaves state behind: no schema change, and a cookie left in a browser is ignored by code that does not read it.

**Risks**: the fence is applied to a namespace that is currently serving, so a missed peer is an outage rather than a test failure, and both failures so far were peers that were missed rather than peers that were wrong. Build plan step 5 exists for exactly that, and its order matters: the cross node pull runs before the rest of the walk, because it is the one check that has now been got wrong twice and everything after it is cheaper to redo.
