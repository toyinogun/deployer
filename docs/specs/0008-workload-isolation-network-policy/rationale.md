# 0008. Workload isolation and network policy: rationale

The reasoning behind [index.md](index.md). Not read during a build.

## Context

> ⚠️ Premise note: the scope names this slice "workload isolation and network policy", but four of the five things its "done when" asks for already ship. Non root, dropped capabilities, resource ceilings, the per app namespace, and the impossibility of a caller injecting a privileged field were all built into slice 1's deploy path, exactly as spec 0001's own premise note argued they should be. What has never existed is the network half: this cluster has run seven AI deployed apps with unrestricted reach to every other workload on it. So this is not a hardening pass across five dimensions; it is one missing dimension plus a set of regression tests pinning the four that are already right. Framing it that way is what keeps the slice small enough to get correct.

The platform's whole purpose is to take code an AI wrote and run it inside the cluster that also runs your real services: n8n, Longhorn, a Postgres cluster, ArgoCD, cert-manager, a Tailscale operator. Those services are on the same pod network as every app the platform deploys. Cilium is installed and genuinely enforcing, so a NetworkPolicy would work, and there is not a single one in any namespace the platform owns.

The forces are concrete, taken live from the cluster on 2026-08-13 rather than assumed:

| Fact | Value |
|---|---|
| Pod CIDR | `10.42.0.0/16` (per node `/24`s across the four nodes) |
| Service CIDR | `10.43.0.0/16`, API server on `10.43.0.1` |
| Nodes | `172.16.70.20` to `.23` |
| Ingress load balancer | `172.16.70.40` |
| `ingress-nginx` namespace labels | `kubernetes.io/metadata.name=ingress-nginx`, `name=ingress-nginx` |
| `ingress-nginx` pod networking | two controller pods, `hostNetwork` unset, service `externalTrafficPolicy: Local` |
| Existing NetworkPolicy in the cluster | only in `argocd` and `provic`, all standard `networking.k8s.io` kind |
| CiliumNetworkPolicy in use | none |
| App namespaces already running unpoliced | seven |

Two constraints shape everything. First, the platform composes every manifest field by field in Go and no caller supplied value ever reaches a pod spec, which is what makes an isolation guarantee statable at all. Second, the control plane's RBAC already grants `networkpolicies` inside app namespaces (`deploy/rbac.yaml`, granted during slice 1 so RBAC would not need reopening), so this slice adds no cluster right.

The cost of not deciding is that the isolation the scope calls non negotiable stays a claim about pod security and nothing about the network, on a platform whose entire input is code nobody reviewed.

## Options considered

### Option 1: Standard NetworkPolicy, composed in Go, deny by default with three narrow allows

Two objects per app namespace written by the same code that writes the Deployment: a deny all, and an allow of ingress from the ingress controller, egress to CoreDNS, and egress to the internet with private ranges excepted.

**Pros**:
- The type is already in the typed client the platform uses, the RBAC already exists, and the kind is already in use elsewhere on this cluster, so nothing new has to be operated.
- Egress by CIDR exception expresses the actual requirement exactly: an app may talk to the world, and not to anything of yours.
- Composed in Go means the policy is derived from the slug and configuration and is testable without a cluster, matching every other manifest the platform writes.

**Cons**:
- Cannot express "this app may reach `api.stripe.com`", only "this app may reach public addresses". Exfiltration is out of scope, and stays out.
- Two more API writes on the deploy path, and two more ways for a deploy to fail.
- An `except` list that is wrong in either direction fails quietly: too wide and an app is fenced out of the internet, too narrow and the fence has a hole nothing tests.

### Option 2: CiliumNetworkPolicy

The CRD Cilium adds on top of the standard kind, with DNS aware egress (`toFQDNs`) and L7 rules.

**Pros**:
- Egress could be an allow list of hostnames rather than a block list of ranges, which is a strictly stronger boundary.
- L7 rules could restrict an app to specific HTTP methods or paths on a destination.

