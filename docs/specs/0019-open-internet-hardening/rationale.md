# 0019. Rationale: open internet hardening

Reasoning behind [index.md](index.md).

## Context

Two protections were weighed and refused earlier, and both refusals are written down in the spec that made them. Spec 0013 says the pre authentication posts carry no synchroniser token, because there is no session to bind one to and a pre session cookie was more moving parts than the risk earned on a tailnet only internal tool. Spec 0008 says the `deployer-system` namespace is deliberately left unpoliced, so any workload on the cluster can still reach the platform API, on the reasoning that tokens guard it. Both are correct as written and both were correct because reaching either surface meant being on the tailnet already.

Slice 12 exists to rebuild the controls the tailnet was providing, and slice 13 removes it. Once the console is reachable from the open internet, the first refusal means an attacker's page can make a visitor's browser post to the sign in and reset forms, and the second means the platform API is one connection away from anything running on the cluster, including the code strangers deploy.

There is a force here that is specific to this platform rather than general. Apps deployed by other people are served on sibling hostnames under one wildcard domain, and the console will sit under the same registrable domain. A cookie set by a sibling subdomain can shadow a cookie on the parent, so a bare double submit cookie is weaker here than it would be on a domain whose subdomains are all yours. That is the fact the carrier decision turns on.

The consequence of not deciding is that slice 13's flip is the moment both gaps stop being theoretical, and slice 13 is one manifest change that is meant to be uneventful.

## Options considered

### Option 1: Fix both in place, a signed double submit cookie and an ingress only policy

Add a `__Host-` prefixed nonce cookie to the five pre authentication forms, with the HMAC of the nonce travelling in the `csrf` field the signed in forms already use, and add a static NetworkPolicy pair to `deployer-system` that denies ingress and allows the tailnet proxy, the four nodes, and the two build namespaces.

_As proposed, that is. The node half of this option did not survive contact with the cluster: see the second set of options below, where the four node addresses are replaced by Cilium entities. The rest of the option stands as written._

**Pros**:
- Reuses everything: the same key, the same field name, the same audit action, the same refusal path, and the same static policy shape as the build namespaces.
- The `__Host-` prefix answers the sibling subdomain problem directly rather than hoping it does not come up.
- Ingress only means the policy cannot break an outbound path nobody enumerated.
- Both halves are revertible on their own, and neither touches the database.

**Cons**:
- Two CSRF mechanisms now live in one package and a reader has to learn which applies where.
- Real people will hit the new refusal, so it needs a friendly path that only exists for them.
- The node addresses end up hard coded in YAML with nothing checking them.

### Option 2: Rely on the origin headers alone, and police nothing

Keep spec 0013's position: `Origin` and `Sec-Fetch-Site` already refuse a cross site post, and tokens already guard the API.

**Pros**:
- No new code, no new failure mode, no new cookie.
- The header check is genuinely strong against the browsers people actually use.

**Cons**:
- Both headers are optional and are trusted only when present, so the guard is exactly as strong as the client's honesty. That is a fine bet inside a tailnet and a poor one on the open internet.
- It leaves the platform API reachable from every pod on a cluster whose whole purpose is running code an AI wrote for a stranger, which is defence in depth abandoned rather than deferred.
- It leaves two specs carrying refusals whose stated reason has expired, which is worse than either gap: the reasoning stops matching reality.

### Option 3: Deny both directions on `deployer-system`

Police egress as well, enumerating the Kubernetes API server, cluster DNS, the registry, and Resend.

**Pros**:
- Genuine containment of a compromised control plane, which is the pod holding the database, the registry credential, and every app's configuration.
- Says out loud what the control plane is allowed to talk to, which is knowledge currently held nowhere.

**Cons**:
- The API server peer is a list of node addresses that changes whenever a node is added, and getting it wrong stops the platform dead rather than degrading it.
- Egress to Resend is a public address that can change without notice, so the policy would need a hostname rule and Cilium's `toFQDNs`, which is a different policy kind and a bigger step.
- The blast radius is the whole platform, for a threat that starts with the control plane already compromised.

### Option 4: A rotating per render token

Issue a fresh nonce on every form render.

