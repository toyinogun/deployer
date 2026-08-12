# Scope: Deployer

Deployer is a small internal platform that lets an AI coding agent (Claude Code, Codex, Gemini and friends) deploy an application it just wrote onto your homelab k3s cluster, over MCP, as long as the caller holds a valid account token. The person never touches kubectl, Docker, or YAML.

**Build approach:** Tracer Bullet (prove the whole pipe works end to end before building any part of it fully).
**Workflow:** GA (after `/develop`: `/check verify`, then `/test`, then a fresh model `/check review`, then `/document`). The project default level of rigor. `/architect` is the recommended first stop for a feature with a real decision, but skippable when you already know the build. Any feature can carry its own tag (e.g. `· Beta`) to do more or less.

_These are recommendations to keep your build orderly, not requirements. Skip anything that does not fit: if you already know how to build a feature, use `/develop` and skip `/architect`. You decide when a feature is `done`._

## Settled for the MVP

Decisions you already made, so no feature reopens them:

- The runtime is your existing k3s cluster, and the control plane runs inside it as a workload.
- Source arrives as a tarball uploaded by the MCP client. There is no Git server, no repo provisioning, no Git credentials anywhere.
- Builds use Buildpacks with no config, and fall back to the project's Dockerfile when one is present.
- Callers authenticate with a platform issued API token. No identity provider, no OAuth in the MVP.
- Apps are reachable on your LAN or VPN only, on a hostname under one wildcard domain.
- Apps are stateless. No databases, no volumes, no object storage in the MVP.
- Isolation is enforced, not advisory: own namespace, non root, dropped capabilities, resource ceilings, and network policy that stops apps reaching each other or your cluster services.

## What the cluster already gives you

Verified live on 2026-08-11, so no feature needs to build these. Feature 4 should use them rather than install its own.

| Need | Already running |
|---|---|
| Cluster | k3s v1.35.5+k3s1, 4 nodes (`k3sprox-cp-0` plus 3 workers), 172.16.70.20 to .23 |
| Networking and policy | Cilium 1.16.5, so NetworkPolicy is genuinely enforced |
| Ingress | ingress-nginx 1.11.3 on 172.16.70.40, plus a `tailscale` ingress class |
| Certificates | cert-manager v1.16.2 |
| Load balancer IPs | MetalLB 0.14.9; 172.16.70.41 and .42 are free again |
| Storage | Longhorn 1.11.1, `longhorn` is the default StorageClass |
| GitOps | ArgoCD v3.3.6, root app from `k3sprox-gitops`, 12 apps healthy |
| Secrets | sealed-secrets 0.36.6 |
| Access | Tailscale operator; you reach the cluster as context `k3sprox-operator.tail62ceef.ts.net` |

Deployer needs to add, on top of this: an image registry, a builder, the control plane and its database, app routing under a wildcard hostname, and the MCP server.

## At a glance

| # | Feature | Phase | Status |
|---|---------|-------|--------|
| 1 | Stack & architecture | Foundation | done |
| 2 | Coding standards & tooling | Foundation | done |
| 3 | Platform data model | Foundation | done |
| 4 | Cluster foundation: namespaces, ingress, wildcard DNS & TLS | Foundation | done |
| 5 | First deploy end to end | Slice 1 | done |
| 6 | Async deployment jobs & status | Slice 2 | in-progress |
| 7 | Application logs | Slice 3 | in-progress |
| 8 | Accounts, API tokens & app ownership | Slice 4 | planned |
| 9 | Workload isolation & network policy | Slice 5 | planned |
| 10 | Dockerfile build path | Slice 6 | planned |
| 11 | App environment configuration | Slice 7 | planned |
| 12 | Rollback & release history | Slice 8 | planned |
| 13 | App lifecycle: list & decommission | Slice 9 | planned |

## Foundations

