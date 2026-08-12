# 0003. Cluster foundation: namespaces, RBAC, tailnet ingress, wildcard DNS and TLS

**Date**: 2026-08-11
**Status**: Accepted

## Summary

This is the ground the platform stands on inside your k3s cluster. The control plane runs in its own namespace as a single pod with a service account whose rights are listed explicitly, one line per Kubernetes kind it actually creates. Every deployed app gets its own namespace stamped with restricted pod security and a resource ceiling, so a bad app is stopped by the cluster itself rather than by platform code being careful.

Traffic reaches an app one way only. The pfSense box that already routes a subnet onto your tailnet advertises one more host route, `172.16.70.40/32`, the address the existing ingress-nginx controller already answers on. A deployed app is therefore reachable from your tailnet and from your LAN, and from nowhere else. cert-manager gets one wildcard certificate for `*.<DEPLOYER_APP_DOMAIN>` from Let's Encrypt by proving control of the domain through Cloudflare DNS, and ingress-nginx serves that one certificate as its default. An app therefore never carries a certificate, and no app namespace ever holds a private key.

Nothing here builds an image. This feature is proved by hand applying a hello world container in the exact shape the control plane will later generate, and reaching it over HTTPS from a tailnet device.

Reasoning and the options weighed: see [rationale.md](rationale.md). Verify steps: see [verify.md](verify.md).

## Requirements

**User stories**:

- As the operator, I want the control plane to hold the smallest set of cluster rights that still lets it deploy apps, so a bug or a stolen token cannot reach the rest of my cluster.
- As the operator, I want every deployed app confined by the cluster itself, so an app an AI wrote cannot run as root, escape its namespace, or eat a node.
- As an AI agent, I want a deployed app to come back as a working HTTPS URL, so I can hand the person a link that opens without a certificate warning.
- As the operator, I want deployed apps reachable only from my tailnet or my own LAN, so nothing an agent generates is ever exposed to the internet.

**Acceptance criteria**:

- **AC-1**: `deployer-system` exists, carries pod security `enforce: restricted`, and is the only namespace the platform's ArgoCD Application manages.
- **AC-2**: The control plane runs as exactly one replica with strategy `Recreate`, non root, read only root filesystem, all Linux capabilities dropped, seccomp `RuntimeDefault`, and the 10Gi Longhorn volume mounted at `/data` and writable by its non root user.
- **AC-3**: The control plane's ServiceAccount is bound to exactly one ClusterRole holding the resource and verb set in `## Feature design`, and holds no rights on nodes, RBAC objects, CustomResourceDefinitions, or anything in `kube-system`.
- **AC-4**: The control plane reports ready only after the database is open and every migration has applied. A readiness failure removes it from its Service; a liveness failure restarts it.
- **AC-5**: An app namespace is named `app-<slug>`, carries the ownership labels, `enforce: restricted` pod security, a ResourceQuota of 1 CPU, 1Gi memory, and 5 pods, and a LimitRange supplying default requests and limits.
- **AC-6**: A pod requesting privileged mode, host networking, a host path mount, or a root user is rejected by the API server in an app namespace, with no platform side check involved. A pod carrying no security context at all is rejected too, so the platform must compose one on every app pod.
- **AC-7**: A container that declares no resources at all is admitted and gets the LimitRange defaults, and a workload asking for more than the namespace quota is refused at admission with a quota error.
- **AC-8**: cert-manager holds a valid Let's Encrypt certificate named `wildcard-apps`, covering both `*.<DEPLOYER_APP_DOMAIN>` and the bare `<DEPLOYER_APP_DOMAIN>`, obtained through a Cloudflare DNS-01 challenge, and renews it without anyone touching it. Both names are required: a wildcard certificate does not cover the bare domain, so dropping the second name would leave the apex serving a certificate for the wrong host.
- **AC-9**: ingress-nginx serves that certificate as its default for any host with no more specific certificate, so an app Ingress carries no `tls` block and no app namespace holds a private key.
- **AC-10**: `*.<DEPLOYER_APP_DOMAIN>` resolves to `172.16.70.40`, the address the ingress-nginx controller already answers on, and that address is advertised onto the tailnet as a `/32` host route by the existing pfSense subnet router.
- **AC-11**: From a tailnet device that is not on the LAN, a hand applied hello world at `https://<slug>.<DEPLOYER_APP_DOMAIN>` returns HTTP 200 with a publicly trusted certificate and no browser warning.
- **AC-12**: From a device that is on neither the tailnet nor the LAN, that same hostname resolves but the TCP connection never establishes. Reachability from your own LAN is intended, and matches how the twelve applications already behind this controller work.
- **AC-13**: The control plane's MCP and upload endpoints are reachable at a tailnet name of their own, distinct from the app wildcard, and no app hostname can route to a platform path.
- **AC-14**: The Cloudflare API token exists in the cluster only as a SealedSecret, is scoped to DNS edit on the one zone, and never appears in plain text in any repository.
- **AC-15**: The platform manifests are delivered by an ArgoCD Application whose scope cannot prune a runtime created `app-<slug>` namespace.
- **AC-16**: Every setting this feature adds is validated in `internal/config` at startup, and a missing or malformed one fails the boot with an error naming it.