**Pros**:
- Narrowest window for a stolen token.
- No long lived value in the browser at all.

**Cons**:
- Breaks two tabs open on the same form, which happens during a password reset more than anywhere else.
- Buys very little: the token only authorises attempting an unauthenticated action, so shortening its life is not where the risk is.

### Options considered for the node peer, after the live proof

AC-19 asked whether Cilium matches the node addresses and required the live proof to answer. It answered no, so these four were weighed on 2026-08-15.

#### Option A: a small CiliumNetworkPolicy carrying the node peer by identity

Drop the four `/32` entries from the v1 policy and add one namespaced Cilium object naming `host` and `remote-node`.

**Pros**:
- The only option that expresses what is actually true: node traffic has an identity here, not an address.
- Kills the drift hazard outright rather than guarding it. A node added later needs no edit, so AC-16 and its follow up both close.
- The CRD dependency is one small object in its own file, and the rest of the fence stays portable.
- Nothing outside this repository changes, so the fix ships through the same ArgoCD sync as everything else.

**Cons**:
- `deploy/` stops being CNI portable for the first time. Another CNI means rewriting this object.
- Two policy models for one namespace, so a reader has to hold both.
- No Kubernetes API type exists for the CRD, so the parse test either declares its own struct or pulls a very large module in for one file.

#### Option B: convert the whole allow policy to one CiliumNetworkPolicy

One file, one model, entities and selectors together.

**Pros**:
- A reader learns one policy language for this namespace instead of two.
- Entity and endpoint peers sit side by side, which reads better than a fence split across kinds.

**Cons**:
- Rewrites three working peers to fix one broken one, on a namespace where a missed peer is an outage and two have been missed already.
- Throws away the portable half for no gain: the pod peers work correctly as v1 today.
- The whole parse test is rewritten at once, so the test and the thing it pins change together, which is exactly when a pinning test is worth least.

#### Option C: set `policy-cidr-match-mode` to `nodes` on Cilium

A cluster wide flag that makes CIDR rules match node identities, after which the existing YAML works unchanged.

**Pros**:
- Not one line of this repository changes.
- Keeps every policy in the portable kind.

**Cons**:
- It is a change to the k3sprox cluster, outside this repository and outside ArgoCD, needing a `cilium-agent` restart. The policy would then depend on a flag nothing here records, which is a worse version of the hazard AC-16 existed for.
- It changes CIDR semantics for every policy on the cluster at once, including the `except` lists the app namespaces rely on, to fix one rule in one namespace.
- It keeps the address list and therefore keeps the drift.

#### Option D: fence the platform pod only and leave the registry open

Narrow the default deny to `app: deployer`, so the registry is selected by nothing and no node rule is needed.

**Pros**:
- Pure v1, no CRD, no node peer, the simplest thing that could work.
- Honest about the fact that the registry has its own htpasswd auth.

**Cons**:
- Gives up half of AC-13, which was proved working. A stray pod could reach the registry again, and the registry is where every app's image lives.
- Solves the problem by deleting the requirement, which is the right move only when the requirement was wrong, and this one is not.

## Rationale

Option 1 wins on the specific forces above rather than on general preference.

**The node peer takes Option A.** The deciding force is that the address list was never expressing a true thing. Cilium settles node sourced traffic onto a reserved identity before a CIDR rule is evaluated, so the four `/32` entries were not a slightly wrong rule that could be corrected, they were a rule that could never match anything. Once that is understood, Option C is the only other one that keeps them, and it keeps them by moving the load bearing part of the decision to a cluster flag in a different repository, which is the same failure the address list already demonstrated: a dependency nothing here records and nothing will warn about.

Option B is the tidier end state and it is refused on blast radius rather than on taste. Three peers work today and were established at real cost. Rewriting them to fix a fourth, on a namespace where the last two mistakes were both outages, spends risk to buy consistency.

Option D is the one worth pausing on, because the registry does authenticate and the simplest fix is often the right one. It is refused because AC-13 was not aspirational: it was proved live, and it is the half of this spec that answers spec 0008's named gap. Trading it away to avoid one small file is the wrong side of that trade.

