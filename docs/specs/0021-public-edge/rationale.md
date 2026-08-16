# 0021. Public edge: reasoning and options

The build spec is [index.md](index.md). This file is the decision record. `/develop` does not read it.

## Context

> ⚠️ Premise note: the scope row frames this feature as removing the wall between a new person and the platform, but the decision taken here removes only half of it. The console becomes public and the MCP endpoint and upload endpoint do not, so a person can sign in from anywhere and an agent still cannot deploy without Tailscale. That is a deliberate choice made in the design conversation, and it is the right one for a first flip, because the deploy path is the surface that runs code on your cluster. It should be said plainly rather than discovered later: joining goes from four steps to four steps, and what changes is that three of them stop needing a VPN. The remaining one is enrolled as the first follow up.

The platform is complete and reachable by almost nobody. Everything it serves sits behind one of two private paths: apps answer on a wildcard hostname whose only DNS record is `172.16.70.40`, an address nothing on the internet can route to, reached over a `/32` host route the pfSense subnet router advertises onto the tailnet; and the control plane answers on a `ts.net` name through the `tailscale` ingress class. Spec 0003 built both, deliberately, and the scope's settled decisions say in as many words that slices 12 and 13 reopen the LAN or VPN half on purpose.

Slice 12 is finished. The per account app cap, bounded egress, account suspension, invite only registration, open internet login CSRF, the control plane fence, and the platform's own encrypted backup all exist and are all provable today over the tailnet. The ordering was chosen so the irreversible step would land on controls that already work, and this is that step.

Four forces shape it.

The first is that the operator is publishing from a home. A public IP in a DNS record or on a certificate is a home address, and the done when for this feature forbids it. That rules out anything whose shape is a port forward, however it is dressed up.

The second is that the control plane is not a homogeneous surface. Some of it is a person's console and some of it changes the cluster: the MCP endpoint deploys, the upload endpoint accepts a 100 MB tarball, and the admin pages suspend accounts and read backups. Today one hostname carries all of it because one hostname was private. Publishing without splitting it publishes the deploy path.

The third is that a proxy in the path breaks two things that were previously free. `clientAddress` in `internal/web` and `internal/httpapi` takes the last hop of `X-Forwarded-For`, which is correct behind one trusted proxy and wrong behind two. Behind a tunnel every public visitor would share one rate limit bucket, so a single abuser locks out everyone. And `audit_log` has no address column at all, so the trail cannot say where anything came from even if the derivation were right.

The fourth is that the console is about to become a sibling of subdomains running arbitrary code the platform did not write. Spec 0019 already saw this coming for the pre authentication CSRF cookie and gave it the `__Host-` prefix, with a comment saying the prefix is what stops an app on a sibling subdomain writing that cookie. The session cookie did not get the same treatment, because nothing was public yet.

The cost of not deciding is that slice 12 is a set of controls guarding a platform nobody outside the tailnet can reach, which is a lot of work sitting idle.

## Options considered

### Option 1: A locally managed Cloudflare Tunnel with two origins, split in the router

`cloudflared` runs in the cluster and dials out to Cloudflare, so nothing listens on a public address at the house. Cloudflare terminates TLS at the edge and the tunnel carries the two hostnames that should be public. The app wildcard goes to `ingress-nginx`, which routes it exactly as it routes tailnet traffic today. The console hostname goes straight to the control plane Service, which is what lets network policy prove the request came through the tunnel. What must stay private is simply not registered on the console host, so the mux answers `404` for it.

**Pros**:
- No port opened, no public IP anywhere. The zone and an API token already exist because cert-manager uses Cloudflare for the DNS-01 challenge, so the vendor is not new to this project.
- The wildcard hostname shape and the domain do not change, which is what the scope settled.
- The tunnel's routing is a reviewed manifest, so what is public is a file and a test rather than a dashboard setting.
- `cloudflared` is boring. It is a single static binary in a Deployment with a token, and its failure mode is that connectors go away, which is visible.

**Cons**:
- Cloudflare is in the path of everything public, and their outage is yours.
- The free plan caps a request body at 100 MB, which is exactly the platform's upload ceiling. That is survivable here only because the upload endpoint is not being published.
- Two origins means two paths into the cluster, and the console's is plain HTTP inside it. A symptom on one path says nothing about the other.
- One more namespace, one more credential, one more thing to patch.

### Option 2: Tailscale Funnel

Reuse the operator already running on the cluster to serve the console and apps publicly. No new vendor, no new credential, and one fewer moving part than a tunnel.

**Pros**:
- Nothing new to install or hold. The operator is already there and already trusted with the console's ingress.
- The tailnet and public paths would be the same mechanism, so there is one thing to reason about.

**Cons**:
- Funnel serves `ts.net` names only. Neither the console nor the app wildcard could keep `deploy.toyintest.org`, which the scope settles as unchanging, and every app URL would become a long machine generated name.
- Funnel is not designed for a wildcard of arbitrary app hostnames, so the app half does not work at all.

### Option 3: A VPS reverse proxy over the tailnet

A small cloud instance holds the public IP and proxies back into the cluster over the tailnet. Full control of the edge, no vendor limits, and the real client address is whatever you configure it to be.

**Pros**:
- No body size cap, so the upload and MCP endpoints could be published later without a new decision.
- No third party sees plaintext traffic, and no dashboard holds any part of the configuration.
- The edge behaviour is yours to change without a deploy of the cluster.

**Cons**:
- A machine to run, patch, monitor and pay for, and it is the one component with no vendor status page. This is a platform for under ten people operated by one person.
- The public IP is now a rented one rather than a home one, which satisfies the constraint, but the box is a genuine attack surface that has to be hardened and kept hardened.
- Everything Cloudflare gives for free, volumetric absorption and bot handling, would have to be built or done without.

