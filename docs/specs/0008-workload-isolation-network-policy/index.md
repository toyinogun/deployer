# 0008. Workload isolation and network policy

**Date**: 2026-08-13
**Status**: Accepted

## Summary

Every app the platform deploys already runs in its own namespace, as a non root user, with no capabilities, no Kubernetes token, and hard CPU and memory ceilings. What none of them has is a network fence: today an app an AI wrote can open a connection to any other app, to your Longhorn and n8n and Postgres, to the Kubernetes API, to the image registry, and to every machine on your home network. This decision adds that fence. Each app namespace gets two NetworkPolicy objects (the standard Kubernetes kind, which Cilium enforces on this cluster): one that denies all traffic in and out, and one that allows back exactly three things, inbound from the ingress controller on port 8080, outbound to cluster DNS, and outbound to the public internet with every private range carved out. The build namespace gets the same treatment as static YAML, because a build pod runs an AI written project's dependency install beside a registry push credential.

## Requirements

**User stories**:
- As the platform owner, I want an app an agent deployed to be unable to reach my other apps or my cluster services, so that running AI written code next to my real services is a bounded risk rather than a hope.
- As an agent deploying an app, I want the app to still resolve names and call external APIs, so that a normal web service works without me asking for network permission.
- As the platform owner, I want the fence to repair itself, so that a policy someone deleted by hand does not leave an app open forever.

**Acceptance criteria**:

- **AC-1**: Every app namespace holds exactly two NetworkPolicy objects, composed field by field in Go: `default-deny` and `app-allow`. Neither carries any caller supplied value.
- **AC-2**: `default-deny` selects all pods in the namespace (empty pod selector), declares both `Ingress` and `Egress` policy types, and carries no rules, so it denies everything in both directions.
- **AC-3**: `app-allow` permits ingress only from namespaces labelled `kubernetes.io/metadata.name: ingress-nginx`, and only on TCP `8080` (`deploy.ContainerPort`).
- **AC-4**: `app-allow` permits egress to pods labelled `k8s-app: kube-dns` in the namespace labelled `kubernetes.io/metadata.name: kube-system`, on UDP 53 and TCP 53, and to no other cluster destination.
- **AC-5**: `app-allow` permits egress to `0.0.0.0/0` on any port, with every configured blocked CIDR listed under `except`, so external destinations are reachable and private ranges are not.
- **AC-6**: A deployed app cannot open a connection to another app's pod IP, or to another app's Service IP. Proved live, not only in unit tests.
- **AC-7**: A deployed app cannot reach the Kubernetes API server, the in cluster image registry, or the control plane's internal URL.
- **AC-8**: A deployed app cannot reach a node IP or any other address on the home LAN, including the ingress load balancer, so an app cannot call its own or a sibling's public hostname.
- **AC-9**: A deployed app can resolve a public DNS name and open a connection to a public host on port 443.
- **AC-10**: The app stays reachable on its own hostname through ingress under the policy, and its readiness probe still succeeds, so no existing behaviour from specs 0003 to 0007 regresses.
- **AC-11**: Both policies are applied on every reconcile, overwriting whatever is there. A policy deleted or weakened by hand is restored on the next deploy of that app.
- **AC-12**: On startup `PolicySweep` walks every namespace labelled `app.kubernetes.io/managed-by: deployer` and applies both policies, so app namespaces created before this slice are policed without being redeployed. It runs before the existing deployment `Sweep`, and a failure on one namespace is logged and does not stop the rest.
- **AC-13**: The policies are written before the Deployment. A policy write that fails ends the deployment with reason `internal` and no Deployment is created, so an app is never running while unpoliced.
- **AC-14**: The blocked CIDR list comes from `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS`, validated in `internal/config` at startup by parsing each entry as an IPv4 CIDR. An unset or empty value falls back to the default list, since `os.Getenv` cannot tell the two apart and a ConfigMap that omits the key must still boot fenced. A value that is set but yields no usable entry (`,  ,`), an unparseable value, or an IPv6 entry stops the process at boot rather than at first deploy.
- **AC-15**: The `deployer-builds` namespace carries a NetworkPolicy pair delivered as static YAML in `deploy/`: deny by default both ways, egress to cluster DNS, egress to the `deployer-system` namespace on TCP 5000 and TCP 8080 only, and egress to `0.0.0.0/0` with the same private ranges excepted. No ingress is allowed at all.
- **AC-16**: A build still completes end to end under that policy: it fetches the source tarball from the control plane, downloads dependencies from the public internet, and pushes the built image to the registry.
- **AC-17**: A unit test pins the composed pod specs, for both the app Deployment and the build Job: `hostNetwork`, `hostPID`, `hostIPC`, `privileged`, `hostPath` volumes and any added capability are all absent, and `runAsNonRoot`, `allowPrivilegeEscalation: false`, `drop: ALL` and the seccomp profile are all present.
- **AC-18**: A unit test pins that `deploy.Input` carries no free form passthrough: no map, no annotations field, no environment passthrough, no override of any kind. This is what makes a privileged request unexpressible rather than merely rejected.
- **AC-19**: A probe app in `testdata/probe` deploys through the real `deploy_app` path and reports, over HTTP on `/probe`, the outcome of each attempted connection: a sibling pod IP, a sibling Service IP, the Kubernetes API, the registry, a node IP, the ingress load balancer, and a public host. It is deployed twice under two slugs so each instance is the other's sibling, every dial carries a 3 second timeout (a blocked destination is a silent drop, not a refusal, so an untimed dial hangs forever), and the response is a JSON array of `{target, address, outcome, ms}` where `outcome` is one of `reached`, `refused`, `timeout`, `dns_failed`.
- **AC-20**: A unit test parses `deploy/builds-networkpolicy.yaml` and asserts its `except` list matches the `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS` default in `internal/config`, so the two copies of the blocked range list cannot drift silently.