**The rule names `host` as well as `remote-node`, and covers the whole namespace rather than the registry alone.** Only `remote-node` on 5000 was observed to be broken, so the minimal rule would stop there. It does not, because the reason the local case works is Cilium's default allowance for host traffic, and that default is exactly the kind of unwritten fact this spec has now been burned by twice. Writing it down costs one rule and makes the file the whole answer to "who reaches into this namespace". It also survives host firewall being turned on, which is otherwise a change elsewhere that silently breaks probes here.

**The parse test declares its own struct.** The real Cilium types are correct and free of drift, and they arrive with a very large module and a wide dependency tree in a repository whose only Kubernetes dependency is `client-go`. For one test file asserting six fields, a local struct read with the same `sigs.k8s.io/yaml` call the neighbouring tests already use is the smaller and more honest cost. The runner up is the generic map, refused because its assertions become path walks with type casts that drift silently when a key is renamed.

## What the second attempt got wrong

The first attempt failed on a peer nobody thought of. The second failed on a peer that was thought of, written down carefully, given its own comment explaining why both its ports were needed, and admitted nothing.

The verify run on 2026-08-15 checked the node peer three times and every check passed for a reason that had nothing to do with the peer:

- both `deployer-system` pods held `1/1` with no restarts for ten minutes under the fence, which reads as proof the kubelet's probes still land. Each pod's probe comes from its own node, so all that was proved is the `host` path, which Cilium permits by default and which the address list was not carrying.
- the Paketo deploy reached `healthy` in 44 seconds and its pod pulled its image in 188 milliseconds. It landed on `k3sprox-wkr-pve2-0`, the node the registry pod runs on, so that pull was `host` traffic too.
- the Dockerfile deploy landed on a different node and failed, `dial tcp 10.43.166.224:5000: i/o timeout` into `ImagePullBackOff`, with the build having pushed successfully. Deleting the two policies and deleting the stuck pod made the same digest pull on the same node immediately.

The general lesson is narrower and sharper than the first attempt's, and it is the one worth carrying forward: **a check that lets the scheduler choose where the work runs is not a check of anything that depends on where the work runs.** Three of the four nodes were broken and the two passing observations were both accidents of placement. Nothing in the checklist recorded which node anything landed on, so nothing in it could have caught that.

Two corollaries already folded into the spec. AC-14 now says "a node that is not the registry's own" rather than "a node", and build plan step 5 pins the node by hand and runs the check both ways, so a rule that admits nothing fails the check instead of passing it.

The carrier choice is settled by the wildcard. On a domain whose subdomains you control, a plain double submit cookie and a signed one are close to equivalent, and the simpler one wins. Here the subdomains are handed to strangers by design, so a sibling can write a cookie onto the parent domain and shadow a plain nonce. The `__Host-` prefix refuses that write, and the HMAC means a nonce that somehow arrived anyway still does not verify. Two independent reasons the attack fails, for the cost of reusing an HMAC helper that already exists.

The policy reach is settled by what a mistake costs. The gap spec 0008 named is inbound: a workload reaching the platform API. An ingress only policy closes exactly that and cannot break anything outbound, because it names no egress rules at all. Adding egress would close a different and much rarer threat, one that begins with the control plane already compromised, at the price of a peer list containing node addresses that move. That trade is the wrong way round, so egress is a follow up with the reason written down rather than a silent omission.

The one place the carrier decision is compromised is plain HTTP. A browser refuses a `Secure` cookie there and refuses a `__Host-` cookie without one, so holding the prefix unconditionally would mean nobody can sign in on a laptop. The choice is between a protection that cannot apply on plain HTTP anyway, since there are no sibling apps on a laptop and no confidentiality either, and a platform a contributor cannot run. The prefix comes off with `s.secure`, which is already how the session cookie behaves, and the cluster is served over HTTPS so production always carries it. The honest cost is that the guarantee is one a local test can never exercise.

The peer list is settled by reading the cluster rather than by repeating spec 0008's follow up. That follow up says ingress from `ingress-nginx` and `deployer-builds`, and both halves are now wrong: the control plane sits on the `tailscale` ingress class so nginx never touches it, and there are two build namespaces since the Dockerfile path landed. The node addresses were not in the follow up at all, and they are the entry whose omission would have been the worst of the three, because containerd pulls images from the in cluster registry as the node, not as a pod, and losing that breaks image pulls everywhere at once rather than breaking the thing you were changing.

