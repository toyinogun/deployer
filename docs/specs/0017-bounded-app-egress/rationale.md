# 0017. Bounded app egress: rationale

The reasoning behind [index.md](index.md). Not read during a build.

## Context

Spec 0008 fenced an app's network in one direction only. It decided which addresses an app may reach, carving every private range out of `0.0.0.0/0` so an app cannot see the cluster it runs on, the home network, or a sibling app. It deliberately said nothing about which ports, and its own Consequences names the gap: egress is allow by range, not by name, so an app may reach any public host. What it did not spell out is that it may also reach any public host on any port.

That was safe while the tailnet was the outer fence, because every account belonged to you. Slice 12 removes that fence: the platform is handed to invited strangers, and the apps it runs are written by an AI on their behalf. Two abuse patterns follow directly, and both are notable because their cost lands outside the cluster rather than inside it. An app that opens port 25 to public mail exchangers can run a spam campaign from your home address, which gets your address on a blocklist and puts you in a conversation with your internet provider. An app that opens a stratum port to a mining pool spends your electricity and your CPU, which a resource quota already bounds but does not stop.

Neither is bounded today by anything. The resource quotas cap what one app consumes; nothing caps what it does with it. The CIDR fence stops it reaching your services; it does nothing about the open internet, which is exactly where both abuses point. The scope has already settled the shape of the answer for this slice: ports rather than hostnames, because a hostname allow list breaks every app that calls an external API until its owner declares it, and the owners here are explicitly not technical.

One constraint shapes every option. A standard Kubernetes NetworkPolicy is a permit list and only a permit list. It has no way to express "everything except this port", and adding a rule can only widen what is allowed. So "close these ports" is not directly expressible in the object the platform already writes, and every option below is a different way around that.

## Options considered

### Option 1: Port ranges on the existing `app-allow` rule

Give the existing public egress rule a ports list containing the complement of the blocked set, expressed as ranges with `port` plus `endPort` (standard since Kubernetes 1.25). Eight TCP ranges plus one UDP entry replace one unported rule. The build namespaces go the other way, to an allow list of TCP 80 and 443, since a build has no use for anything else.

**Pros**:
- Nothing new exists afterwards. Same object kind, same Go composition, same reconcile point, same `PolicySweep` retrofit, same typed fake clientset in tests. The change is a field on a struct that is already written.
- No new CRD, no new RBAC, no new client. The control plane's dependency surface is unchanged.
- Existing apps are picked up for free by the sweep spec 0008 already built.
- The bound stays inside the platform's own self healing fence: a policy someone loosens by hand is restored on the next deploy, ports included.

**Cons**:
- Eight ranges to express seven blocked ports is an inversion the reader has to undo. The live policy no longer says what it means.
- It relies on Cilium enforcing `endPort`, which no unit test on this project can check. The fake clientset composes happily and the cluster may or may not agree.
- Naming ports at all narrows the rule from every protocol to the ones named, so SCTP goes from permitted to denied as a side effect.

### Option 2: A `CiliumClusterwideNetworkPolicy` applied by ArgoCD

One static manifest with `egressDeny` rules covering the blocked ports, selecting every namespace labelled `app.kubernetes.io/managed-by: deployer`. Cilium evaluates deny before any allow, so the existing policies stay untouched and the deny simply subtracts.

**Pros**:
- By far the smallest change: one file, no Go, no configuration, no test wiring.
- Says exactly what it means. The manifest lists the blocked ports, in order, as blocked ports.
- Covers app namespaces and both build namespaces in one object, including namespaces created later, with no sweep and no retrofit step.
- Deny semantics mean it cannot be widened by accident, which is the failure mode a permit list has.

**Cons**:
- Moves a security control out of the platform's own reconcile and into ArgoCD. The platform stops being able to state that it wrote the fence before it started the workload.
- A Cilium specific CRD, so the fence stops being portable to a cluster with a different network implementation. Every other policy on the platform is the standard kind.
- `enableDefaultDeny` must be set `false` or the object silently imposes deny all on every selected app. That is a footgun with a blast radius of every running app, in a file nothing in the Go test suite reads.

