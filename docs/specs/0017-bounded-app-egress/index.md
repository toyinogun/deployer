# 0017. Bounded app egress: closing the ports abuse actually uses

**Date**: 2026-08-14
**Status**: In Progress

## Summary

An app the platform runs can currently open a connection to any port on any public address. That is how a stranger's app sends spam straight to mail servers or joins a mining pool, and both cost you your relationship with your internet provider rather than just some CPU. This decision closes a small, named set of outbound TCP ports (port 25, the one spam uses, and the common mining pool ports) by turning the existing wide open egress rule into a list of port ranges that steps around them. Ports rather than hostnames, so no app owner has to declare anything, and the ports a legitimate app needs (443 for web calls, 587 for sending mail through a real provider) stay open.

## Requirements

**User stories**:
- As the platform owner, I want an app deployed by a stranger to be unable to send mail directly to the internet, so that a spam run from my cluster does not put my home address on a blocklist and cost me my internet provider.
- As the platform owner, I want the common mining pool ports closed, so that the cheapest and laziest way to abuse free compute does not work.
- As an agent deploying an app, I want an ordinary web application to work unchanged, including sending mail through a provider, so that the bound never becomes a thing I have to ask permission around.

**Acceptance criteria**:

- **AC-1**: The blocked port list comes from `DEPLOYER_APP_EGRESS_BLOCKED_PORTS`, validated in `internal/config` at startup. Default `25,3333,4444,5555,7777,9999,14444`. Each entry is parsed as an integer in `1..65535`, then deduplicated and sorted. Unset or empty falls back to the default, since `os.Getenv` cannot tell the two apart and a ConfigMap that omits the key must still boot bounded. A value that is set but unparseable, out of range, or leaving no usable entry (`,  ,`) stops the process at boot rather than at first deploy. A list whose complement over `1..65535` is empty is also refused at boot, for the same reason spec 0008 refuses an empty `except` list: it silently inverts the rule, here into an app with no outbound TCP at all.
- **AC-2**: A pure function in `internal/deploy` turns the blocked list into its complement over `1..65535` as an ordered list of ranges. It handles adjacent blocked ports (no empty range between them), duplicates, and the boundary values `1` and `65535` (no range starting at `0` and none ending at `65536`), and never emits an inverted range. A range one port wide carries `EndPort` equal to `Port` rather than an unset `EndPort`, so every composed entry has one shape.
- **AC-3**: `app-allow`'s public egress rule carries a ports list: one TCP entry per derived range, expressed as `port` plus `endPort`, and one UDP entry that sets `Protocol` explicitly to UDP and leaves `Port` unset. The explicit protocol is load bearing, not stylistic: `NetworkPolicyPort.Protocol` defaults to TCP when unset, so a UDP entry composed without it means "all TCP, any port" and reopens the whole bound. Every TCP port outside the blocked set stays reachable, every blocked TCP port does not, and all UDP stays open.
- **AC-4**: The `ipBlock` peer and its `except` list are byte for byte what spec 0008 composes, and that rule carries exactly one peer. The private range fence and app to app isolation are untouched by this change, and a second peer added to this rule later would silently inherit the ports list, so the single peer is pinned by a test rather than left as a convention.
- **AC-5**: A deployed app cannot open TCP 25 to a public mail exchanger that does listen on 25, and cannot open TCP 3333 to a public mining pool that does listen on 3333. Each reads as `timeout`, not `refused`, which is what distinguishes the fence from an absent listener. Proved live, not in unit tests.
- **AC-6**: A deployed app can still open TCP 443 to a public host and TCP 587 to a public mail relay. Both read as `reached`. Proved live.
- **AC-7**: The probe app in `testdata/probe` gains four targets, reported in the existing `{target, address, outcome, ms}` shape with the existing 3 second dial timeout, so the whole result is one HTTP read as it is today. The targets are `aspmx.l.google.com:25`, `pool.supportxmr.com:3333`, `smtp.sendgrid.net:587` and the existing public host on 443, each chosen because it genuinely listens on that port.
- **AC-8**: The same probe is run once **before** the change lands and reads `reached` on 25 and on 3333. Without that reading, a `timeout` afterwards proves nothing: many home internet connections already block outbound 25 at their own edge, so AC-5 would be satisfied by the provider rather than by the policy, and a regression such as a mis composed UDP entry would pass unnoticed. If the before run shows 25 already blocked upstream, that is recorded here and AC-5 is carried by the stratum port alone.
- **AC-9**: Both build namespace policies (`deploy/builds-networkpolicy.yaml` and `deploy/builds-dockerfile-networkpolicy.yaml`) restrict public egress to TCP 80 and TCP 443. Their DNS rule, their `deployer-system` rule and their `except` list are unchanged.
- **AC-10**: A real build completes end to end under both tightened build policies: `testdata/sample-buildpacks` through the Buildpacks path and `testdata/sample-dockerfile` through the Dockerfile path, each fetching dependencies and pushing its image. If those fixtures are named differently in the tree, the two existing fixtures the build tests already use stand in.
- **AC-11**: App namespaces that existed before this change carry the port bound with no redeploy, through the `PolicySweep` that already runs at startup.
- **AC-12**: An app's inbound path is unchanged: it still serves on its own hostname through ingress and its readiness probe still succeeds.
- **AC-13**: `deploy_app`'s description names the bound in prose: outbound mail on port 25 and the common mining pool ports are closed, and an app that sends mail should use a provider's relay on 587. It names the shape, not the literal list, so a configuration change cannot falsify it.
- **AC-14**: A unit test pins the composed `app-allow` ports list against the eight literal ranges written in this spec, not against the output of the complement function that produced them, so a broken complement function fails rather than agreeing with itself. It asserts on `Protocol` as well as on the numbers, and it fails when the config default changes without the policy following.