### 1. Stack & architecture · done
Decide the language, framework, database, build tooling, and how the control plane, build worker, and deploy worker are split, then scaffold a runnable project. Every later slice sits on this.
**Done when:** the stack and the plane split are recorded in a spec, and the empty scaffold boots locally and passes build.
spec [0001](../specs/0001-stack-and-architecture/index.md) · code in `cmd/deployer`, `internal/config`
- [x] Decide the stack (spec): `/architect stack & architecture`
- [x] Scaffold from the decision: `/develop stack & architecture`
- [ ] Verify it: `/check verify stack & architecture` — skipped
- [ ] Test it: `/test stack & architecture` — skipped
- [ ] Review it (fresh model): `/check review stack & architecture` — skipped
- [ ] Document it: `/document stack & architecture` — skipped

### 2. Coding standards & tooling · done
Capture the conventions from the real scaffolded project, then install lint, format, type checking, and pre-commit enforcement.
**Done when:** root `AGENTS.md` reflects the real stack, and lint, format, and type checks run clean.
- [x] Capture conventions + tooling choices: `/audit`
- [x] Install the tooling: `/develop tooling` — `.golangci.yml`, `.githooks/pre-commit`, `.github/workflows/ci.yml`

### 3. Platform data model · done
The entities the whole platform turns on: accounts, API tokens, apps, deployments, deployment events, releases. Getting this wrong is the most expensive thing to redo, so it is decided once, up front, before any slice writes to it.
**Done when:** the schema supports the deployment state machine, release history for rollback, and per app ownership, and it migrates cleanly on a fresh database.
spec [0002](../specs/0002-platform-data-model/index.md) · code in `internal/store`, `internal/domain`, `internal/ids`
- [x] Design it (spec): `/architect platform data model`
- [x] Build it: `/develop platform data model`
  - [x] Ids, the one migration, and a booting migrated database — AC-1, AC-2, AC-3, AC-4
  - [x] Deployment lifecycle: transitions, events, create with supersession, the claim — AC-5, AC-6, AC-7, AC-8, AC-16
  - [x] Apps, releases, rollback, and the store interfaces — AC-9, AC-10, AC-11, AC-12
  - [x] Accounts, tokens, audit, config, uploads, and the retention sweep — AC-13, AC-14, AC-15, AC-17
  - [x] Store test suite against a real SQLite file — AC-18
- [x] Verify it: `/check verify platform data model`
- [x] Test it: `/test platform data model`
- [x] Review it (fresh model): `/check review platform data model`
- [x] Document it: `/document platform data model`

### 4. Cluster foundation: namespaces, ingress, wildcard DNS & TLS · done
The ground the platform stands on inside k3s: how the control plane is deployed and what service account rights it holds, the namespace layout for platform versus user apps, the ingress controller, and how one wildcard hostname plus TLS reaches an app container.
**Done when:** the control plane runs in the cluster with a scoped service account, and a hand deployed hello world container is reachable over HTTPS on a generated hostname from your LAN or VPN.
spec [0003](../specs/0003-cluster-foundation/index.md) · code in `deploy`, `internal/config`, `cmd/deployer`
- [x] Design it (spec): `/architect cluster foundation`
- [x] Build it: `/develop cluster foundation`
  - [x] Tailnet routing, wildcard DNS, and the shared certificate — AC-8, AC-9, AC-10, AC-14
  - [x] Hello world reachable over HTTPS, and unreachable off the tailnet — AC-11, AC-12
  - [x] App namespace template: labels, pod security, quota, limit range — AC-5, AC-6, AC-7
  - [x] The control plane in the cluster: namespace, RBAC, volume, config, probes — AC-1, AC-2, AC-3, AC-4, AC-16
  - [x] Platform exposure and ArgoCD delivery — AC-13, AC-15. The certificate alert is deferred, see the spec's Follow-up: the cluster runs no monitoring stack, so there is nowhere to send one yet
- [x] Verify it: `/check verify cluster foundation`
- [x] Test it: `/test cluster foundation`
- [x] Review it (fresh model): `/check review cluster foundation`
- [x] Document it: `/document cluster foundation`

## Slice 1: First deploy end to end

