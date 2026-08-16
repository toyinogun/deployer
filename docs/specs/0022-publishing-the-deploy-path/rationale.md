# 0022. Publishing the deploy path: rationale

Reasoning and options for [index.md](index.md).

## Context

> ⚠️ Premise note: the feature reads as an exposure decision and the scope row already corrects that to a body size decision. Both are true and neither is the hard part. The hard part is that the tailnet was a control, not just a route, and nothing in the platform records which controls it was providing. Publishing without naming them replaces a network perimeter with nothing and calls it routing. This spec therefore treats the exposure and the ceiling as the easy half, and the enumeration of what the tailnet was doing as the work.

Spec 0021 published two surfaces and left three where they were. It split them deliberately: the console is where a person signs in, and the MCP endpoint plus the tarball upload are the surface that runs code on your cluster. That split is why a person no longer needs Tailscale and an agent still does, which leaves joining a developer's job in exactly the place feature 23 is trying to fix.

Three forces shape what follows.

The first is the edge's body cap. Cloudflare's free plan refuses a request body over 100 MB and `MaxUploadBytes` defaults to exactly `100 << 20`. Equal limits mean the platform can accept a body the edge will not carry, and the caller then meets Cloudflare's error page instead of a closed reason code and an audit row. Nothing in the platform sees that failure at all.

The second is that the tailnet was load bearing in a way no file records. `internal/identity/limiter.go` says so in its own comment, "the perimeter is a tailnet", as the justification for the rate limit living in memory and being lost on every restart. That comment was written when everything was private. Spec 0021 retired the premise for the console and nobody revisited the limiter, and `internal/identity/AGENTS.md` already flags it as owed rather than settled. Meanwhile nothing at all bounds the call rate on `POST /v1/uploads` or `/mcp`, nothing caps how much upload volume one account can hold on the PVC, and nothing deletes an unclaimed upload's file, because `uploads.Remove` is only ever called by the reconcile loop once a build has claimed one.

The third is the shape spec 0021 established and this must not break. A route that changes cluster state is absent from a hostname's mux rather than refused by a check inside it, registration on a public hostname is opt in, and `CF-Connecting-IP` is believed on exactly one hostname because that hostname's tunnel origin is the `deployer` Service directly rather than the shared ingress controller. Publishing a second machine facing hostname either extends that shape or quietly undermines it.

## Options considered

### Option 1: A third hostname on the existing tunnel

Add `mcp.deploy.toyintest.org` as a cloudflared rule above the app wildcard, pointed straight at the `deployer` Service, and register the two agent facing routes a second time under that host pattern. Lower the platform's ceiling below the edge's so the platform always refuses first, and replace each named tailnet control in the platform.

**Pros**:

- Reuses the tunnel, the wildcard certificate and the network policy already in place. No new infrastructure component and nothing new to operate (basis: your `AGENTS.md`, the deploy path and the tunnel already run).
- Keeps the opt in registration invariant intact rather than special casing it. The console still carries nothing that changes cluster state (basis: spec 0021, AC-2).
- Keeps `CF-Connecting-IP` trust tied to the property that makes it safe, an origin that bypasses `ingress-nginx`, rather than to a hostname literal.

**Cons**:

- Inherits the edge's 100 MB cap permanently, so the ceiling is a Cloudflare number rather than yours.
- A third name to keep in step across the mux, the tunnel config, DNS and two policies, and the leading `*.` ordering trap now applies to two names.

### Option 2: Publish the two routes on the console hostname

Register `/v1/uploads` and `/mcp` under the existing console host pattern. One name, one record, one rule.

**Pros**:

- Fewest moving parts of any option, and no new configuration at all.
- Nothing about the tunnel, the certificate or the policies changes.

**Cons**:

- Destroys the invariant AC-2 of spec 0021 exists to protect. The console hostname is the one an untrained person types into a browser, and it would then carry the endpoint that runs code on your cluster.
- Makes the opt in registration rule read as advisory. Once one state changing route is registered there, the next one is an argument rather than a rule.

### Option 3: Chunked upload, decoupling the ceiling from any edge

The agent posts the tarball in parts under the cap and the platform reassembles.

**Pros**:

- The ceiling becomes yours permanently. A larger tarball is a config change rather than a plan change, on any edge, on any plan.
- Removes the whole class of "the platform accepted what the edge refused" from the design.

**Cons**:

- A new protocol on the upload endpoint, a new partial state to clean up, and a change to `deploy_app`'s contract, all for a ceiling nothing has hit.
- Every agent client has to implement it, which is the opposite of what feature 23 is for.

### Option 4: Direct to R2 with a presigned URL

The platform hands back a presigned URL, the agent uploads to the bucket, and the build pulls from there. The body never crosses the tunnel or the control plane.

**Pros**:

- The cap becomes R2's rather than the proxy's, and the control plane stops carrying 90 MB bodies at all.
- The client is already in the tree, since spec 0020 runs `minio-go` against R2 for backups (basis: spec 0020).

**Cons**:

- A second credential path and a new trust question: the agent gets a write into your bucket, and the platform no longer sees the bytes it later builds.
- The build path changes from a volume read to a network read, which is a change to the load bearing handoff in spec 0001 for a problem nobody has.

## Rationale