## Decision

**Chosen option**: Option 1: Port ranges on the existing `app-allow` egress rule, with an allow list of two ports for the build namespaces.

Give the existing `0.0.0.0/0` egress rule an explicit ports list, derived in Go from a configured blocked port list as the complement over the full port space, and separately narrow both build namespace policies to TCP 80 and 443.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`)

## Rationale

Reasoning, the options weighed, and the live cluster facts this rests on: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**: no schema change, for the same reason as spec 0008. A port bound is derived entirely from configuration, so the cluster is the only place it needs to exist. `deployments.failure_reason` gains no value; nothing about this can fail a deploy that spec 0008's policy write did not already fail.

**State transitions**: none new. The change is inside the object `ApplyNetworkPolicies` already writes, at the point in the reconcile it already writes it.

**The changed rule.** `app-allow`'s second egress rule keeps its peer exactly as it is and gains a ports list:

| Direction | Peer | Ports |
|---|---|---|
| Ingress | `namespaceSelector: kubernetes.io/metadata.name=ingress-nginx` | TCP 8080, unchanged |
| Egress | CoreDNS pods in `kube-system` | UDP 53, TCP 53, unchanged |
| Egress | `ipBlock: 0.0.0.0/0`, `except:` the blocked CIDRs, unchanged, and the only peer in its rule | **new**: one TCP entry per allowed range, plus one UDP entry with `Protocol` set and `Port` unset |

With the default blocked list the derived TCP ranges are:

`1-24` · `26-3332` · `3334-4443` · `4445-5554` · `5556-7776` · `7778-9998` · `10000-14443` · `14445-65535`

Eight TCP entries and one UDP entry, in one rule, on one peer. A Kubernetes NetworkPolicy can only ever permit a port, never refuse one, so a blocked port is a port that appears in no range. That is why the change edits the existing rule rather than adding a new one: adding rules to a NetworkPolicy can only widen what is permitted.

**The build namespaces.** Both static policies swap their unported public egress rule for the same peer carrying `TCP 80` and `TCP 443` and nothing else. A build fetches dependencies and base images, which is HTTP and HTTPS, so an allow list of two ports is both stronger and simpler than eight ranges. It also means the port list is never restated in YAML, so unlike the CIDR list there is no second copy to drift.

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| compose `app-allow` | the blocked port list | `DEPLOYER_APP_EGRESS_BLOCKED_PORTS`, parsed and sorted in `internal/config` at startup |
| compose `app-allow` | the allowed TCP ranges | derived from that list by the pure complement function in `internal/deploy`, never configured directly |
| compose `app-allow` | UDP reach | a constant decision, not derived from the list: all UDP ports, always |
| compose `app-allow` | the peer and its `except` list | unchanged, spec 0008's `deploy.Input.EgressBlockedCIDRs` |
| build namespace policy | the two allowed public ports | static literals in the two YAML files, beside the rules they sit in |
| `deploy_app` description | the ports it names | prose describing the default's shape, deliberately not the literal list |
| probe report | `outcome` per target | the dial itself, in the vocabulary spec 0008 defined (`reached`, `refused`, `timeout`, `dns_failed`) |
| probe report | the four target addresses | named literals in `testdata/probe`, listed in AC-7, each chosen because it genuinely listens on that port |
| probe report | whether a `timeout` on 25 is the policy | the baseline run in AC-8, not the post change run on its own |

**Key invariants**:
- A blocked port is a port named in no allowed range. There is no deny rule anywhere, because the object kind has none.
- The complement function is the only thing that decides what is open. Nothing else may add a port to the policy, or the list stops being the single description of the bound.
- Every port entry states its protocol. An unset `Protocol` is TCP by API default, so an entry that means to be UDP and does not say so becomes the widest TCP rule in the object.
- The public egress rule carries exactly one peer. A `ports` list applies to every peer under `to` in the same rule, so a second peer added there later silently inherits this bound, which is a way to weaken or break an unrelated destination without touching a line of this design.
- The blocked list is a bound on abuse, not a tuning knob. Removing a port from it opens that port for every app on the platform at once, the same way the CIDR list works in spec 0008.
- UDP is unconditioned by the list. The bound is about TCP services, and quietly narrowing UDP would break HTTP/3 and time synchronisation with no matching benefit.
- The build namespaces express the same intent the opposite way round, as an allow list of two ports. Adding a port there is a deliberate edit to a manifest under ArgoCD, not a configuration change.
- A change to `DEPLOYER_APP_EGRESS_BLOCKED_PORTS` reaches a running app on its next deploy or on the next control plane restart through `PolicySweep`, not immediately. Same as the CIDR list, and for the same reason: nothing watches configuration at runtime.

**Security model**: unchanged as to who may do what. This narrows what a running workload may reach, exactly as spec 0008 did, and needs no RBAC change: the control plane already holds `networkpolicies` in `ClusterRole/deployer-app`. No new compliance scope.

**Configuration required**:
- `DEPLOYER_APP_EGRESS_BLOCKED_PORTS`: comma separated TCP ports an app may not reach on any public address. Default `25,3333,4444,5555,7777,9999,14444`, which is direct to mail exchanger delivery plus the stratum ports mining pools most often listen on. Unset or empty means the default, because a ConfigMap that omits the key has to boot bounded rather than open. A set value that parses to nothing is rejected for the same reason an empty CIDR list is, and so is one that leaves no port open at all: both ends of the range are a silent inversion of what the rule is for.

**Critical test scenarios**:
- Complement, pure and test first: adjacent blocked ports, duplicates, a blocked `1`, a blocked `65535`, a one port wide range carrying `EndPort` equal to `Port`, and the default list, verifies **AC-2**.
- Configuration: the default when unset and when empty, an override, and a refusal to boot on an unparseable entry, a port `0`, a port `65536`, a set value leaving nothing, and a list whose complement is empty, verifies **AC-1**.
- Composition: the composed list matches the eight literal ranges, the UDP entry carries an explicit protocol, the peer and `except` list are unchanged, and the rule holds exactly one peer, verifies **AC-3**, **AC-4**, **AC-14**.
- Baseline, live and before the change: the probe reads `reached` on 25 and 3333, which is what makes the later block attributable to the policy rather than to the upstream connection, verifies **AC-8**.
- Blocked, live: the probe reads `timeout` dialling a listening mail exchanger on 25 and a listening pool on 3333, verifies **AC-5**, **AC-7**.
- Allowed, live: the probe reads `reached` on 443 and on a relay's 587, verifies **AC-6**.
- Retrofit: an app namespace deployed before this change carries the ports list after a control plane restart with no redeploy, verifies **AC-11**.
- No regression: the app still serves through ingress and stays ready under the narrowed policy, verifies **AC-12**.
- Build: one Buildpacks build and one Dockerfile build each complete under the two port build policy, verifies **AC-9**, **AC-10**.
- Contract: `deploy_app`'s description names the bound, verifies **AC-13**.

## Build plan

Ordered as a Tracer Bullet: one app is really, provably bounded against a real listening mail server before anything is generalised or the build namespaces are touched. The baseline run comes first, because it is the only step that cannot be done after the fact.

1. [ ] Extend `testdata/probe` with the four named targets, deploy it through the real `deploy_app` path on the platform as it stands today, and record the report. All four must read `reached`; anything else means the upstream connection is already doing the blocking and the later proof has to be read accordingly, satisfies **AC-7**, **AC-8**.
2. [x] Add `DEPLOYER_APP_EGRESS_BLOCKED_PORTS` to `internal/config` beside the CIDR list, with its default, parse, range check, deduplication and sort, and every boot refusal including the empty complement, satisfies **AC-1**.
3. [x] Write the complement function in `internal/deploy` test first, covering adjacency, duplicates, both boundaries and the one port wide range, satisfies **AC-2**.
4. [x] Thread the list into `deploy.Input` and give `AllowPolicy`'s public egress rule its ports list, with the explicit UDP protocol, and unit tests over the eight literal ranges, the protocol, the untouched peer and the single peer rule, satisfies **AC-3**, **AC-4**, **AC-14**.
5. [ ] Deploy the probe again and read the report against the live cluster: 25 and 3333 now time out where step 1 read reached, 443 and 587 are still reached, the app still serves and stays ready, satisfies **AC-5**, **AC-6**, **AC-12**.
6. [ ] Confirm the retrofit on an app namespace that predates the change, with a control plane restart and no redeploy, satisfies **AC-11**.
7. [ ] Narrow both build namespace policies to TCP 80 and 443 and prove both build paths still complete end to end, satisfies **AC-9**, **AC-10**.
8. [x] Add the bound to `deploy_app`'s description, naming the shape rather than the literal ports, satisfies **AC-13**.

## Consequences

**Positive**:
- The two abuse patterns that cost you something outside the cluster, direct to mail exchanger spam and pool mining, stop being one line of an app's code away.
- It costs an app owner nothing. Nobody declares anything, no deploy is refused, and the ports a normal application uses are all open.
- The build namespaces end up on an allow list of two ports, which is a genuinely tighter fence than the apps get, and it removes a second copy of a list rather than adding one.
- The bound is one configured line, so adding a port later is a ConfigMap edit and a restart.

**Negative / tradeoffs**:
- The mining half is weak and worth being honest about. Most pools also listen on 443, and a payload that wants to move can. Port 25 is the half that genuinely holds, because a mail exchanger cannot move off 25.
- A blocked connection is a silent timeout, not an error. An app that tries to send mail on 25 hangs for its own timeout and reports nothing useful, and the app's owner has no way to discover why. The `deploy_app` description is the only place that explains it, and only an agent that reads descriptions benefits.
- Naming ports on that rule turns it from "any protocol, any port" into an enumerated set, so SCTP outbound, which is permitted today, becomes denied. Nothing plausible on this platform uses it, and denied is the safe direction to be wrong in, but it is a real narrowing nobody asked for.
- Eight ranges is less readable than one open rule. Anyone reading the live policy sees an arithmetic puzzle rather than a list of blocked ports, and has to invert it in their head or read this spec.
- A build that fetches a dependency over SSH on 22 or from a registry on a non standard port now fails. It fails loudly at build time rather than silently at runtime, which is the better failure, but it is a new one.
- Two more things can now break a working app for a reason its owner cannot see, and both of them look like the app being slow.

**Neutral**:
- The port bound and the CIDR bound compose without interacting: the `except` list decides which addresses, the ports list decides which ports on the ones that are left.
- `PolicySweep` already exists and already walks every managed namespace, so retrofitting existing apps costs nothing new. This is the second time that sweep has paid for itself.
- Cilium has enforced standard NetworkPolicy `endPort` since well before 1.16, so this is a thing to confirm rather than a cliff edge. It still cannot be confirmed in a unit test: the fake clientset will happily compose a policy the cluster ignores, so build step 5 is not optional and its live result is the real acceptance of **AC-3**.
- The two probe targets that must genuinely listen belong to other people. If a pool moves off 3333 or a mail exchanger stops answering, the check turns flaky or quietly false rather than failing honestly. Running your own listener outside the cluster would fix that and was judged not worth the standing infrastructure for a homelab, which is a choice to revisit if the check ever misleads you.

## Follow-up

- [ ] If a live check ever shows `endPort` unenforced on this Cilium version, the fallback is a `CiliumClusterwideNetworkPolicy` with `egressDeny` rules and `enableDefaultDeny: false`, which is option 2 in the rationale and needs no Go change.
- [ ] Egress by hostname stays deferred and unchanged by this. It is the only thing that bounds exfiltration rather than abuse, and it still breaks every app that calls an API until its owner declares it.
- [ ] Revisit the mining port list if it ever blocks something real, or delete it if it never blocks anything. It has no way to tell you either, which is worth remembering when it is a year old.
- [ ] Nothing tests that `deploy_app`'s description matches the bound, which is the eighth instance of the untested tool description gap the scope already tracks.

## Migration plan

**Strategy**: one control plane deployment plus one ArgoCD manifest change, with the existing startup sweep doing the retrofit.

**Phases**:
0. Run the probe on the platform as it stands and record all four targets as `reached`. This is a phase rather than a note because it cannot be done afterwards, and without it the next phase's result is not attributable to anything.
1. Ship the control plane change. On boot, `PolicySweep` rewrites `app-allow` in every managed namespace with the ports list, and every later deploy composes it as part of reconcile. Run the probe again and compare against phase 0.
2. Ship the two build namespace YAML files through ArgoCD, then run one Buildpacks build and one Dockerfile build to confirm both still complete.

**Rollback**: revert the commit and restart. The next `PolicySweep` rewrites `app-allow` without the ports list, and reverting the manifests restores the build policies. Nothing is written to the database and no app manifest changes, so a revert is complete.

**Risks**: an app already running that legitimately talks to one of these ports starts failing at the moment of the sweep, and it fails as a timeout inside the app rather than as a platform error. Port 25 is the plausible case, an app sending mail directly rather than through a relay. Phase 1's probe run characterises the bound before it is trusted, but it cannot tell you which existing app depended on what. With seven or so apps and one operator this is a small enough surface to check by hand if something goes quiet.
