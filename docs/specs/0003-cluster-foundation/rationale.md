# 0003. Cluster foundation, rationale

The build spec is [index.md](index.md). This file is the reasoning behind it and is not read during a build.

## Context

> ⚠️ Premise note: pointing ingress-nginx at a default certificate is a controller wide setting, and that controller already serves twelve applications of yours. Today a host with no certificate of its own gets nginx's built in fake certificate and a loud browser warning. After this change it gets a real, trusted certificate for the wrong name, which is a quieter and more confusing failure. The chosen design accepts this because every existing app has its own certificate through cert-manager and is unaffected, but it is the sharpest edge here. If you would rather not touch a shared controller at all, the honest fix is a second ingress controller dedicated to deployed apps (Option 3 below), at the cost of another controller to run and upgrade. Follow-up in [index.md](index.md) records the middle path.

Specs 0001 and 0002 decided what the platform is and what it stores. Neither decided how it actually sits in the cluster. Spec 0001 explicitly handed three things here: the namespace policy, the control plane's service account rights, and how one wildcard hostname plus TLS reaches an app container.

The forces are unusually concrete, because the cluster already exists and was inventoried on 2026-08-11. k3s v1.35.5 across four nodes, Cilium enforcing NetworkPolicy, ingress-nginx 1.11.3 on 172.16.70.40 plus a `tailscale` ingress class, cert-manager v1.16.2, Longhorn as the default StorageClass, ArgoCD driving twelve healthy apps from `k3sprox-gitops`, and sealed-secrets. Nothing in this decision should install a component that overlaps one of those.

Two constraints shape everything else. First, the code being deployed was written by an AI and is not trusted, so the confinement has to be enforced by the cluster rather than by the platform remembering to be careful. Second, deployed apps must be reachable only to members of the operator's tailnet, which rules out Let's Encrypt HTTP-01 entirely, because Let's Encrypt cannot reach a host that is not publicly routable.

The consequence of not deciding is that slice 1, the tracer bullet, has nowhere to land. It cannot create a namespace it has no rights to, and it cannot return a working URL if no hostname resolves and no certificate exists.

## Options considered

### Option 1: One Tailscale device fronting the existing ingress-nginx, shared wildcard certificate as the controller default

A single Tailscale device is the tailnet entry point for all deployed apps. Behind it, the ingress-nginx controller your cluster already runs matches per app Ingress objects by hostname. cert-manager obtains one wildcard certificate through a Cloudflare DNS-01 challenge and nginx serves it as its default, so an app Ingress carries no TLS configuration at all.

**Pros**:

- App create and delete stay pure Kubernetes operations. Nothing in the deploy path touches Tailscale, Cloudflare, or cert-manager.
- One certificate issuance, ever. Rate limits cannot bite, renewal is one thing to watch, and no per app hostname is published to Certificate Transparency logs.
- The wildcard private key lives in exactly one namespace and never enters an app namespace, which is exactly what slice 5's isolation goal wants.
- Reuses four components already running, and adds no new controller.

**Cons**:

- Changes a setting on a controller that twelve other apps depend on. See the premise note.
- Every tailnet member can reach every deployed app. There is no per app access control.
- The single fronting device and the single certificate are both shared failure points: if either breaks, every app breaks at once.

### Option 2: One Tailscale device per app, `ts.net` hostnames, no cert-manager at all

The Tailscale operator gives each deployed app its own tailnet identity and its own valid certificate. No wildcard domain, no DNS records, no DNS-01 challenge, no Cloudflare token.

**Pros**:

- The least infrastructure of any option. Certificates and DNS simply do not exist as concerns.
- Real per app access control becomes possible through tailnet ACLs, which Option 1 cannot offer.
- No shared certificate and no shared blast radius on renewal.

**Cons**:

- Every app create and delete now touches Tailscale as well as Kubernetes, so a deploy has a second system that can fail, and a leaked or half deleted app leaves a stale device behind.
- Hostnames live in Tailscale's namespace, not the operator's domain, which contradicts `DEPLOYER_APP_DOMAIN` already being a required setting in spec 0001 and in the shipped `internal/config`.
- Device count grows with app count, and these are meant to be throwaway apps an agent generates.

### Option 3: A dedicated app only ingress controller behind its own Tailscale device

Same as Option 1, but deployed apps get their own ingress-nginx instance on a separate MetalLB address, so nothing about untrusted apps touches the controller serving the operator's existing apps.

**Pros**:

- Removes the premise note entirely. The default certificate flag, the configuration, and the version of that controller are all Deployer's alone.
- A misbehaving app, a bad annotation, or a controller upgrade cannot affect the twelve existing apps.
- Clean ownership boundary for a component whose whole job is serving untrusted workloads.

**Cons**:

- A second ingress controller to install, configure, monitor, and keep upgraded, for a homelab with a single operator.
- Duplicates a component the cluster already runs well, which is exactly what spec 0001 set out to avoid.
- More moving parts before the tracer bullet can fire.

## Rationale

Option 1 wins on the constraint that matters most here, which is that a deploy must not have failure modes outside Kubernetes. The platform's job is to turn an agent's tarball into a URL, and every extra system in that path is a way for a deploy to half succeed. Option 2 puts Tailscale in the deploy path and Option 3 puts a second controller in the operating burden; Option 1 puts neither, and confines both Tailscale and Cloudflare to one time setup that a human does once and `verify.md` records.

The wildcard certificate as controller default is the specific choice that pays off repeatedly. It is what makes the app Ingress a five line object with no secret reference, what keeps a private key out of every app namespace, and what removes certificate issuance latency from the deploy path so an agent gets a working URL immediately rather than after a challenge round trip. The alternative of mirroring the wildcard secret into each app namespace would have handed a copy of the private key to exactly the workloads slice 5 is designed to distrust, which is backwards.

On rights, the narrow ClusterRole beats the tighter looking alternative of minting per namespace Roles at deploy time. That alternative reads as least privilege but is not: to create a RoleBinding you need the right to create RoleBindings, which is the right to grant yourself anything. It buys the appearance of a smaller blast radius by adding a genuine privilege escalation path, plus more machinery. One explicit ClusterRole that a reviewer can read in full is the more honest security posture, and the one genuinely broad right left in it, namespace delete, is called out in Follow-up with the admission policy that would close it.

A fourth path was raised in review and is worth recording as rejected: advertising the node running ingress-nginx through a Tailscale subnet router, rather than exposing the controller Service through the operator. It is fewer Kubernetes objects, but it makes app reachability depend on a subnet route staying advertised and approved, and it brings every other address on 172.16.70.0/24 along with it. The operator exposed Service is the narrower boundary and was the engineer's choice.

Option 3 remains the right answer the day a deployed app causes a problem for the existing twelve. Nothing in this design blocks that move: the Ingress objects the control plane writes name their class through `DEPLOYER_INGRESS_CLASS_NAME`, so switching to a dedicated controller later is a configuration change rather than a rewrite.