## Decision

**Chosen option**: Option 4: A `/32` host route to the existing ingress-nginx address, advertised by the pfSense subnet router already on the tailnet, with a shared wildcard certificate served as the controller default.

App traffic enters the tailnet through the pfSense subnet router that already serves `172.16.60.0/24`, which advertises one additional host route for `172.16.70.40/32`. It reaches the ingress-nginx controller your cluster already runs and is matched to a per app Ingress object by hostname. TLS terminates at nginx using one wildcard certificate that cert-manager renews through Cloudflare DNS-01.

This replaces the original choice, a Tailscale operator exposure of the controller Service, which was built on 2026-08-11 and does not work on this cluster. See [rationale.md](rationale.md) for what failed and why. The control plane holds one narrow ClusterRole, creates each app's namespace with restricted pod security and a resource quota, and is itself delivered by an ArgoCD Application scoped so it can never prune what the control plane creates at runtime.

**Implementation skills**: `senior-kubernetes-engineer` (`.claude/skills/senior-kubernetes-engineer/`) · `security-patterns` (`.claude/skills/security-patterns/`) · `golang-patterns` (`.claude/skills/golang-patterns/`)

## Rationale

Reasoning, the options weighed, and the premise note: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**: none. This feature adds no tables, columns, or migrations. It reads two existing columns from spec 0002, `apps.id` and `apps.slug`, and the objects it creates are Kubernetes objects, laid out below.

**Namespace layout**:

| Namespace | Created by | Contents | Pod security |
|---|---|---|---|
| `deployer-system` | ArgoCD, from `deploy/` | Control plane pod, its Service, its PVC, its ServiceAccount | `enforce: restricted` |
| `app-<slug>` | The control plane, at deploy time | One app: Deployment, Service, Ingress, later NetworkPolicy and pull secret | `enforce: restricted` |
| `deployer-builds` | Slice 1, not this feature | Short lived build Jobs | Decided by slice 1, see Follow-up |

Every namespace the platform creates carries these labels, and every later sweep, delete, or audit selects on them rather than on the name:

```
app.kubernetes.io/managed-by: deployer
deployer.internal/app-id:     <the apps.id value>
deployer.internal/app-slug:   <the apps.slug value>
```

App namespaces additionally carry the pod security admission labels:

```
pod-security.kubernetes.io/enforce:         restricted
pod-security.kubernetes.io/enforce-version: latest
pod-security.kubernetes.io/warn:            restricted
pod-security.kubernetes.io/audit:           restricted
```

**Per app ResourceQuota** (in `app-<slug>`, values from configuration):

| Item | Limit | What consumes it |
|---|---|---|
| `requests.cpu` / `limits.cpu` | 1 | The app's containers |
| `requests.memory` / `limits.memory` | 1Gi | The app's containers |
| `pods` | 5 | 1 running, plus headroom for a rolling replacement and a failed one |
| `services` | 3 | 1 for the app; the rest is headroom |
| `secrets` | 10 | The `imagePullSecret` (slice 1), the app's environment secret (slice 7), and headroom |