### Option 3: A `CiliumNetworkPolicy` per namespace, composed in Go

Deny semantics with the platform still owning and writing the fence, as a third object beside the two it already writes.

**Pros**:
- Keeps both good properties at once: real deny rules, and the platform writing the fence before the workload as its invariant requires.
- Reads as clearly as option 2 while retaining the self healing of option 1.

**Cons**:
- Drags a CRD dependency into the control plane: new RBAC for `cilium.io`, and either a generated Cilium client or unstructured objects through the dynamic client.
- The typed fake clientset the entire test suite is built on cannot express it, so this option costs a second testing approach for one object.
- Most machinery of the three, for a bound that is seven integers long.

## Rationale

Option 1 wins on the same ground spec 0008 chose the standard kind in the first place. The whole fence is already a pair of standard NetworkPolicy objects composed field by field in Go, written before the workload, swept across existing namespaces at startup, and covered by the typed fake clientset. Option 1 is a field on an object that already exists in all of that machinery. Options 2 and 3 both buy clearer semantics by introducing the platform's first Cilium specific object, and neither pays for itself: the bound is seven integers, and inverting seven integers into eight ranges is a twenty line pure function with an obvious test.

Option 2 was the closest call, and it is genuinely the lazier answer. Two things decided against it. The first is that `enableDefaultDeny: false` is load bearing and invisible: get it wrong and every app on the platform loses all network access, from a file no test in this repo reads. The second is that it moves the control out of the reconcile, which costs the platform its clean statement that it writes the fence before it starts the workload. It stays on record as the fallback, because if Cilium turns out not to enforce `endPort` on 1.16.5 it becomes the answer with no Go change at all.

On the port list itself, the recommendation deliberately departs from the scope row's wording, which reads as though all outbound mail should close. What actually costs you the relationship with your internet provider is direct to mail exchanger delivery, and that only happens on port 25. Ports 465, 587 and 2525 are authenticated submission to a relay someone already has an account with, which is rate limited and abuse managed by that provider, and it is exactly what a well behaved AI written app does when it sends a signup email. Closing them would break ordinary apps to prevent abuse that lands on someone else's doorstep. Blocking 25 alone gets the entire benefit.

The mining ports are in the list on the engineer's call and are worth being honest about in both directions. They cost nothing to include and they stop the laziest version of the attack, the one where a payload is configured for a default stratum port and nobody bothers to change it. They do not stop anyone who reads a pool's documentation, since the same pools offer 443. They are cheap insurance against low effort abuse, not a control.

The build namespaces went the other way for a reason worth keeping: a build's outbound needs are genuinely known and genuinely tiny. Dependencies and base images arrive over HTTP and HTTPS. An allow list of two ports there is stronger than the apps' bound, simpler to read, and it removes rather than adds a copy of a shared list, so the drift problem spec 0008 had to pin with a test never appears for ports at all.

## Cluster facts this rests on

Read live from the `k3sprox` context on 2026-08-14, before the options were put to the engineer:

- Kubernetes server 1.35, so `endPort` on a NetworkPolicy port is long past general availability rather than a feature gate question.
- Cilium `v1.16.5` is the network implementation, running as a DaemonSet in `kube-system`.
- The Cilium CRDs are installed, including `ciliumnetworkpolicies.cilium.io` and `ciliumclusterwidenetworkpolicies.cilium.io`, so options 2 and 3 were real choices rather than hypothetical ones.

What was not checked live, and is the assumption the build has to prove: that this Cilium version enforces `endPort` as written rather than accepting and ignoring it. Cilium has supported it since well before 1.16, so this is confirmation rather than a gamble, but no unit test on this project can reach it. Build step 5 in [index.md](index.md) is what settles it, and the fallback if it does not hold is option 2.

The second thing a live check has to establish is not about the cluster at all. Many home internet connections block outbound port 25 at their own edge, so a probe that times out on 25 after the change may be reading the internet provider rather than the policy. That is why the baseline run is build step 1 and migration phase 0: without a `reached` on 25 beforehand, the strongest acceptance criterion in this spec is unfalsifiable, and a mis composed policy that does nothing at all would pass it.