### 5. First deploy end to end · done
The tracer bullet, and the walking skeleton in one. An agent holding a valid token calls one MCP tool, the source tarball uploads, Buildpacks builds an image, the platform deploys it to k3s with enforced defaults, and the agent gets back a healthy hostname. Real auth, real build, real cluster, deliberately narrow: one token, one sample app, one language, no status polling, no logs, no rollback yet.
**Done when:** from a fresh Claude Code session you can say "deploy this app" and reach the running app on its hostname, with the deployment recorded against a real account and app record.
spec [0004](../specs/0004-first-deploy-end-to-end/index.md) · code in `internal/auth`, `internal/uploads`, `internal/source`, `internal/httpapi`, `internal/build`, `internal/registry`, `deploy`
- [x] Design it (spec): `/architect first deploy end to end`
- [x] Build it: `/develop first deploy end to end`
  - [x] Store interfaces, the registry and build namespace, config and bootstrap seeding — AC-1, AC-17, AC-20
  - [x] The upload endpoint, the hardened `fetch-source` extractor, and the redeem path — AC-2, AC-8, AC-19
  - [x] The build Job and the registry client: digest resolve and the non root image check — AC-7, AC-9, AC-10
  - [x] App side composition: namespace, pull secret, Deployment, Service, Ingress — AC-11, AC-12, AC-13
  - [x] The reconcile loop, the `deploy_app` tool, reason codes, and the startup sweep — AC-3, AC-4, AC-5, AC-6, AC-14, AC-15, AC-16, AC-18, AC-22. The real deploy against the cluster (AC-21) needs a published image, so it runs in `/check verify`; `testdata/sample-go` is in place for it
- [x] Verify it: `/check verify first deploy end to end` — 21 of 22 acceptance criteria proved against the real cluster. AC-10 (a root image is refused) is deferred to slice 6: `deploy_app` only builds from source and Paketo will not produce a root running image, so it cannot be reached through the real path yet
- [x] Test it: `/test first deploy end to end`
- [x] Review it (fresh model): `/check review first deploy end to end`
- [x] Document it: `/document first deploy end to end`

## Slice 2: Async deployment jobs & status

### 6. Async deployment jobs & status · done
Thickens the deploy segment. The deploy call returns a job identifier immediately instead of holding the connection open through a build, the deployment walks a real state machine, and the agent can ask how it is going and get a useful answer when it fails.
**Done when:** a deploy returns a job id within a second, every state transition is recorded, a `deployment_status` MCP call reports the current state, and a failed build reports a sanitized reason rather than a timeout.
spec [0005](../specs/0005-async-deployment-jobs-status/index.md) · code in `internal/mcp`, `internal/reconcile`, `internal/store`, `internal/domain`, `internal/auth`, `internal/kube`
- [x] Design it (spec): `/architect async deployment jobs & status`
- [x] Build it: `/develop async deployment jobs & status`
  - [x] The two new reason codes and the status reads over deployments and events — AC-8, AC-11, AC-13
  - [x] The `deployment_status` tool: arguments, account scope, payload per state, description — AC-5, AC-6, AC-7, AC-9, AC-10
  - [x] The non blocking `deploy_app` and supersession reporting — AC-1, AC-2, AC-3, AC-4, AC-12, AC-20
  - [x] The deploy budget inside the reconcile loop, with the Job delete — AC-14, AC-14a, AC-15, AC-16, AC-17, AC-18
- [x] Verify it: `/check verify async deployment jobs & status` — all 20 acceptance criteria proved against the real cluster, including the real deploy from an agent session (AC-19) and the budget on resume (AC-14a). The one step still open is the root image failure projection, deferred to slice 6 for the same reason as spec 0004's AC-10
- [x] Test it: `/test async deployment jobs & status`
- [x] Review it (fresh model): `/check review async deployment jobs & status`
- [x] Document it: `/document async deployment jobs & status`

## Slice 3: Application logs