**Per app LimitRange** (in `app-<slug>`, required, not optional): a ResourceQuota that constrains `limits.cpu` and `limits.memory` makes the API server reject any pod that declares neither requests nor limits, which is most container images. The LimitRange supplies the defaults that make such a pod admissible:

| Field | Value |
|---|---|
| `default.cpu` / `default.memory` | 500m / 512Mi |
| `defaultRequest.cpu` / `defaultRequest.memory` | 100m / 128Mi |
| `max.cpu` / `max.memory` | 1 / 1Gi |

**Required app pod security context**: restricted pod security does not merely reject a pod that asks for root, it rejects a pod that says nothing. The platform therefore composes this on every app pod it creates, and this spec is where the contract lives so slice 1 has something to build against:

```
securityContext:                      # pod level
  runAsNonRoot: true
  seccompProfile: { type: RuntimeDefault }
containers[].securityContext:
  allowPrivilegeEscalation: false
  capabilities: { drop: [ALL] }
```

The platform does not pin `runAsUser`, so an image with its own non root `USER` keeps it. An image that has no `USER` line and would run as root fails admission, which is the correct outcome and is a build path error message slice 1 owes the caller.

**Control plane ClusterRole** (the whole set; anything not listed is denied):

| API group | Resources | Verbs |
|---|---|---|
| core | `namespaces` | get, list, watch, create, delete |
| core | `services`, `secrets`, `serviceaccounts`, `configmaps`, `resourcequotas`, `limitranges` | get, list, watch, create, update, patch, delete |
| core | `pods` | get, list, watch, delete |
| core | `pods/log` | get |
| core | `events` | get, list, watch |
| apps | `deployments`, `replicasets` | get, list, watch, create, update, patch, delete |
| batch | `jobs` | get, list, watch, create, delete |
| networking.k8s.io | `ingresses`, `networkpolicies` | get, list, watch, create, update, patch, delete |

Not granted, deliberately: `nodes`, `persistentvolumes`, `clusterroles`, `clusterrolebindings`, `roles`, `rolebindings`, `customresourcedefinitions`, `apiservices`, and every `escalate`, `bind`, `impersonate`, or `*` verb. The `jobs` and `pods/log` rows exist now so slice 1 and slice 3 do not have to reopen RBAC.

**Control plane pod shape**:

- `replicas: 1`, `strategy: Recreate` (spec 0001 invariant: two writers on one SQLite file is corruption).
- `securityContext`: `runAsNonRoot: true`, `runAsUser: 65532`, `fsGroup: 65532`, `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`. The `fsGroup` is load bearing, not decoration: Longhorn presents the volume root owned, so without it the non root process cannot create the SQLite file and the pod never becomes ready.
- Volumes: the Longhorn PVC at `/data`, plus an `emptyDir` at `/tmp` because the root filesystem is read only.
- Probes: `GET /healthz` liveness (the process is alive), `GET /readyz` readiness (the database is open and migrated). Readiness is what gates traffic; liveness never depends on the database, so a locked database restarts nothing.
- Resources: requests 100m CPU and 128Mi memory, limits 1 CPU and 512Mi memory.
- `DEPLOYER_POD_NAME` and `DEPLOYER_NAMESPACE` come from the downward API.

**Request path**:

```
tailnet device (or a device on the LAN, which skips the first two hops)
  → *.<DEPLOYER_APP_DOMAIN> resolves to 172.16.70.40
  → pfSense subnet router, advertising 172.16.70.40/32 onto the tailnet
  → the LAN, to the MetalLB address ingress-nginx already holds
  → ingress-nginx :443, terminates with the wildcard certificate
  → Ingress rule matched by host
  → app Service → app pod
```

The entry point is a subnet route on a box that is not part of the cluster, which is the whole point: no packet on this path is ever forwarded through a Kubernetes pod, so nothing about it depends on how Cilium translates service addresses. Nothing in the cluster is added, annotated, or restarted to make an app reachable.

Two mechanisms are deliberately not used here. A Tailscale operator exposure of the controller Service does not work on this cluster, proven on 2026-08-11, see [rationale.md](rationale.md). An Ingress on the `tailscale` class would terminate TLS at Tailscale with a `ts.net` certificate and break the whole certificate design.

The tailnet ACL must carry one grant for this route, and nothing wider:

```json
{ "action": "accept", "src": ["group:prod"], "dst": ["172.16.70.40/32:443"] }
```

A single host on a single port, matching the style of the existing `172.16.42.0/24:443` rule. Port 80 is deliberately not granted, so a typed `http://` URL times out rather than redirecting. Platform issued URLs are always `https`.

The ACL is not the only gate on that hop. pfSense has its own firewall, separate from the tailnet policy, and a route being advertised and approved only carries a packet as far as the pfSense interface. A pass rule on the `tailscale0` interface to `172.16.70.40:443` is required as well, alongside the rule already there for the camera VLAN. Forgetting it produces a connection timeout, which looks exactly like AC-12 passing, so it is called out here and in `verify.md` rather than left to be discovered.

The control plane sits on a separate path, and there the `tailscale` ingress class is exactly right: its own Ingress on that class gives it a `ts.net` name with a Tailscale issued certificate. Platform and apps never share a hostname or an ingress rule.

That is a different mechanism from the one that failed, and the difference is worth stating plainly, because the two look alike. A `tailscale` class Ingress terminates TLS inside tailscaled and then opens its **own** connection to the backend Service. That is a connection the proxy originates, which Cilium translates correctly, and it is proven on this cluster four times over: `argocd`, `n8n`, `longhorn` and `provic` all run on it today. The mechanism that failed forwarded raw packets through the pod to a ClusterIP, which Cilium does not translate. Same operator, opposite behaviour.

**Certificate and DNS**:

| Piece | Where it lives | Owned by |
|---|---|---|
| Cloudflare API token, DNS edit on one zone | `SealedSecret` in cert-manager's cluster resource namespace, which is an install time flag and must be read rather than assumed to be `cert-manager` | `k3sprox-gitops` |
| `ClusterIssuer` letsencrypt-staging and letsencrypt-prod, DNS-01 via Cloudflare | Cluster scoped | `k3sprox-gitops` |
| `Certificate` named `wildcard-apps`, covering `*.<DEPLOYER_APP_DOMAIN>` and the bare domain, secret `wildcard-apps-tls` | ingress-nginx namespace | `k3sprox-gitops` |
| ingress-nginx `--default-ssl-certificate` pointed at that secret | Controller flag or Helm value | `k3sprox-gitops` |
| Wildcard A record, plus an apex record for the bare domain, both to `172.16.70.40`, DNS only, never proxied | Cloudflare zone | Set by hand, recorded in `verify.md` |
| The `172.16.70.40/32` route advertisement, its approval, the pfSense firewall rule on `tailscale0`, and the tailnet ACL grant | pfSense and the tailnet policy file | Set by hand, recorded in `verify.md` |

**Per app Ingress shape** (what the control plane composes in slice 1, fixed here):

```
ingressClassName: <DEPLOYER_INGRESS_CLASS_NAME>
rules:
  - host: <slug>.<DEPLOYER_APP_DOMAIN>
    http: { paths: [ { path: /, pathType: Prefix, backend: <app service>:80 } ] }
# no tls block: the controller default certificate covers this host
```

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| Create app namespace | Namespace name | `"app-" + apps.slug` (column, spec 0002) |
| Create app namespace | `deployer.internal/app-id` label | `apps.id` column |
| Create app namespace | Quota CPU, memory, pod count | `DEPLOYER_APP_QUOTA_CPU`, `_MEMORY`, `_PODS` config |
| Create app namespace | LimitRange defaults and maxima | Fixed in this spec, derived from the quota values |
| Create app workload | App pod security context | Fixed in this spec, composed by the platform, never from user input |
| Create app workload | App container user | The image's own `USER`. The platform sets `runAsNonRoot` but never pins a UID, so an image with no `USER` fails admission by design |
| Create app Ingress | Hostname | `apps.slug` column plus `DEPLOYER_APP_DOMAIN` config |
| Create app Ingress | Ingress class name | `DEPLOYER_INGRESS_CLASS_NAME` config |
| Create app Ingress | TLS secret | None. The controller default certificate covers it, so the platform never names one |
| Serve HTTPS | Certificate for `<slug>.<domain>` | The single wildcard `Certificate`, cert-manager renewed |
| Resolve hostname | `172.16.70.40`, the ingress-nginx MetalLB address | Already allocated from the `k3sprox-pool` and stable; a human writes the wildcard A record once. Not read by any code |
| Reach the platform API | The control plane's `ts.net` name | The `tls.hosts` entry on its own Ingress in `deploy/ingress.yaml`, currently `deployer`, which the Tailscale operator turns into `deployer.<tailnet>.ts.net`. A fixed manifest value, never configuration and never read by any code |
| Record a claim | Claiming pod name | `DEPLOYER_POD_NAME`, downward API |
| Pull an app image | `imagePullSecret` | Not this feature. Slice 1 mints it, spec 0001 |