**Cons**:
- Needs a CRD type in the platform's client, so either a generated client or unstructured objects, and it puts a Cilium version dependency into the platform's contract for a cluster that could otherwise run any CNI that enforces NetworkPolicy.
- The stronger feature is unusable: the platform cannot know which hostnames an AI written app needs, and nothing in the scope lets a caller declare them. An allow list nobody can populate collapses back to allow everything or deny everything.
- More to understand at 2am, for a capability the product cannot supply inputs for.

### Option 3: Namespace isolation only, no egress rules

Deny ingress from everywhere except ingress-nginx, and leave egress alone.

**Pros**:
- Half the rules, no CIDR list, no configuration, nothing to get wrong about DNS.
- Closes app to app reachability, which is the headline of the feature.

**Cons**:
- Leaves the direction that actually matters open. An app that wants your Postgres does not need to be reached, it needs to reach, and a deny ingress policy stops neither that nor a connection to the Kubernetes API.
- Would have to be reopened almost immediately, and reopening a security boundary is where mistakes live.

## Rationale

Option 1 wins on the two forces that dominate: the cluster inventory, and what the product can actually know.

The inventory says standard NetworkPolicy is free here. Cilium enforces it, the typed client already carries the type, the RBAC was granted in slice 1, and two of your own namespaces already use the kind, so a human debugging this at 2am is reading the same object shape they already read in `provic`. Option 2's advantage is real but unusable: `toFQDNs` is a better fence only if something can populate the list, and the platform's entire input is a tarball plus a slug. Until slice 7 gives an app a way to declare configuration, there is nobody to ask which hostnames it needs, so the CRD would buy a dependency and no boundary. That is why it is recorded as a Follow-up against slice 7 rather than dismissed.

The blocked CIDR list is deliberately whole RFC1918 plus link local plus the Tailscale CGNAT range, rather than the four exact ranges this cluster uses. Naming `10.42.0.0/16` and `10.43.0.0/16` precisely would leave the rest of `10.0.0.0/8` reachable, and the point of a fence is not to be exactly the shape of today's cluster. `100.64.0.0/10` is on the list because you reach this cluster over Tailscale; without it an app could reach every device on your tailnet, which is a larger network than the LAN it was fenced off from.

One cluster fact in that table is load bearing and was checked rather than assumed. The ingress controller pods do not use host networking, so traffic arriving at an app pod carries the `ingress-nginx` namespace identity and the namespace selector in AC-3 matches it. Had those pods been host networked, which is common on bare metal installs fronted by a load balancer IP, Cilium would have seen the traffic as coming from the host rather than from that namespace, the rule would not have matched, and every app on the platform would have gone dark on the first deploy while every unit test still passed. That is the shape of failure this design is most exposed to, so the fact is recorded here rather than left implicit.

It is also worth saying plainly what the three allow rules really are, because the mental model is simpler than the list suggests: one inbound rule, one outbound block by range, and a single deliberate hole in that block for DNS. Everything in the cluster other than CoreDNS is unreachable for the same reason, not for three different ones.

Two smaller calls worth recording. Applying the policies over any existing object contradicts the create once and never edit rule that governs the namespace, quota and LimitRange, and it should: that rule exists so the platform cannot move a fence a human tightened, and its content there is partly a human's. A policy composed entirely from code and configuration has no such ambiguity, so rewriting it can only restore, never loosen. And the startup sweep reads namespaces from the cluster rather than apps from the database, because the namespace whose database row went missing is precisely the orphan you least want running unpoliced, and the cluster is the only thing that knows it is there.

The engineer chose not to police `deployer-system` in this slice. That leaves any workload on the cluster able to reach the platform API, which is guarded by tokens rather than by network, so it is a defence in depth gap rather than an open door. It is recorded as the first Follow-up.