The ordering inside the build plan follows the same reasoning. The policy is the half that can take the platform down and it goes first, while the tailnet is still in front and an outage costs an afternoon rather than a person's trust.

## Cluster facts this rests on

Read out of the repository and the scope on 2026-08-15, not assumed:

- The control plane's Ingress uses `ingressClassName: tailscale` (`deploy/ingress.yaml`), so inbound console traffic arrives from the Tailscale operator's proxy pod, not from `ingress-nginx`. Apps use the `nginx` class on the shared wildcard, which is a different path entirely.
- The registry runs inside `deployer-system` on a pinned `clusterIP` of `10.43.166.224` (`deploy/registry.yaml`), and every node's `/etc/rancher/k3s/registries.yaml` mirrors the registry's `.svc` name to that address, because containerd pulls through the host resolver which has no cluster DNS. Every image pull therefore arrives from a node address, not from a pod.
- Kubelet probes on both pods in the namespace also arrive from a node address, and always from the pod's own node.
- The four nodes are `172.16.70.20` to `.23` (`docs/scope/scope.md`, verified live 2026-08-11). MetalLB holds `172.16.70.41` and `.42` in the same subnet, which is why the whole `/24` is a poor peer. This is recorded because it is true, not because the policy uses it: after AC-12a no policy in this repository names a node address.

Read live on 2026-08-15, after the second attempt failed:

- The CNI is Cilium `v1.16.5`, one agent per node, with `routing-mode: tunnel`, `kube-proxy-replacement: true` and `enable-bpf-masquerade: true`.
- `policy-cidr-match-mode` is unset in `cilium-config`, which is the default. That is the setting that decides the whole question: with it unset, a CIDR rule does not match node identities, so an `ipBlock` naming a node address permits nothing. Setting it to `nodes` would change that for every policy on the cluster at once.
- `enable-host-firewall` is unset, which is why local host traffic reaches pods regardless of policy today, and why AC-12a writes the `host` rule out rather than relying on it.
- The Cilium CRDs are installed, including `ciliumnetworkpolicies.cilium.io` and `ciliumclusterwidenetworkpolicies.cilium.io`, so the fallback needs nothing added to the cluster.
- **Allows compose as a union across policy kinds.** When more than one policy object selects the same endpoint, Cilium admits a connection that any one of them permits, whether those objects are `networking.k8s.io/v1` NetworkPolicies, CiliumNetworkPolicies, or a mix. This is the fact the two file split rests on: the v1 pair and the Cilium object both select every pod in the namespace, and neither has to know about the other.

  It is worth naming what kind of fact this is, because the rest of this list was read off the cluster and this one was not. It is a property of Cilium rather than a setting on this installation, so there is no `kubectl` that confirms it in the abstract. What confirms it here is the cross node pull in build plan step 5, which cannot succeed unless the union holds. That is why the pull runs first: a wrong answer here fails the same loud way the address list did, rather than hiding.