## Decision

**Chosen option**: Option 1: Standard NetworkPolicy composed in Go, deny by default with three narrow allows.

Each app namespace gets a `default-deny` and an `app-allow` NetworkPolicy, composed field by field in `internal/deploy` alongside the Deployment and Service, applied on every reconcile ahead of the workload, and swept across existing namespaces at startup; the build namespace gets an equivalent fixed pair as static YAML under GitOps.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`)

## Rationale

Reasoning, options weighed, and the live cluster inventory this rests on: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**: no schema change. Nothing about a network policy is worth a row: the policy is derived entirely from the slug and configuration, so the cluster is the only place it needs to exist. `deployments.failure_reason` gains no new value either (AC-13 reuses `internal`).

**State transitions**: none new. The policy write is a step inside the existing reconcile, between namespace creation and the Deployment write.

**The composed objects** (both in namespace `app-<slug>`, both labelled `app.kubernetes.io/managed-by: deployer`):

| Object | Selects | Policy types | Rules |
|---|---|---|---|
| `default-deny` | all pods | Ingress, Egress | none, which is what makes it a deny |
| `app-allow` | all pods | Ingress, Egress | the three below |

`app-allow` rules:

| Direction | Peer | Ports |
|---|---|---|
| Ingress | `namespaceSelector: kubernetes.io/metadata.name=ingress-nginx` | TCP 8080 |
| Egress | `namespaceSelector: kubernetes.io/metadata.name=kube-system` plus `podSelector: k8s-app=kube-dns` | UDP 53, TCP 53 |
| Egress | `ipBlock: 0.0.0.0/0`, `except:` the configured blocked CIDRs | all |

Two rules that look alike are not: the DNS rule pairs a namespace selector and a pod selector inside **one** peer, so it means "CoreDNS pods in kube-system", not "anything in kube-system or anything labelled kube-dns anywhere". Getting that wrong opens the whole kube-system namespace.

**The build namespace pair** (`deploy/builds-networkpolicy.yaml`, applied by ArgoCD): identical shape, with the ingress rule dropped entirely (nothing connects to a build pod) and one extra egress peer, `namespaceSelector: kubernetes.io/metadata.name=deployer-system` on TCP 5000 (registry) and TCP 8080 (the platform API the build pod fetches source from).

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| compose `app-allow` | the namespace it lands in | `deploy.NamespaceName(slug)`, the platform derived slug |
| compose `app-allow` | the allowed inbound port | `deploy.ContainerPort`, the existing constant |
| compose `app-allow` | the ingress controller's identity | a constant in `internal/deploy`; the `kubernetes.io/metadata.name` label is set by the API server, so it cannot be forged or drift |
| compose `app-allow` | the DNS pod's identity | a constant in `internal/deploy` (`kube-system` plus `k8s-app=kube-dns`), confirmed live on this cluster |
| compose `app-allow` | the blocked CIDR list | `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS`, parsed in `internal/config` at startup |
| startup sweep | the list of namespaces to police | the cluster: namespaces labelled `app.kubernetes.io/managed-by=deployer`, not the database |
| build namespace policy | the registry and API ports | static, in the YAML beside the Services they name |

**Where it runs**: the reconcile loop gains one `Cluster` method, `ApplyNetworkPolicies(ctx, namespace, deny, allow) error`, called in `deployApp` immediately after `EnsureNamespace` and before the pull secret write, so the fence exists before anything else in the namespace does. The startup pass is a separate function, `PolicySweep`, in `internal/reconcile`, run before the existing deployment `Sweep`; the two share a name only in English, and `PolicySweep` never touches deployment state.

**Key invariants**:
- A policy is written before the workload, never after. Failure to write one ends the deployment (AC-13).
- App to app isolation rides on the blocked CIDR list, not on a separate rule: another app's pod IP and Service IP are unreachable because the pod and service ranges sit inside the blocked ranges. Narrowing that list for an unrelated reason therefore weakens app to app isolation as a side effect, which is why the list is validated as a whole and not treated as a tuning knob.
- The policy content comes only from code and configuration. No caller supplied value reaches it, exactly as with the Deployment.
- The policies are applied, not created if absent. Overwriting can only restore the platform's own fence, never weaken it, because its content is not caller derived. This is the one place the slice departs from the create once and never edit rule that governs the namespace, quota and LimitRange, and it departs on purpose: those objects were left alone so the platform could not move a fence someone had tightened, and this one is rewritten so a fence someone loosened comes back.
- Egress is allow by exception. A new destination is reachable only by removing a range from the blocked list, never by an app asking.
- A policy is a namespaced object with no life of its own: deleting an app's namespace takes both policies with it, and the platform tracks them nowhere else. The self healing in AC-11 restores a policy inside a namespace that exists, and never recreates a namespace.
- A change to `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS` reaches a running app on its next deploy, or on the next control plane restart through `PolicySweep`, not immediately. Expected behaviour, not an oversight: nothing watches config at runtime.
- A policy write that fails leaves the namespace, its quota, its LimitRange and possibly its pull secret in place with no workload. Every one of those writes is idempotent, so a retried deploy converges rather than conflicting.

**Security model**: unchanged as to who may do what; this slice is about what a running workload may reach, not who may call the platform. The control plane already holds `networkpolicies` in `ClusterRole/deployer-app` (`deploy/rbac.yaml`), bound only into app namespaces, so no RBAC change is needed. No new compliance scope: the platform holds no regulated data.

**Configuration required**:
- `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS`: comma separated CIDRs an app and a build pod may not reach. Default `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10`, which covers the pod range (`10.42.0.0/16`), the service range (`10.43.0.0/16`), the nodes and the LAN (`172.16.70.0/24`), link local and cloud metadata, and the Tailscale CGNAT range. Unset or empty means the default, because a ConfigMap that omits the key has to boot fenced rather than open. Each entry is parsed as an IPv4 CIDR at startup; a value that is set but leaves no usable entry is rejected, because an empty `except` silently turns the fence into an open door, and an IPv6 entry is rejected because the allow rule names `0.0.0.0/0` only, so an IPv6 exception would parse cleanly and do nothing.

**Critical test scenarios**:
- Happy path: an app deploys, serves on its hostname through ingress, resolves a public name and reaches it on 443, verifies **AC-3**, **AC-5**, **AC-9**, **AC-10**.
- Isolation: the probe app reports every blocked destination as refused or timed out, verifies **AC-6**, **AC-7**, **AC-8**, **AC-19**.
- Drift: delete `app-allow` by hand, redeploy the app, confirm it is back and identical, verifies **AC-11**.
- Retrofit: an app namespace that existed before this slice has both policies after a control plane restart with no redeploy, verifies **AC-12**.
- Failure case: the policy write fails, the deployment ends `failed` with reason `internal` and no Deployment object exists in the namespace, verifies **AC-13**.
- Configuration: the process refuses to boot on an unparseable CIDR list, an IPv6 entry, or a set value that leaves no usable entry, and falls back to the default when the variable is unset or empty, verifies **AC-14**.
- Build: a build completes under the build namespace policy, verifies **AC-15**, **AC-16**.
- Structural: the composed pod specs carry no host or privileged field, and `deploy.Input` carries no passthrough, verifies **AC-17**, **AC-18**.

## Build plan

Ordered as a Tracer Bullet: one app gets a real, enforced, proven fence end to end before anything is generalised.

1. Add `DEPLOYER_APP_EGRESS_BLOCKED_CIDRS` to `internal/config` with its default, parsed and validated as IPv4 CIDRs at startup, and threaded into `deploy.Input`, satisfies **AC-14**.
2. Compose `DefaultDenyPolicy` and `AllowPolicy` in `internal/deploy`, field by field, with unit tests over the selectors, ports and `except` list, satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-5**.
3. Add `ApplyNetworkPolicies` to the reconcile `Cluster` interface and call it in `deployApp` after `EnsureNamespace` and before the pull secret, applying over any existing object, failing the deployment with reason `internal` on a write error, satisfies **AC-11**, **AC-13**.
4. Build the probe app in `testdata/probe` with its dial timeout and JSON report, deploy it twice through the real `deploy_app` path so each instance is the other's sibling, and confirm the blocked set is blocked and the allowed set is allowed against the live cluster, satisfies **AC-6**, **AC-7**, **AC-8**, **AC-9**, **AC-10**, **AC-19**.
5. Add `PolicySweep` over namespaces labelled `app.kubernetes.io/managed-by=deployer`, run at startup ahead of the deployment `Sweep`, logging and continuing past a per namespace failure, satisfies **AC-12**.
6. Add `deploy/builds-networkpolicy.yaml` to the Kustomization, add the test that pins its `except` list against the config default, and prove a real build still completes under it, satisfies **AC-15**, **AC-16**, **AC-20**.
7. Pin the isolation that already exists: the pod spec field test over the app Deployment and the build Job, and the `deploy.Input` structure test, satisfies **AC-17**, **AC-18**.

## Consequences

**Positive**:
- The scope's promise that isolation is enforced rather than advisory becomes true for the network, which was the one dimension where it was not.
- The blast radius of a hostile or merely buggy AI written app drops to: its own namespace, its own resources, and the public internet. It cannot see the cluster it is running on.
- The build pod, which runs an untrusted project's dependency resolution beside a registry push credential, stops being the quietest hole in the platform.
- The fence self heals, so a hand edit during debugging does not silently persist.

**Negative / tradeoffs**:
- An app cannot call its own public hostname, or any sibling's, because the ingress load balancer sits on the blocked LAN range. This is deliberate, and it will look like a bug the first time it bites. It is recorded here so the answer is one lookup away.
- Egress is allow by range, not by name. An app may reach any public host, so this bounds the cluster, not exfiltration. Blocking by hostname would need CiliumNetworkPolicy and a per app allow list the platform has no way to know.
- The blocked CIDR list exists twice: as the config default in Go and as literal text in the build namespace YAML. AC-20 pins them together with a test, but they are still two copies, and a deployment that overrides the config value diverges from the build namespace's fixed list by design.
- Every deploy now writes two more objects, so a deploy makes two more API calls and has two more ways to fail.
- Debugging an app's network from inside gets harder for you too, which is the point, but it is still a cost.

**Neutral**:
- IPv6 is denied outright: the allow rule names `0.0.0.0/0` only, so a dual stack cluster would need `::/0` added deliberately. Denied is the safe direction to be wrong in.
- Cilium allows the kubelet's health probes from the node's host namespace by default, so readiness keeps working under a deny all ingress policy. If host firewall were ever enabled on this cluster, that stops being true, and AC-10 is the criterion that would catch it.
- The `deployer-system` namespace is deliberately left unpoliced by this slice, so any workload on the cluster can still reach the platform API. See Follow-up.

## Follow-up

- [ ] Police the `deployer-system` namespace: ingress from `ingress-nginx` and `deployer-builds` only. Cheap static YAML, deliberately deferred out of this slice.
- [ ] Decide whether the build namespace's blocked CIDR list should be generated from the same source as the Go default rather than restated in YAML and pinned by a test, once there is a third place that needs it.
- [ ] Revisit FQDN based egress (CiliumNetworkPolicy) if an app ever needs a genuine allow list of external hosts rather than all of the internet. Slice 7's app configuration is where a per app allow list would naturally be expressed.
- [ ] Spec 0004's AC-10 (a root image is refused) is still deferred to slice 6 and is unaffected by this slice.

## Migration plan

**Strategy**: single deployment plus a startup sweep, with the seven existing app namespaces picked up automatically.

**Phases**:
1. Ship the control plane change. On boot it sweeps the existing app namespaces and applies both policies; every subsequent deploy applies them as part of reconcile.
2. Ship the build namespace YAML through ArgoCD in the same change, and run one real build to confirm it.

**Rollback**: revert the commit and delete the policy objects (`kubectl delete netpol -l app.kubernetes.io/managed-by=deployer -A`). Nothing is written to the database and no manifest an app depends on changes, so a revert restores the previous behaviour completely.

**Risks**: the seven running apps have never run under a fence, so any of them that quietly depended on cluster reach will start failing at the moment of the sweep, and the failure appears as an application error rather than a platform one. The probe app in build step 4 exists to characterise that before the sweep lands rather than after.