Option 1 wins on the third force in Context. The shape spec 0021 established is the most valuable thing here and it is also the most fragile: it is held by a registration convention rather than by a check, so it survives only while every new route follows it. A third hostname extends that shape by repeating it, and Option 2 is refused because it spends it. The console being the one name a person types is exactly why it must stay the name that cannot change cluster state.

Options 3 and 4 both solve the ceiling properly and both solve it for a problem the platform does not have. A source tarball over 90 MB is an outlier, and neither the chunked protocol nor the presigned bucket pays for itself against one config value and a lower default. Option 4 is the one to come back to if uploads ever become the expensive part, because it removes the body from the control plane entirely, and the client is already there. Today it adds a credential path and moves the build's source read across the network to save nothing.

The ceiling of 90 MB rather than 95 is deliberate: Cloudflare documents the limit but not precisely what it counts, so gzip framing and chunked transfer overhead should not be able to put a body the platform accepted over it. Ten MB of headroom costs nothing real and removes the whole question.

On the controls, the enumeration matters more than any single choice. Reachability is replaced by nothing, and that is the honest answer. An outer gate such as Cloudflare Access with a service token would restore a real perimeter for free, and it was refused on onboarding cost: a second secret in an MCP client's config file is the step feature 23 exists to remove.

That reason on its own is weaker than it looks, and it is worth saying why rather than leaving it. A service token would ride in the same generated block feature 23 already hands out, so it need not add a step a person performs. The stronger objection is the one this repo's own design keeps making: a service token is configured in the Cloudflare dashboard, which nothing here can see, review or test. `deploy/cloudflared-configmap.yaml` exists in this repo precisely so that what is public is reviewable in a file at the cost of a deploy to change it, and a gate whose state lives only in a vendor console is the shape that drifts without anybody noticing. That is what settles it, not the step count. It remains the first thing to reach for if the token alone ever stops feeling like enough.

The other three halves are replaced in the platform, where they are testable, rather than at the edge, where this repo cannot see them.

The lockout goes inside `auth.Authenticator` and nowhere else, because this repo has already paid for the alternative. Until 2026-08-16 the sign in lockout lived in the JSON handler alone while `Service.Login` touched the limiter nowhere, so the browser counted no failures and honoured no penalty, and spec 0021 had just made the browser the only surface on the open internet. Two routes now share a bearer credential path, which is the same shape, so the rule goes in once.

Keeping the limiter in memory is a decision rather than an omission. A restart clears the penalty and ArgoCD restarts the pod on each sync, so an attacker who waits gets a fresh budget. Against a high entropy random token that leaves brute force nowhere near feasible, so the limiter's real job is slowing a script and bounding what one caller can cost you, and it does that either way. What was wrong was the reasoning, not the storage: the comment justifies it with a perimeter that no longer exists. Correcting the comment and recording the bound closes the item `internal/identity/AGENTS.md` flags as owed by deciding it.

Retiring the tailnet path was the engineer's call against the recommendation to keep both. It is a real acceptance and it is recorded in Consequences: after phase 3 a tunnel outage stops every deploy including yours, with no second way in. The cutover order is what makes it survivable, and it follows spec 0021's flip exactly, because that flip is what surfaced the rule ordering defect no earlier proof could reach.

## References

**Project sources** (verifiable, in this repo):

- Spec [0021](../0021-public-edge/index.md): the deliberate split, the opt in registration rule (AC-2), the `CF-Connecting-IP` trust boundary (AC-15a), the tunnel rule ordering defect, and the 100 MB note that raised this feature.
- Spec [0020](../0020-platform-backup-restore/index.md): the `minio-go` R2 client already in the tree, weighed as Option 4.
- Spec [0016](../0016-per-account-app-cap/index.md): the transactional count and insert shape the unclaimed upload cap follows.
- `internal/identity/AGENTS.md`: the in memory limiter flagged as owed rather than settled, and the two different things `limiter.go` calls an address.
- `internal/httpapi/AGENTS.md`: the two audiences in one package, and why `uploadAddress` passes an empty console host today.
- `internal/domain/reserved.go`: `api`, `mcp`, `deployer` and `deploy` already reserved, so the chosen name needs no change there.
- Root `AGENTS.md`: the closed reason code rule, the internal versus public URL rule, the tool description as contract, and the startup validation rule for every `DEPLOYER_*` value.

**Practices & standards**:

- Strangler pattern for a live cutover: build beside, prove, then retire.
- Fail closed at the boundary you control: the platform's ceiling stays strictly under the edge's so the platform is always the thing that refuses.
- One place per rule that judges a credential, so two surfaces cannot diverge.

**Links** (web verified):

- Cloudflare default cache behavior, the per plan maximum upload size (Free and Pro 100 MB, Business 200 MB, Enterprise 500 MB and above): https://developers.cloudflare.com/cache/concepts/default-cache-behavior/
- Cloudflare error 524, the 125 second origin response timeout on the free plan: https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/
- Cloudflare Tunnel connection docs, checked for whether Tunnel changes either limit and stating neither: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/

Two things the official pages do not state and which are therefore recorded as unverified rather than asserted: whether the 125 second timeout is reset by bytes flowing on a streaming response, and whether Cloudflare Tunnel alters the body cap or the timeout.