### 7. Application logs · in-progress
Thickens the readback segment so an agent can debug an app it deployed without you opening a terminal. Bounded, redacted application logs only, never cluster or platform internals.
**Done when:** a `get_logs` MCP call returns recent application output for an app the caller owns, bounded in size and time, with secrets and tokens redacted, and platform logs are never exposed.
spec [0006](../specs/0006-application-logs/index.md)
- [x] Design it (spec): `/architect application logs`
- [x] Build it: `/develop application logs`
  - [x] The pure `internal/logs` package: parsing, redaction, clamping, bounding, and the constants — AC-2, AC-3, AC-6
  - [x] The cluster reads in `internal/kube`: newest pod with its status, and one container's tail — AC-5, AC-11
  - [x] The `app_unknown` reason code and the logs audit action — AC-8, AC-9
  - [x] The `get_logs` tool: the empty case gate, the previous container block, the failure path — AC-1, AC-4, AC-7, AC-10, AC-12
  - [x] The tool description as contract, and the RBAC confirmation in verify — AC-13, AC-14

  code in `internal/logs/`, `internal/kube/kube.go`, `internal/mcp/logs.go`
- [ ] Verify it: `/check verify application logs`
- [ ] Test it: `/test application logs`
- [ ] Review it (fresh model): `/check review application logs`
- [ ] Document it: `/document application logs`

## Slice 4: Accounts, API tokens & app ownership

### 8. Accounts, API tokens & app ownership · needs a decision
Thickens the auth segment from the single token of slice 1 into something real: multiple accounts, tokens you can mint and revoke, and apps that belong to an owner so one account cannot deploy over or read another's app.
**Done when:** tokens are stored hashed and can be revoked, every API call resolves to an account, a caller cannot deploy to, read logs from, or delete an app they do not own, and every denial is recorded.
- [ ] Design it (spec): `/architect accounts, API tokens & app ownership`

## Slice 5: Workload isolation & network policy

### 9. Workload isolation & network policy · needs a decision
Thickens the runtime segment into a boundary you can trust with code an AI wrote. The platform owns the workload manifest completely and the user cannot inject privileged fields into it.
**Done when:** every app runs non root with dropped capabilities and CPU and memory ceilings, privileged mode and host mounts and host networking are impossible to request, and network policy blocks an app from reaching another app or your cluster services while still serving traffic through ingress.
- [ ] Design it (spec): `/architect workload isolation & network policy`

## Slice 6: Dockerfile build path

### 10. Dockerfile build path
Thickens the build segment with the escape hatch: when the project ships a Dockerfile, build that instead of running Buildpack detection, so apps Buildpacks cannot handle still deploy.
**Done when:** a project with a Dockerfile builds through it, a project without one still builds through Buildpacks, and which path ran is recorded on the deployment.
- [ ] Build it: `/develop dockerfile build path`

## Slice 7: App environment configuration

### 11. App environment configuration · needs a decision
Thickens the app contract so deployed apps can actually be configured. The platform injects `PORT` and any values set for the app, and decides what happens to values that are sensitive.
**Done when:** an agent can set and read configuration for an app it owns, values reach the container as environment variables, a change triggers a new release rather than mutating the running one, and sensitive values never appear in MCP responses or logs.
From spec 0006: this is also where `get_logs` gains exact redaction, because the platform will finally know which values are secret because it injected them.
- [ ] Design it (spec): `/architect app environment configuration`

## Slice 8: Rollback & release history

### 12. Rollback & release history
Thickens the release segment. Every healthy deploy is a known good release, and going back to one re promotes the exact prior image rather than rebuilding from source.
**Done when:** an agent can list an app's recent releases and roll back to one, the rollback re promotes the stored image digest without a build, health is re verified, and a failed new release never replaces a healthy current one.
- [ ] Build it: `/develop rollback & release history`

## Slice 9: App lifecycle: list & decommission

### 13. App lifecycle: list & decommission
Closes the loop. An agent can see what it has deployed and tear an app down cleanly, which matters most when the agent is generating throwaway apps.
**Done when:** a caller can list their apps with current state and hostname, and delete an app so its workload, route, namespace resources, and hostname are all released, with the delete recorded.
- [ ] Build it: `/develop app lifecycle`

## Deferred
Out of scope for the current build pass, kept so the plan stays honest.