### Option 4: Cloudflare proxied DNS pointed at the home address

Turn the orange cloud on and forward a port. No tunnel daemon, no namespace, no credential.

**Pros**:
- The least infrastructure of any option, and the fastest to stand up.
- Cloudflare still fronts the traffic, so the edge features come along.

**Cons**:
- It needs a port forward on the home router, and the origin address remains discoverable through DNS history, certificate logs, and direct probing of the address range. The done when for this feature forbids exactly this.
- Once the address is known, the proxy is bypassable and every control in slice 12 is reachable directly.

## Rationale

Option 4 is refused by the constraint rather than by a tradeoff: the requirement is that the home address appears nowhere, and a port forward means it appears somewhere, however well hidden it starts out. Option 2 is refused by the settled scope: Funnel cannot serve `deploy.toyintest.org`, and cannot serve a wildcard of app hostnames at all, so it fails the half of the feature that matters most. Option 3 is genuinely the technically freest answer, and it is the one to come back to the day the upload endpoint needs publishing, because the 100 MB cap is the one real limit Option 1 imposes. It loses today on operational reality: a single operator running a homelab does not need a second machine whose only job is to be the thing that is always up.

Option 1 wins on the force from Context that the Cloudflare relationship already exists. cert-manager already holds a DNS edit token for this zone, the records are already there, and the wildcard certificate is already issued through a Cloudflare DNS-01 challenge. Adding a tunnel is adding one component to a vendor already load bearing in the design, not adding a vendor.

The split is where the real design work is, and it comes from the second force. Publishing one hostname would publish the deploy path, so the decision is that the console hostname is a reduced surface rather than the whole server. Putting that rule in Go rather than only in the tunnel configuration follows directly from a rule the project already holds, that a refusal a caller sees is a real access decision: a route the tunnel simply does not mention is guarded by a manifest nothing in CI reads, and a typo there is silent. Expressing it in the mux rather than in a middleware then removes the last way it can go wrong quietly. A middleware needs a list of which routes are private, and a list beside a routing table is a second source of truth that drifts the first time somebody adds a route. Host qualified mux patterns make the routing table say it once, and the failure mode inverts: a route nobody registers on the console host is private, which is the direction to fail in.

Reading `CF-Connecting-IP` only on the console hostname follows the third force, but the first draft of this spec overclaimed what that buys, and the cross check caught it. A host gate stops a caller on a different hostname. It does nothing about a caller inside the cluster sending the console hostname, and behind a shared `ingress-nginx` that is most of the cluster, because a controller serving twelve other apps cannot tell tunnel relayed traffic from anything else arriving at it. The gate would have been a comment rather than a control.

That is why the console does not go through `ingress-nginx`. Pointing the tunnel straight at the `deployer` Service puts the console behind the same fence spec 0019 built, so the set of pods that can present the console hostname is the set the policy names: the tunnel namespace, the tailscale proxy, and the two build namespaces. Of those, a build pod is the only one a user influences, and it is short lived and holds no session, so the most a forgery buys is a wrong address on an audit row for an action it cannot perform. The cost is that the console hop is plain HTTP inside the cluster rather than verified TLS, which is a genuine reduction and is written into the tradeoffs. Enforcement in a layer that actually enforces beats encryption on a hop that was not the exposure.

The `__Host-` prefix on the session cookie is the fourth force answered the same way spec 0019 answered it for the CSRF cookie, including the plain HTTP fallback and the comment explaining what that fallback gives up. Doing it differently here would leave one prefixed cookie and one unprefixed one on the same server for no reason a reader could reconstruct.

Nulling the audit address in place rather than deleting the row is the one place where the two goods genuinely conflict. The trail of who did what is worth keeping for years; the address is not. Nulling keeps the first and drops the second, at the cost of making an aged row indistinguishable from a platform written one. That is a real loss and it is written into the spec rather than glossed, because someone reading a three year old denial should know why the address is missing.

Finally, the ordering. The project builds Tracer Bullet, thin end to end threads first, and this feature deliberately does not. The end to end thread here is the DNS change, and it is the one step that cannot be reverted by reverting a commit. The scope already argued this out and the design conversation confirmed it: build and prove everything against the tailnet, then flip once.

## The policy peer, and how it was nearly got wrong twice

The scope row for feature 22 carries a warning from spec 0019: *this feature has to add its tunnel's namespace as an ingress peer on the new `deployer-system` policy, or the console goes dark on the flip.* That warning is right, and the design here lands exactly on it: the tunnel namespace on container port `8080`.

It is worth recording that it did not arrive there in a straight line. The first draft of this spec sent the console through `ingress-nginx` alongside the apps, concluded that the row was wrong because the connection would then originate in the `ingress-nginx` namespace, and wrote a section explaining the correction. The reasoning was sound for the design it described. It was the design that was wrong: routing the console through a shared controller is what would have made the `CF-Connecting-IP` gate unenforceable, and choosing the direct origin instead put the peer back where the row said it was.

So the row was correct all along, for a reason it did not state. Both mistakes are the same one spec 0019 documented three times: a policy peer read off a picture of how traffic is imagined to flow rather than off where the connection actually originates, and a control assumed to be enforced because it is stated. On the record a fourth time.

The `tailscale` peer stays either way, because the admin split makes the tailnet path the only way to reach the admin pages rather than a legacy door being replaced. The row assumed a swap; the design is an addition.

`ingress-nginx` is not a peer of `deployer-system` at all, before or after this feature. Spec 0019 already said so in as many words, that the control plane is not behind `ingress-nginx`, and it is still true.