- The `deployer` AppProject leaves `namespaceResourceWhitelist` unset, so any namespaced kind applies into its three destinations. Its `clusterResourceWhitelist` names exactly five kinds, `Namespace`, `ClusterRole`, `ClusterRoleBinding`, `ValidatingAdmissionPolicy` and `ValidatingAdmissionPolicyBinding`, and no `cilium.io` group. A namespaced `CiliumNetworkPolicy` therefore applies with no change to the separate gitops repository; a `CiliumClusterwideNetworkPolicy` would not, and would take everything else in `deploy/` down with it.
- The registry pod carries `app=deployer-registry` and the control plane pod carries `app=deployer`, still the only two in the namespace.
- The registry pod runs on `k3sprox-wkr-pve2-0`. Nothing pins it there, so which node is the local one moves when that pod is rescheduled. Any check that depends on being on a different node has to read where the registry is at the time rather than hard coding a node name.
- **Every `app-<slug>` namespace already runs a strict ingress deny with no node peer at all**, allowing only `ingress-nginx` on 8080, and every app pod carries a readiness probe on 8080 and has been ready for days. This is the fact that settles what the node peer is actually for: probes were never depending on it. Its only real job is containerd pulling from the registry.
- Both build namespaces already carry egress to `deployer-system` on TCP 5000 and TCP 8080 (`deploy/builds-networkpolicy.yaml`, `deploy/builds-dockerfile-networkpolicy.yaml`), so the ingress rule here is the mirror of a bound that already exists on the other side.
- The signed in CSRF token is the hex HMAC SHA256 of `sessions.id` under `DEPLOYER_CSRF_KEY`, derived at render and never stored (`internal/web/csrf.go`). The key is validated at startup and delivered by `deploy/web-sealedsecret.yaml`, the one Secret the pod refuses to boot without.
- The session cookie is already `HttpOnly`, `SameSite=Lax`, and `Secure` when `s.secure` is set (`internal/web/session.go`), so the new cookie's attributes match a pattern that is already in the code.
- `internal/config` already parses the two build policy files and pins their whole shape in Go tests, which is the convention the new policy test follows.
- `web.go` registers `POST /resend` but no `GET /resend`. The resend form only ever renders inside `GET /unverified`, so the pages that set the cookie and the posts that are guarded are two different sets of five.
- `refuseCSRF` always renders the standalone `message` template and has no path back to a form, so the friendly refusal AC-6 asks for is new plumbing rather than a reuse.
- `s.secure` is derived from `PublicURL`'s scheme, so a local `go run ./cmd/deployer` over plain HTTP has it false. That is what makes the cookie name conditional rather than fixed.
- The control plane Service listens on 80 and forwards to container port 8080, so every port in the new policy is a container port. A policy written against the Service port permits nothing, which is the trap the build policy comments already call out.
- The binary exposes `/healthz` and `/readyz` and no metrics endpoint, so no scrape peer is needed. ArgoCD and the sealed secrets controller both act through the Kubernetes API server rather than connecting into the namespace, so neither needs one either. Checked rather than assumed, and recorded here so the next reader does not have to check again.
- **The control plane is itself a peer.** `resolveImage` in `internal/reconcile` calls `internal/registry` over HTTP (`Digest`, then `ImageUser`, which calls `configDigest`) against `deployer-registry.deployer-system.svc:5000`. That is pod to pod traffic inside the namespace, carrying a `10.42.x` source that matches no outside peer. The pod labels are `app=deployer` and `app=deployer-registry`, read live on 2026-08-15, and they are the only two in the namespace.

## What the first attempt got wrong

The first policy listed the three outside callers and was checked carefully against everything that reaches in from elsewhere: the tailnet proxy, the nodes, the build namespaces, and explicitly not ArgoCD, not a scrape. The list above was written before the policy and it is right about all of that. What it never asked was which pods inside the namespace talk to each other, and the answer turned out to be the busiest path the platform has.

The failure is worth recording because of how well it hid. `/check verify` on 2026-08-15 ran the parse test, the kustomize build, the server dry run, the probe refusals from a stray pod, the console over the tailnet, and two minutes of watching both pods hold `1/1` with no restarts. All of it passed. The deploy then failed, and the trail read like a build problem:

- the build Job reported `Complete` in 33 seconds
- its pod log ended `Saving deployer-registry.deployer-system.svc:5000/apps/probea-gh44sv...` followed by `*** Images (sha256:f849bc50...)`, so the push genuinely landed
- the platform answered `build_no_digest`, "the build reported success but pushed no image"

The push worked and the read back did not. Measured directly afterwards, same pod and same destination with the policy as the only variable: `401 Unauthorized` from the registry with the policy removed, timeout with it applied.

Three general lessons, in rough order of how much they cost:

1. A namespace fence has to be written from the inside out, not the outside in. Listing who may reach in is natural and it silently assumes the namespace's own traffic is exempt, which network policy does not do.
2. A build Job going green proves the push and nothing past it. The check has to be a deploy carried to `healthy`.
3. A parse test that pins shape can never report a missing peer, because a shorter peer list is a perfectly valid policy. Only the live walk finds an omission, which is why AC-14 now names its callers instead of saying nothing broke.