- **Web UI**: view apps, releases, and logs without an agent · needs a decision
- **App databases**: a provisioned Postgres database and role per app · needs a decision
- **Persistent volumes**: disk that survives a restart, for apps that are not stateless · needs a decision
- **Public exposure**: real public hostnames with certificates, chosen per app · needs a decision
- **Image and dependency scanning**: block a release on a critical finding · needs a decision
- **Metrics and alerting**: CPU, memory, restart counts, and alerts on repeated failures · needs a decision
- **Platform backup and restore**: back up the metadata database and rehearse the restore. From spec 0002: the file is a secret store, not just metadata, because every release snapshots the app's configuration in clear and releases are never pruned · needs a decision
- **Admission policy on namespace delete**: a Kyverno or Validating Admission Policy rule letting the control plane delete only namespaces carrying its own ownership label, closing the one broad right left in its ClusterRole. From spec 0003 · needs a decision
- **Registry token auth for per build push credentials**: a token service issuing a per build, per repository, push only credential, closing the one place a write credential sits beside untrusted build code. From spec 0004 · needs a decision
- **Registry garbage collection**: every deploy pushes an image and nothing ever deletes one, so the registry volume grows without bound. From spec 0004 · needs a decision
- **Kubernetes events for an app that prints nothing**: surfacing image pull failures, out of memory kills, and probe failures, which explain a failed app that produced no output of its own. From spec 0006, only worth building if `state: failed` with an empty log turns out to be a common dead end · needs a decision
- **Push based deploy outcome**: a progress notification on the open call, or a webhook, so an agent that never polls still learns how its deploy ended. From spec 0005, only worth building if deploys start silently going unread · needs a decision
- **Multiple replicas and autoscaling**: horizontal scale once one pod is measurably not enough
- **Custom domains per app**: an app served on a hostname you choose rather than the wildcard slug

## Legend

**The decision box.** Every feature carries exactly one, the sub-task whose label ends with `(spec)`. Its wording varies (`Design it (spec)` normally, `Decide the stack (spec)` on Stack & architecture), so skills locate it by that `(spec)` suffix, never by an exact label. Every other box is an execution box and `/architect` never ticks one.

**Feature lifecycle**: the scope updates as a feature moves; each row is what it shows and who sets it:

| State | Set by | The feature shows |
|---|---|---|
| `planned` · needs a decision | `/scope` | one box: `Design it (spec): /architect <feature>` |
| `in-progress` (designed) | **`/architect` at spec capture** | `Design it` ticked; spec linked; `Build it: /develop <feature>` + **2 to 5 milestones**; the tier's closing boxes (`Verify it`, `Test it`, `Review it`, `Document it`); any surfaced follow-up enrolled |
| `in-progress` (building) | `/develop` | milestone sub-boxes tick one by one; code pointer filled |
| `in-progress` (verified) | `/check verify` | `Build it` + milestones ticked; `Verify it` ticked |
| `done` | **you, when you decide it is** (any skill sets it when you say so); `/sync` reconciles | boxes you ran ticked, skipped ones marked skipped; at GA the suggested point to call it done is after `/test`; `/sync` captures conventions |

- **Next step** = the first unticked box (always a command or a tracked milestone).
- **needs a decision** = run `/architect` first; otherwise straight to `/develop` (or `/audit` for standards & tooling). The tag drops once the spec is captured.
- **Atomic build tasks live in the spec's `## Build plan`, not here**: the scope carries only the milestone rollup.
- **Status** `planned` → `in-progress` → `done`, plus `existing` (pre-workflow) and `dropped` (de-scoped, kept for history).
- **Approach tag** beside a heading (e.g. `· Facade`) overrides the project default for that feature; no tag = inherits it.
- **Workflow tier tag** beside a heading (e.g. `· Beta`) sets that one feature's rigor above or below the project default; no tag inherits the default.
- **Workflow** (header line) is the project default, what runs after `/develop`: **Prototype** = nothing; **Alpha** = `/check verify`; **Beta** = `/check verify` then `/test`; **GA** = adds a fresh model `/check review` then `/document`. A feature built on an unratified decision (an `Assumed` spec) stays flagged, but that never blocks `done`.
- **Pointer line** (`spec <n> · code in <path>`): the spec link added by `/architect`, the code path by `/develop`.
