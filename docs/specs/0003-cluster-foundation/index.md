# 0003. Cluster foundation: namespaces, RBAC, tailnet ingress, wildcard DNS and TLS

**Date**: 2026-08-11
**Status**: In Progress

## Summary

This is the ground the platform stands on inside your k3s cluster. The control plane runs in its own namespace as a single pod with a service account whose rights are listed explicitly, one line per Kubernetes kind it actually creates. Every deployed app gets its own namespace stamped with restricted pod security and a resource ceiling, so a bad app is stopped by the cluster itself rather than by platform code being careful.

Traffic reaches an app one way only. A single Tailscale device sits in front of the existing ingress-nginx controller, so a deployed app is reachable to members of your tailnet and to nobody else. cert-manager gets one wildcard certificate for `*.<DEPLOYER_APP_DOMAIN>` from Let's Encrypt by proving control of the domain through Cloudflare DNS, and ingress-nginx serves that one certificate as its default. An app therefore never carries a certificate, and no app namespace ever holds a private key.

Nothing here builds an image. This feature is proved by hand applying a hello world container in the exact shape the control plane will later generate, and reaching it over HTTPS from a tailnet device.

Reasoning and the options weighed: see [rationale.md](rationale.md). Verify steps: see [verify.md](verify.md).

## Requirements

**User stories**:

- As the operator, I want the control plane to hold the smallest set of cluster rights that still lets it deploy apps, so a bug or a stolen token cannot reach the rest of my cluster.
- As the operator, I want every deployed app confined by the cluster itself, so an app an AI wrote cannot run as root, escape its namespace, or eat a node.
- As an AI agent, I want a deployed app to come back as a working HTTPS URL, so I can hand the person a link that opens without a certificate warning.
- As the operator, I want deployed apps reachable only to my tailnet, so nothing an agent generates is ever exposed to the internet.

**Acceptance criteria**:

- **AC-1**: `deployer-system` exists, carries pod security `enforce: restricted`, and is the only namespace the platform's ArgoCD Application manages.
- **AC-2**: The control plane runs as exactly one replica with strategy `Recreate`, non root, read only root filesystem, all Linux capabilities dropped, seccomp `RuntimeDefault`, and the 10Gi Longhorn volume mounted at `/data` and writable by its non root user.
- **AC-3**: The control plane's ServiceAccount is bound to exactly one ClusterRole holding the resource and verb set in `## Feature design`, and holds no rights on nodes, RBAC objects, CustomResourceDefinitions, or anything in `kube-system`.
- **AC-4**: The control plane reports ready only after the database is open and every migration has applied. A readiness failure removes it from its Service; a liveness failure restarts it.
- **AC-5**: An app namespace is named `app-<slug>`, carries the ownership labels, `enforce: restricted` pod security, a ResourceQuota of 1 CPU, 1Gi memory, and 5 pods, and a LimitRange supplying default requests and limits.
- **AC-6**: A pod requesting privileged mode, host networking, a host path mount, or a root user is rejected by the API server in an app namespace, with no platform side check involved. A pod carrying no security context at all is rejected too, so the platform must compose one on every app pod.
- **AC-7**: A container that declares no resources at all is admitted and gets the LimitRange defaults, and a workload asking for more than the namespace quota is refused at admission with a quota error.
- **AC-8**: cert-manager holds a valid Let's Encrypt certificate for `*.<DEPLOYER_APP_DOMAIN>`, obtained through a Cloudflare DNS-01 challenge, and renews it without anyone touching it.
- **AC-9**: ingress-nginx serves that certificate as its default for any host with no more specific certificate, so an app Ingress carries no `tls` block and no app namespace holds a private key.
- **AC-10**: `*.<DEPLOYER_APP_DOMAIN>` resolves to the tailnet address of the Tailscale device fronting ingress-nginx.
- **AC-11**: From a tailnet device, a hand applied hello world at `https://<slug>.<DEPLOYER_APP_DOMAIN>` returns HTTP 200 with a publicly trusted certificate and no browser warning.
- **AC-12**: From a device that is not on the tailnet, that same hostname resolves but the TCP connection never establishes.
- **AC-13**: The control plane's MCP and upload endpoints are reachable at a tailnet name of their own, distinct from the app wildcard, and no app hostname can route to a platform path.
- **AC-14**: The Cloudflare API token exists in the cluster only as a SealedSecret, is scoped to DNS edit on the one zone, and never appears in plain text in any repository.
- **AC-15**: The platform manifests are delivered by an ArgoCD Application whose scope cannot prune a runtime created `app-<slug>` namespace.
- **AC-16**: Every setting this feature adds is validated in `internal/config` at startup, and a missing or malformed one fails the boot with an error naming it.