**Key invariants**:

- A slug never contains a dot. `*.<domain>` matches exactly one DNS label, so a dotted slug would silently fall off the certificate. `domain.DeriveSlug` already guarantees this by collapsing every non alphanumeric run to a dash; this spec is why that must not change.
- The wildcard private key exists in exactly one namespace, the ingress-nginx one. It is never copied, mirrored, or referenced from an app namespace.
- ArgoCD's scope never includes a runtime created namespace, so a sync can never delete a running app.
- The control plane creates namespaces but holds no rights to create Roles, RoleBindings, or ClusterRoles, so it cannot widen its own access.
- Restricted pod security is enforced by the API server, not by platform code. Slice 5 hardens the manifests the platform writes; this layer holds even if slice 5 has a bug.
- An app image with no non root `USER` cannot be deployed at all. That is the design working, not a defect, and the error belongs to the build path rather than to the caller as a raw admission message.
- A ResourceQuota constraining limits makes a LimitRange mandatory, not optional. The two ship together or every ordinary image is refused.
- The control plane's readiness depends on the database; its liveness does not.

**Security model**:

- Reachability is the outer boundary: an app is reachable from the tailnet and from the LAN, because the only address in DNS is `172.16.70.40`, an RFC 1918 address nothing on the internet can route to. There is no public path. The LAN path is intended and matches the scope's settled decision that apps are reachable on your LAN or VPN only.
- The tailnet half of that boundary is two locks, not one: a device must both accept the advertised route and be allowed by the ACL grant, which is a single host on a single port.
- The DNS record is public and the target is private. Anyone can learn that the name points at `172.16.70.40`; nobody outside your LAN or tailnet can use it. Individual app names are never published, because the record is a wildcard and the certificate is a wildcard, so no per app name reaches Certificate Transparency logs either.
- Authentication and authorization of MCP callers are not this feature. Feature 8 owns them. Until then the control plane's own tailnet name is the only access control on the platform API, and that is stated so nobody mistakes it for finished.
- The Cloudflare token is the one credential this feature introduces. It is scoped to DNS edit on a single zone, sealed, and readable only by cert-manager.
- No regulated data class is in scope, so no compliance regime applies.

**Configuration required** (all validated at startup by `internal/config`, AC-16):

- `DEPLOYER_NAMESPACE`: the control plane's own namespace, from the downward API. Required.
- `DEPLOYER_INGRESS_CLASS_NAME`: ingress class for app Ingress objects. Default `nginx`.
- `DEPLOYER_APP_QUOTA_CPU`: per app CPU ceiling. Default `1`.
- `DEPLOYER_APP_QUOTA_MEMORY`: per app memory ceiling. Default `1Gi`.
- `DEPLOYER_APP_QUOTA_PODS`: per app pod ceiling. Default `5`.

Existing settings this feature finally gives a real value to: `DEPLOYER_APP_DOMAIN`, the wildcard domain, and `DEPLOYER_DB_PATH` plus `DEPLOYER_UPLOAD_DIR`, both under the `/data` mount.

**Critical test scenarios**:

- Happy path: a hand applied hello world in `app-hello-<suffix>` answers HTTPS 200 on its hostname from a tailnet device, with a trusted certificate, verifies **AC-11**.
- Network boundary: the same hostname from off the tailnet resolves and then times out, verifies **AC-12**.
- Admission: a pod manifest with `privileged: true`, one with `runAsUser: 0`, and one with no `securityContext` at all are each rejected by the API server at apply time, verifies **AC-6**.
- Defaulting: a Deployment whose container declares no `resources` block is admitted and its pod comes up carrying the LimitRange defaults, verifies **AC-7**.
- Quota: a Deployment asking for 2 CPU in an app namespace is refused with a quota error, verifies **AC-7**.
- RBAC: the control plane's token can create a namespace and a Deployment, and is refused on `get nodes`, `create clusterrolebinding`, and `list secrets -n kube-system`, verifies **AC-3**.
- Certificate: the wildcard `Certificate` reports Ready with a Let's Encrypt production issuer, and nginx serves it for a hostname that has no Ingress at all, verifies **AC-8** and **AC-9**.
- Startup: the control plane with `DEPLOYER_APP_QUOTA_MEMORY=banana` fails to boot with an error naming that variable, verifies **AC-16**.
- Readiness: the control plane with an unwritable `/data` reports not ready and is removed from its Service, without restarting in a loop, verifies **AC-4**.

## Build plan

Tracer Bullet, so the request path is proved end to end before the control plane exists. The thinnest thread here is one HTTPS request from a tailnet device reaching a pod; everything else thickens it.

1. Advertise `172.16.70.40/32` from the pfSense subnet router, approve the route in the Tailscale admin console, add the ACL grant above, and set the wildcard A record for `*.<DEPLOYER_APP_DOMAIN>` to `172.16.70.40`, DNS only. Prove plain HTTP reaches nginx from a tailnet device that is off the LAN, satisfies **AC-10**.
2. Read cert-manager's cluster resource namespace from its running deployment, then add the Cloudflare token there as a SealedSecret and the `letsencrypt-staging` ClusterIssuer with a DNS-01 solver, both in `k3sprox-gitops`, satisfies **AC-14**.
3. Request the wildcard `Certificate` against staging, then point `--default-ssl-certificate` at its secret. This restarts the shared ingress-nginx controller and briefly interrupts TLS for your existing twelve apps, so do it deliberately rather than as a side effect. Confirm nginx serves the certificate for a host with no Ingress, satisfies **AC-9**.
4. Add the `letsencrypt-prod` ClusterIssuer and switch the `Certificate` to it once staging has succeeded, satisfies **AC-8**.
5. Hand apply the hello world namespace, Deployment, Service, and Ingress in the exact shape the control plane will generate, including the required app pod security context, and reach it over HTTPS from a tailnet device that is off the LAN. Confirm from a device on neither network that it does not connect, satisfies **AC-11**, **AC-12**.
6. Add the app namespace template to `deploy/`: ownership labels, pod security labels, the ResourceQuota, and the LimitRange. Re-apply the hello world into a namespace built from it, confirm a container declaring no resources is admitted with the defaults, and confirm privileged, root, bare, and over quota pods are all refused, satisfies **AC-5**, **AC-6**, **AC-7**.
7. Write `deploy/` for `deployer-system`: the namespace with restricted pod security, the ServiceAccount, the ClusterRole and binding, and the 10Gi Longhorn PVC, satisfies **AC-1**, **AC-3**.
8. Add the new settings to `internal/config` with validation and tests, and add the `/healthz` and `/readyz` handlers, satisfies **AC-4**, **AC-16**.
9. Write the control plane Deployment and Service with the pod shape above, and confirm it starts, migrates, and reports ready in the cluster, satisfies **AC-2**, **AC-4**.
10. Give the control plane its own Ingress on the `tailscale` class and confirm its health endpoint answers on its `ts.net` name, satisfies **AC-13**.
11. Add the ArgoCD `Application` in `k3sprox-gitops` scoped to `deployer-system` only, sync it, and confirm a sync leaves the hello world app namespace untouched, satisfies **AC-15**.
12. Add an alert on the wildcard `Certificate` leaving the Ready condition. This is the one shared failure that takes every app down at once, so it is a build step rather than a note, even though metrics and alerting are otherwise deferred in the scope, satisfies **AC-8**.

## Consequences

**Positive**:

- Isolation stops being a promise the platform code makes and becomes something the API server enforces. Slice 5 hardens the manifests; this layer holds even when slice 5 has a bug.
- Certificates disappear from the deploy path completely. The control plane never requests, copies, waits for, or renews one, so an entire class of deploy failure and latency does not exist.
- Exactly one wildcard issuance means Let's Encrypt rate limits are never a factor no matter how many apps get deployed, and individual app names never appear in a public certificate log.
- One subnet route rather than one device per app means app create and delete stay pure Kubernetes operations, with no second system to keep in step and no device sprawl in your tailnet.
- The entry path runs entirely outside the cluster, so it cannot be broken by a CNI setting, a controller restart, or anything the platform later does to its own networking. It is also the same path your existing twelve apps already take, so it is proven daily rather than novel.
- The ClusterRole is short enough to read in full during a review, and every row maps to a Kubernetes kind the platform actually composes.

**Negative / tradeoffs**:

- Deployed apps share a controller with the twelve apps your cluster already serves, and the default certificate flag is a controller wide setting. See the premise note in [rationale.md](rationale.md); this is the sharpest edge in the design.
- Reachability is all or nothing. Every tailnet member the ACL grant covers, and everyone on your LAN, can reach every deployed app. Per app access control would need per app Tailscale devices and tailnet ACLs, which is a different design.
- Anyone on your home LAN reaches deployed apps with no Tailscale at all, including guests on the same network. That is a real widening compared with a tailnet only boundary, accepted because the scope already settled on LAN or VPN and because the existing twelve apps work the same way.
- The entry point now lives on pfSense, which is a box configured by hand rather than by a file in a repository. Nothing in Git records that the route exists, which is why `verify.md` carries it.
- The control plane can create and delete any namespace in the cluster. The verbs are narrow and the label convention makes intent clear, but nothing at the API server level stops it deleting a namespace it did not create. Tightening that needs an admission policy, which is real work for a homelab threat model.
- The whole path depends on pfSense staying healthy and on the route staying advertised and approved. It is a single point of failure for every deployed app, though a shared one: if pfSense is down, much else is too.
- The wildcard certificate expiring or failing to renew breaks every app at once rather than one at a time.
- Four repositories or systems now hold a piece of this: this repo, `k3sprox-gitops`, the Cloudflare zone, and the Tailscale admin console. Nothing about that is discoverable from the code alone, which is why `verify.md` exists.

**Neutral**:

- The `jobs` and `pods/log` rows in the ClusterRole are unused until slices 1 and 3. They are granted now so RBAC does not have to be reopened mid slice.
- The quota values are configuration, not constants, so raising a ceiling is an environment change rather than a code change.
- `deployer-builds` is left undefined by design. Its pod security level depends on what the Buildpacks lifecycle and rootless BuildKit actually need, which slice 1 will discover.

## Follow-up

- [ ] Alert on the wildcard `Certificate` leaving Ready (build plan step 12), deferred during the build on 2026-08-11. The cluster runs no monitoring stack at all, `prometheusrules` is not even a resource type, so there is nowhere for an alert to go and no decision on where it should. Until this lands, a silent renewal failure takes every deployed app down with no warning. The rest of **AC-8**, the certificate being issued and renewing, is unaffected.
- [ ] Decide `deployer-builds` pod security in slice 1. Rootless BuildKit in particular may not fit `restricted`, and finding that out mid build is worse than deciding it with the build path in front of you.
- [ ] Slice 1 owes the caller a clear error when an image would run as root, because restricted pod security will refuse it at admission and the raw API server message is not something to hand an agent.
- [ ] Feature 13 owns app delete, but the namespace delete contract starts here. Decide there whether delete is synchronous, and what happens to a namespace stuck `Terminating` on a finalizer.
- [ ] Consider a Kyverno or Validating Admission Policy rule that lets the control plane delete only namespaces carrying its own `managed-by` label, closing the one broad right in the ClusterRole.
- [ ] Confirm `172.16.70.40` stays allocated to ingress-nginx across a MetalLB or chart upgrade. It comes from the `k3sprox-pool` and has held for 102 days, but the wildcard record and the route both hardcode it, so a reallocation would break every deployed app at once with no obvious cause.
- [ ] The control plane's tailnet name is currently the only thing standing between any tailnet member and the platform API. Feature 8 closes this; until it lands, do not treat the platform API as authenticated.
