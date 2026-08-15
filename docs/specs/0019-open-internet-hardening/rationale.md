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

## Rationale

Option 1 wins on the specific forces above rather than on general preference.

The carrier choice is settled by the wildcard. On a domain whose subdomains you control, a plain double submit cookie and a signed one are close to equivalent, and the simpler one wins. Here the subdomains are handed to strangers by design, so a sibling can write a cookie onto the parent domain and shadow a plain nonce. The `__Host-` prefix refuses that write, and the HMAC means a nonce that somehow arrived anyway still does not verify. Two independent reasons the attack fails, for the cost of reusing an HMAC helper that already exists.

The policy reach is settled by what a mistake costs. The gap spec 0008 named is inbound: a workload reaching the platform API. An ingress only policy closes exactly that and cannot break anything outbound, because it names no egress rules at all. Adding egress would close a different and much rarer threat, one that begins with the control plane already compromised, at the price of a peer list containing node addresses that move. That trade is the wrong way round, so egress is a follow up with the reason written down rather than a silent omission.

The one place the carrier decision is compromised is plain HTTP. A browser refuses a `Secure` cookie there and refuses a `__Host-` cookie without one, so holding the prefix unconditionally would mean nobody can sign in on a laptop. The choice is between a protection that cannot apply on plain HTTP anyway, since there are no sibling apps on a laptop and no confidentiality either, and a platform a contributor cannot run. The prefix comes off with `s.secure`, which is already how the session cookie behaves, and the cluster is served over HTTPS so production always carries it. The honest cost is that the guarantee is one a local test can never exercise.

The peer list is settled by reading the cluster rather than by repeating spec 0008's follow up. That follow up says ingress from `ingress-nginx` and `deployer-builds`, and both halves are now wrong: the control plane sits on the `tailscale` ingress class so nginx never touches it, and there are two build namespaces since the Dockerfile path landed. The node addresses were not in the follow up at all, and they are the entry whose omission would have been the worst of the three, because containerd pulls images from the in cluster registry as the node, not as a pod, and losing that breaks image pulls everywhere at once rather than breaking the thing you were changing.

The ordering inside the build plan follows the same reasoning. The policy is the half that can take the platform down and it goes first, while the tailnet is still in front and an outage costs an afternoon rather than a person's trust.

## Cluster facts this rests on

Read out of the repository and the scope on 2026-08-15, not assumed:

- The control plane's Ingress uses `ingressClassName: tailscale` (`deploy/ingress.yaml`), so inbound console traffic arrives from the Tailscale operator's proxy pod, not from `ingress-nginx`. Apps use the `nginx` class on the shared wildcard, which is a different path entirely.
- The registry runs inside `deployer-system` on a pinned `clusterIP` of `10.43.166.224` (`deploy/registry.yaml`), and every node's `/etc/rancher/k3s/registries.yaml` mirrors the registry's `.svc` name to that address, because containerd pulls through the host resolver which has no cluster DNS. Every image pull therefore arrives from a node address, not from a pod.
- Kubelet probes on both pods in the namespace also arrive from a node address.
- The four nodes are `172.16.70.20` to `.23` (`docs/scope/scope.md`, verified live 2026-08-11). MetalLB holds `172.16.70.41` and `.42` in the same subnet, which is why the whole `/24` is a poor peer.
- Both build namespaces already carry egress to `deployer-system` on TCP 5000 and TCP 8080 (`deploy/builds-networkpolicy.yaml`, `deploy/builds-dockerfile-networkpolicy.yaml`), so the ingress rule here is the mirror of a bound that already exists on the other side.
- The signed in CSRF token is the hex HMAC SHA256 of `sessions.id` under `DEPLOYER_CSRF_KEY`, derived at render and never stored (`internal/web/csrf.go`). The key is validated at startup and delivered by `deploy/web-sealedsecret.yaml`, the one Secret the pod refuses to boot without.
- The session cookie is already `HttpOnly`, `SameSite=Lax`, and `Secure` when `s.secure` is set (`internal/web/session.go`), so the new cookie's attributes match a pattern that is already in the code.
- `internal/config` already parses the two build policy files and pins their whole shape in Go tests, which is the convention the new policy test follows.
- `web.go` registers `POST /resend` but no `GET /resend`. The resend form only ever renders inside `GET /unverified`, so the pages that set the cookie and the posts that are guarded are two different sets of five.
- `refuseCSRF` always renders the standalone `message` template and has no path back to a form, so the friendly refusal AC-6 asks for is new plumbing rather than a reuse.
- `s.secure` is derived from `PublicURL`'s scheme, so a local `go run ./cmd/deployer` over plain HTTP has it false. That is what makes the cookie name conditional rather than fixed.
- The control plane Service listens on 80 and forwards to container port 8080, so every port in the new policy is a container port. A policy written against the Service port permits nothing, which is the trap the build policy comments already call out.
- The binary exposes `/healthz` and `/readyz` and no metrics endpoint, so no scrape peer is needed. ArgoCD and the sealed secrets controller both act through the Kubernetes API server rather than connecting into the namespace, so neither needs one either. Checked rather than assumed, and recorded here so the next reader does not have to check again.