## Decision

**Chosen option**: Option 1: One Tailscale device fronting the existing ingress-nginx, with a shared wildcard certificate served as the controller default.

App traffic enters the tailnet at a single Tailscale device, passes to the ingress-nginx controller your cluster already runs, and is matched to a per app Ingress object by hostname. TLS terminates at nginx using one wildcard certificate that cert-manager renews through Cloudflare DNS-01. The control plane holds one narrow ClusterRole, creates each app's namespace with restricted pod security and a resource quota, and is itself delivered by an ArgoCD Application scoped so it can never prune what the control plane creates at runtime.

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
| core | `services`, `secrets`, `serviceaccounts`, `configmaps`, `resourcequotas` | get, list, watch, create, update, patch, delete |
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
tailnet device
  → *.<DEPLOYER_APP_DOMAIN> resolves to 100.x.y.z
  → Tailscale device fronting ingress-nginx (transport hop only, no TLS termination)
  → ingress-nginx :443, terminates with the wildcard certificate
  → Ingress rule matched by host
  → app Service → app pod
```

The fronting device is specifically a Service level exposure, not an Ingress. The Tailscale operator is pointed at the existing ingress-nginx controller Service (`tailscale.com/expose` plus `tailscale.com/hostname` annotations, or `spec.loadBalancerClass: tailscale`), so it forwards TCP and nginx still terminates TLS with the wildcard. An Ingress on the `tailscale` class would terminate TLS at Tailscale with a `ts.net` certificate and break the whole certificate design, so it must not be used here.

The control plane sits on a separate path, and there the `tailscale` ingress class is exactly right: its own Ingress on that class gives it a `ts.net` name with a Tailscale issued certificate. Platform and apps never share a hostname or an ingress rule.

**Certificate and DNS**:

| Piece | Where it lives | Owned by |
|---|---|---|
| Cloudflare API token, DNS edit on one zone | `SealedSecret` in cert-manager's cluster resource namespace, which is an install time flag and must be read rather than assumed to be `cert-manager` | `k3sprox-gitops` |
| `ClusterIssuer` letsencrypt-staging and letsencrypt-prod, DNS-01 via Cloudflare | Cluster scoped | `k3sprox-gitops` |
| `Certificate` for `*.<DEPLOYER_APP_DOMAIN>`, secret in the ingress-nginx namespace | ingress-nginx namespace | `k3sprox-gitops` |
| ingress-nginx `--default-ssl-certificate` pointed at that secret | Controller flag or Helm value | `k3sprox-gitops` |
| Wildcard A record to the fronting device's tailnet address | Cloudflare zone | Set by hand, recorded in `verify.md` |

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
| Resolve hostname | Tailnet address of the fronting device | The Tailscale operator assigns it; a human writes the wildcard A record once. Not read by any code |
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

- Reachability is the outer boundary: an app is reachable only from the tailnet, because the only address in DNS is a `100.64.0.0/10` tailnet address that nothing off the tailnet can route to. There is no LAN path and no public path.
- The DNS record is public and the target is private. Anyone can learn the tailnet address; nobody off the tailnet can use it. Individual app names are never published, because the record is a wildcard and the certificate is a wildcard, so no per app name reaches Certificate Transparency logs either.
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

1. Expose the existing ingress-nginx controller Service through the Tailscale operator, note the tailnet address it is given, and set the wildcard A record for `*.<DEPLOYER_APP_DOMAIN>`. Prove plain HTTP reaches a throwaway pod from a tailnet device, satisfies **AC-10**.
2. Read cert-manager's cluster resource namespace from its running deployment, then add the Cloudflare token there as a SealedSecret and the `letsencrypt-staging` ClusterIssuer with a DNS-01 solver, both in `k3sprox-gitops`, satisfies **AC-14**.
3. Request the wildcard `Certificate` against staging, then point `--default-ssl-certificate` at its secret. This restarts the shared ingress-nginx controller and briefly interrupts TLS for your existing twelve apps, so do it deliberately rather than as a side effect. Confirm nginx serves the certificate for a host with no Ingress, satisfies **AC-9**.
4. Add the `letsencrypt-prod` ClusterIssuer and switch the `Certificate` to it once staging has succeeded, satisfies **AC-8**.
5. Hand apply the hello world namespace, Deployment, Service, and Ingress in the exact shape the control plane will generate, including the required app pod security context, and reach it over HTTPS from a tailnet device. Confirm from off tailnet that it does not connect, satisfies **AC-11**, **AC-12**.
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
- One Tailscale device rather than one per app means app create and delete stay pure Kubernetes operations, with no second system to keep in step and no device sprawl in your tailnet.
- The ClusterRole is short enough to read in full during a review, and every row maps to a Kubernetes kind the platform actually composes.

**Negative / tradeoffs**:

- Deployed apps share a controller with the twelve apps your cluster already serves, and the default certificate flag is a controller wide setting. See the premise note in [rationale.md](rationale.md); this is the sharpest edge in the design.
- Reachability is all or nothing. Every tailnet member can reach every deployed app, because the boundary is the tailnet itself. Per app access control would need per app Tailscale devices and tailnet ACLs, which is a different design.
- The control plane can create and delete any namespace in the cluster. The verbs are narrow and the label convention makes intent clear, but nothing at the API server level stops it deleting a namespace it did not create. Tightening that needs an admission policy, which is real work for a homelab threat model.
- TLS is terminated at nginx, so the Tailscale device is a plain transport hop and the whole path depends on that one device staying healthy. It is a single point of failure for every deployed app.
- The wildcard certificate expiring or failing to renew breaks every app at once rather than one at a time.
- Four repositories or systems now hold a piece of this: this repo, `k3sprox-gitops`, the Cloudflare zone, and the Tailscale admin console. Nothing about that is discoverable from the code alone, which is why `verify.md` exists.

**Neutral**:

- The `jobs` and `pods/log` rows in the ClusterRole are unused until slices 1 and 3. They are granted now so RBAC does not have to be reopened mid slice.
- The quota values are configuration, not constants, so raising a ceiling is an environment change rather than a code change.
- `deployer-builds` is left undefined by design. Its pod security level depends on what the Buildpacks lifecycle and rootless BuildKit actually need, which slice 1 will discover.

## Follow-up

- [ ] Decide `deployer-builds` pod security in slice 1. Rootless BuildKit in particular may not fit `restricted`, and finding that out mid build is worse than deciding it with the build path in front of you.
- [ ] Slice 1 owes the caller a clear error when an image would run as root, because restricted pod security will refuse it at admission and the raw API server message is not something to hand an agent.
- [ ] Feature 13 owns app delete, but the namespace delete contract starts here. Decide there whether delete is synchronous, and what happens to a namespace stuck `Terminating` on a finalizer.
- [ ] Consider a Kyverno or Validating Admission Policy rule that lets the control plane delete only namespaces carrying its own `managed-by` label, closing the one broad right in the ClusterRole.
- [ ] Confirm the Tailscale device fronting nginx keeps a stable tailnet address across an operator upgrade. If it does not, the wildcard record becomes a CNAME to its MagicDNS name instead.
- [ ] The control plane's tailnet name is currently the only thing standing between any tailnet member and the platform API. Feature 8 closes this; until it lands, do not treat the platform API as authenticated.
