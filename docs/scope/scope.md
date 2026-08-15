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
- Apps are reachable on your LAN or VPN only, on a hostname under one wildcard domain. Slice 12 and 13 reopen the LAN or VPN half of this deliberately: the wildcard becomes reachable from the open internet, the domain and the wildcard shape do not change.
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
| 6 | Async deployment jobs & status | Slice 2 | done |
| 7 | Application logs | Slice 3 | done |
| 8 | Accounts, API tokens & app ownership | Slice 4 | done |
| 9 | Workload isolation & network policy | Slice 5 | done |
| 10 | Dockerfile build path | Slice 6 | done |
| 11 | App environment configuration | Slice 7 | done |
| 12 | Rollback & release history | Slice 8 | done |
| 13 | App lifecycle: list & decommission | Slice 9 | done |
| 14 | Web interface: register, sign in, apps & tokens | Slice 10 | done |
| 15 | Stranded deployment recovery | Slice 11 | done |
| 16 | Invite only registration | Slice 12 | done |
| 17 | Per account app cap | Slice 12 | done |
| 18 | Bounded app egress | Slice 12 | done |
| 19 | Account suspension | Slice 12 | done |
| 20 | Open internet hardening: login CSRF & control plane policy | Slice 12 | in-progress |
| 21 | Platform backup & restore | Slice 12 | planned |
| 22 | Public edge: tunnel, real certificates & the console hostname | Slice 13 | planned |
| 23 | Joining: the ready to paste agent configuration | Slice 13 | planned |

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

### 7. Application logs · done
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
- [x] Verify it: `/check verify application logs`
- [x] Test it: `/test application logs`
- [x] Review it (fresh model): `/check review application logs`
- [x] Document it: `/document application logs`

## Slice 4: Accounts, API tokens & app ownership

### 8. Accounts, API tokens & app ownership · done
Thickens the auth segment from the single token of slice 1 into something real: people register with an email and a password, verify the address, sign in to a browser session, and mint API tokens from there for their agent to carry. Apps belong to an owner so one account cannot deploy over or read another's app. No pages here: every endpoint is JSON and drivable with curl, and the web interface is feature 14.
**Done when:** a person can register, verify, sign in and mint a token, that token deploys, a second account cannot deploy to or read anything of the first's app, tokens can be revoked, and every denial and privileged action is recorded.
spec [0007](../specs/0007-accounts-tokens-app-ownership/index.md)
- [x] Design it (spec): `/architect accounts, API tokens & app ownership`
- [x] Build it: `/develop accounts, API tokens & app ownership`
  - [x] The `00002` migration and the store layer: the five account columns, the partial email index, `sessions` and `email_tokens` — AC-1, AC-5, AC-7, AC-12
  - [x] The thin thread end to end: `internal/identity`, `internal/mail` on Resend, and register, verify, login and mint wired up — AC-1, AC-3, AC-4, AC-5, AC-7, AC-12, AC-25, AC-26
  - [x] The gate everywhere and the two account ownership proof — AC-15, AC-16, AC-17, AC-18, AC-21
  - [x] Session lifecycle, token list and revoke, forgot and reset — AC-6, AC-9, AC-10, AC-13, AC-14, AC-28, AC-29
  - [x] Admin, audit, and the hardening: enumeration, rate limits, redaction — AC-2, AC-8, AC-11, AC-19, AC-20, AC-22, AC-23, AC-24, AC-27
  code in `internal/identity/`, `internal/mail/`, `internal/httpapi/{identity,authroutes,tokenroutes}.go`, `internal/store/{identity,identityadapter}.go`, `internal/auth/session.go`, `internal/store/migrations/00002_identity.sql`
- [x] Verify it: `/check verify accounts, API tokens & app ownership` — all 29 acceptance criteria proved against the real cluster, including two real accounts each owning an app called `checkout` on different hostnames. AC-15 failed on the first pass (an unverified session was told `credentials_invalid` rather than `email_unverified`), was fixed in the same session, and passes on the deployed image
- [x] Test it: `/test accounts, API tokens & app ownership` — the suite already carried the HTTP and store halves; this pass added the two gaps it left: `internal/mail` had no tests at all (AC-25, AC-26, AC-27) and the lockout backoff was only proved as far as its first window (AC-23, AC-24). Cross package coverage of `identity`, `auth` and `mail` is 86.8%
- [x] Review it (fresh model): `/check review accounts, API tokens & app ownership`
- [x] Document it: `/document accounts, API tokens & app ownership` — PR description for the test branch, drafted from the branch commits and diff

## Slice 5: Workload isolation & network policy

### 9. Workload isolation & network policy · done
Thickens the runtime segment into a boundary you can trust with code an AI wrote. The platform owns the workload manifest completely and the user cannot inject privileged fields into it.
**Done when:** every app runs non root with dropped capabilities and CPU and memory ceilings, privileged mode and host mounts and host networking are impossible to request, and network policy blocks an app from reaching another app or your cluster services while still serving traffic through ingress.
spec [0008](../specs/0008-workload-isolation-network-policy/index.md) · code in `internal/deploy/networkpolicy.go`, `internal/reconcile/policysweep.go`, `deploy/builds-networkpolicy.yaml`
- [x] Design it (spec): `/architect workload isolation & network policy`
- [x] Build it: `/develop workload isolation & network policy`
  - [x] The blocked CIDR config and the two composed policies — AC-1, AC-2, AC-3, AC-4, AC-5, AC-14
  - [x] The reconcile write, before the workload, failing closed — AC-11, AC-13
  - [x] The probe app and the fence proved live on the cluster — AC-6, AC-7, AC-8, AC-9, AC-10, AC-19
  - [x] The startup policy sweep over existing app namespaces — AC-12
  - [x] The build namespace policy, its drift pin, and the structural tests — AC-15, AC-16, AC-17, AC-18, AC-20
- [x] Verify it: `/check verify workload isolation & network policy`
- [x] Test it: `/test workload isolation & network policy`
- [x] Review it (fresh model): `/check review workload isolation & network policy`
- [x] Document it: `/document workload isolation & network policy` — PR #11 description, drafted from the branch commits and diff

## Slice 6: Dockerfile build path

### 10. Dockerfile build path · done
Thickens the build segment with the escape hatch: when the project ships a Dockerfile, build that instead of running Buildpack detection, so apps Buildpacks cannot handle still deploy.
**Done when:** a project with a Dockerfile builds through it, a project without one still builds through Buildpacks, and which path ran is recorded on the deployment.
spec [0009](../specs/0009-dockerfile-build-path/index.md)
- [x] Design it (spec): `/architect dockerfile build path`
- [x] Build it: `/develop dockerfile build path`
  - [x] The pinned BuildKit image, its own uid pair, and CI's drift check over both build images — AC-8, AC-9
  - [x] Detection as a bounded, header only walk of the stored archive, regular files only — AC-3, AC-6, AC-7a
  - [x] The BuildKit Job composed beside the Paketo one — AC-7, AC-15, AC-18
  - [x] The namespace split: `deployer-builds-dockerfile` at privileged with its own policy pair and binding, `deployer-builds` back to restricted, the config value, and the routing by a build path the deployment now carries into the loop. `baseline` was tried and refuses the pod outright — AC-10, AC-19, AC-19a, AC-20, AC-21, AC-22
  - [x] The thin thread live: both samples deploy on the cluster, BuildKit for the Dockerfile one and Buildpacks for the other — AC-1, AC-2, AC-4, AC-5, AC-17
  - [x] The refusals and the guards: the reworded failure message, the composed context test, the tool description, and both live refusals proved — AC-11, AC-12, AC-13, AC-14, AC-16
- [x] Verify it: `/check verify dockerfile build path`
- [x] Test it: `/test dockerfile build path`
- [x] Review it (fresh model): `/check review dockerfile build path` — reviewed on sonnet over the whole feature range, not just the branch. One major, an unbounded decompression in `HasRootDockerfile`, fixed in the same session; the CI `Config.User` minor fixed with it
- [x] Document it: `/document dockerfile build path` — PR [#19](https://github.com/toyinogun/deployer/pull/19)

## Slice 7: App environment configuration

### 11. App environment configuration · done
Thickens the app contract so deployed apps can actually be configured. The platform injects `PORT` and `APP_URL` plus any values set for the app, and decides what happens to values that are sensitive.
**Done when:** an agent can set and read configuration for an app it owns, values reach the container as environment variables through a Secret, a change lands on the next deploy and is snapshotted onto the release rather than mutating the running one, and sensitive values never appear in MCP responses or logs.
From spec 0006: this is also where `get_logs` gains exact redaction, because the platform will finally know which values are secret because it injected them.
From spec 0009: decide here whether a Dockerfile build receives build arguments or build secrets. Spec 0010 confirms the answer is none, deliberately, because a value that reaches a build layer is baked into a published image.
spec [0010](../specs/0010-app-environment-configuration/index.md) · code in `internal/domain/config.go`, `internal/store/config.go`, `internal/deploy`, `internal/mcp/config.go`
- [x] Design it (spec): `/architect app environment configuration`
- [x] Build it: `/develop app environment configuration`
  - [x] The domain rules and the transactional store batch: reason codes, key shape, reserved names, bounds, the required secret flag, and all or nothing writes — AC-1, AC-3, AC-4, AC-5, AC-6, AC-16
  - [x] The injection side: the app's Secret, `envFrom`, `APP_URL` beside `PORT`, the pod template checksum, and applying it from the reconcile loop — AC-7, AC-10, AC-15, AC-17
  - [x] The tools live: `set_config`, `get_config`, `unset_config`, ownership, audit rows, and the next deploy line in the response — AC-1, AC-2, AC-3, AC-8, AC-12, AC-13
  - [x] Exact log redaction, matched against the running release as well as current configuration — AC-11
  - [x] The first call path: the optional config map on `deploy_app`, the tool descriptions, and the test holding builds clear of configuration — AC-9, AC-14
- [x] Verify it: `/check verify app environment configuration`
- [x] Test it: `/test app environment configuration`
- [x] Review it (fresh model): `/check review app environment configuration`
- [x] Document it: `/document app environment configuration` — CHANGELOG entry, PR [#27](https://github.com/toyinogun/deployer/pull/27)

## Slice 8: Rollback & release history

### 12. Rollback & release history · done
Thickens the release segment. Every healthy deploy is a known good release, and going back to one re promotes the exact prior image rather than rebuilding from source.
**Done when:** an agent can list an app's recent releases and roll back to one, the rollback re promotes the stored image digest without a build, health is re verified, and a failed new release never replaces a healthy current one.
From spec 0010: a rollback must also rewrite the app's configuration Secret from the release snapshot, not only the image digest, and the pod template checksum spec 0010 adds is what makes that roll the pods.
spec [0011](../specs/0011-rollback-and-release-history/index.md) · code in `internal/domain/reason.go`, `internal/store/deployments.go`, `internal/reconcile`, `internal/mcp`
- [x] Design it (spec): `/architect rollback & release history`
- [x] Build it: `/develop rollback & release history`
  - [x] The reason code and the rollback tool: `release_unknown`, the release number lookup, `rollback_app` over the existing `CreateDeployment`, ownership and audit — AC-6, AC-7, AC-8, AC-9, AC-10, AC-18, AC-20, AC-21
  - [x] The loop learns the deployment's kind: `SourceReleaseID` on `reconcile.Deployment`, the branch in `Drive` ahead of the upload read, the `queued` branch in `run`, and the image repo recomposed from the slug — AC-11, AC-19, AC-24
  - [x] Configuration fidelity: the Secret composed from the source snapshot, the `{value, secret}` snapshot format with both contracts widened, and the `app_config` rewrite inside `MarkHealthy` — AC-12, AC-13, AC-14, AC-15, AC-16, AC-17, AC-25
  - [x] The listing: the narrow five column query, `CurrentReleaseID` on the app, `list_releases` bounded at twenty, and the empty and refused cases — AC-1, AC-2, AC-3, AC-4, AC-5
  - [x] The contract surface: both tool descriptions, and both tools driven through a real MCP session including the refusals — AC-22, AC-23
- [x] Verify it: `/check verify rollback & release history`
- [x] Test it: `/test rollback & release history`
- [x] Review it (fresh model): `/check review rollback & release history`
- [x] Document it: `/document rollback & release history` — PR [#28](https://github.com/toyinogun/deployer/pull/28)

## Slice 9: App lifecycle: list & decommission

### 13. App lifecycle: list & decommission · done
Closes the loop. An agent can see what it has deployed and tear an app down cleanly, which matters most when the agent is generating throwaway apps.
**Done when:** a caller can list their apps with current state and hostname, and delete an app so its workload, route, namespace resources, and hostname are all released, with the delete recorded.
From spec 0012: an app's state is two facts, not one, because an app whose last deploy failed is usually still serving its previous release; and the delete gets an orphan reaper behind it, the platform's first unattended destructive loop.
spec [0012](../specs/0012-app-lifecycle-list-delete/index.md) · code in `internal/mcp`, `internal/store/apps.go`, `internal/kube`, `internal/reconcile`, `internal/domain/reason.go`, `internal/config`
- [x] Design it (spec): `/architect app lifecycle`
- [x] Build it: `/develop app lifecycle`
  - [x] `list_apps` end to end: the single statement query, the 50 row bound, the serving and last deployment pair, the audit and the description, driven through a real MCP session — AC-1, AC-2, AC-3, AC-4, AC-5, AC-6, AC-7, AC-8, AC-9, AC-10, AC-11, AC-12
  - [x] `delete_app` end to end: the `deployment_in_flight` code, `kube.DeleteNamespace` tolerating gone and terminating, the one method cluster port on `internal/mcp`, the audit rows, the description, and every refusal over the wire — AC-13, AC-14, AC-15, AC-16, AC-17, AC-18, AC-19, AC-20, AC-21, AC-22, AC-28, AC-29, AC-30, AC-31
  - [x] The orphan reaper: both new variables, `LiveAppSlugs`, `AppNamespacesOlderThan`, and the pass on its own ticker plus one at startup, with the abort, label and grace guards each pinned — AC-23, AC-24, AC-25, AC-26, AC-27
  - [x] What a deleted app answers: the existing tools all refuse it, pinned rather than assumed — AC-32
- [x] Verify it: `/check verify app lifecycle`
- [x] Test it: `/test app lifecycle` — the four criteria the build left untested (AC-7, AC-11's denied row, AC-17, AC-22's freed name) now have tests behind them
- [x] Review it (fresh model): `/check review app lifecycle` — reviewed on sonnet, approve with nits
- [x] Document it: `/document app lifecycle` — CHANGELOG.md, Added, Changed and Security

## Slice 10: Web interface

### 14. Web interface: register, sign in, apps & tokens · done
From spec 0007. The pages on top of the identity surface feature 8 builds: register, verify, sign in, mint and revoke tokens, and see your apps, releases and logs without an agent. Feature 8 deliberately builds no pages, so every endpoint they need is already there and drivable with curl.
**Done when:** a person can register, verify, sign in, mint a token, and see their apps and logs in a browser, on the tailnet, with no curl and no agent.
From spec 0011: `list_releases` is capped at the newest twenty with no paging, on purpose, so this is where full release history gets a paged view over the same store method.
From spec 0012: `list_apps` is capped at the newest fifty the same way, so a paged app list belongs here too, over the store's existing cursor.
Spec 0013 corrects one line above: only identity is already drivable, since everything about an app exists solely as an MCP tool behind a bearer token. The pages read the store server side rather than through a new API.
spec [0013](../specs/0013-web-interface/index.md)
- [x] Design it (spec): `/architect web interface`
- [x] Build it: `/develop web interface` — code in `internal/web`
  - [x] Thin thread and guards: embedded assets, page session middleware, sign in, apps list, sign out, CSRF and origin — AC-1 to AC-5, AC-11 to AC-13
  - [x] Design system: tokens, sidebar and inset panel shell, tables and cards, View Transitions, reduced motion, responsive — AC-27 to AC-29
  - [x] The rest of identity: register, verify, unverified with resend, forgot, reset, and email links repointed at pages — AC-6 to AC-10
  - [x] App pages: paged list with onboarding empty state, overview with polling and failure sentences, releases, logs, config with reveal — AC-14 to AC-20, AC-26
  - [x] Tokens, admin, and the closing pass: mint and revoke, the admin accounts page, accessibility, the leak crawl, and the `DEPLOYER_CSRF_KEY` deploy wiring — AC-21 to AC-25, AC-30, AC-31
- [x] Verify it: `/check verify web interface`
- [x] Test it: `/test web interface`
- [x] Review it (fresh model): `/check review web interface` — reviewed on sonnet, changes requested; the flaky CSRF case and the token expiry coverage gap are both closed
- [x] Document it: `/document web interface` — CHANGELOG.md, Added, Changed and Security

## Slice 11: Stranded deployment recovery

### 15. Stranded deployment recovery · done
From spec 0014, which came out of a real incident rather than the plan: a lost SQLite write left a deployment sitting in `building`, so its app refused deletes and the failure was eventually recorded as `timeout`. The reconcile tick learns to ask the cluster what the build Job actually did, and either ends the row with the true reason or hands it back to be resumed. Narrowed on 2026-08-14 after `/check verify` and `/check review` both found the first framing overclaimed: restarts are already covered by the startup sweep and a dying Job by `awaitBuild`, so this check is a backstop for two internal faults, a state write that did not land and a startup sweep that did not run.
**Done when:** a deployment stranded by one of those two faults is ended at the next tick carrying the reason the Job gave rather than at the deploy budget, a build that had already succeeded is resumed rather than thrown away, the supersession race is visible in the logs, and none of it costs a new setting, a new column, or a second writer of deployment state.
spec [0014](../specs/0014-stranded-deployment-recovery/index.md) · code in `internal/reconcile`, `internal/store`, `deploy`
- [x] Design it (spec): `/architect stranded deployment recovery`
- [x] Build it: `/develop stranded deployment recovery`
  - [x] The claim: `ClaimNext` prefers queued work and falls back to adopting a stray, plus releasing a claim as one conditional write — AC-3, AC-5, AC-5a
  - [x] The check in the tick: the whole Job state table, ahead of the budget pass, with no new setting — AC-1, AC-2, AC-4, AC-4a, AC-5b, AC-6, AC-7, AC-8
  - [x] The proof: fake clientset coverage for every branch, the ordering, the fairness and the supersession race — AC-1 to AC-7, AC-9
  - [x] The invariant written where it is enforced: the single replica note beside the manifest in `deploy/AGENTS.md` — AC-7
  - [x] The race made visible: `ReleaseClaim` returns whether it released a row, and the loop logs the no-op apart from the success — AC-10
  - [x] The two faults tested: a passthrough over the real store failing one call, covering a `Transition` that errors and a `ListNonTerminal` that errors on the startup sweep — AC-1, AC-2
- [x] Verify it: `/check verify stranded deployment recovery`
- [x] Test it: `/test stranded deployment recovery`
- [x] Review it (fresh model): `/check review stranded deployment recovery`
- [x] Document it: `/document stranded deployment recovery`

## Slice 12: The controls the tailnet was providing

Everything through slice 11 was built with the tailnet as the outer fence, and several decisions were taken knowingly because of it: registration is open to anyone who can reach the page, nothing counts apps per account, app egress to the internet is unbounded, the pre authentication posts carry no CSRF token, and there is no way to stop a bad account short of the database. Handing the platform to invited strangers removes that fence, so this slice rebuilds each of those as a real control.

The ordering is deliberate and it is not the usual tracer bullet shape. The public edge in slice 13 is one manifest change and it is the only irreversible step, so it goes last rather than first. Nothing in this slice needs it: every control is testable on the tailnet today.

Settled for these two slices, so no feature reopens them: the platform stays on your homelab cluster and is reached through a tunnel, so your home address never appears in DNS or a certificate; signup is invite only; there is no billing; apps become publicly reachable on the existing wildcard; egress is open outbound with the known abuse ports closed rather than deny by default; the domain stays `deploy.toyintest.org`; and the first months hold under ten people.

### 16. Invite only registration · done
Registration today accepts anyone who can open the page, which was safe only because reaching the page meant being on the tailnet. An invite becomes the thing that authorises an account, so opening the front door does not mean opening it to everyone.
**Done when:** registration without a valid invite is refused with a closed reason code, an invite is single use and expires, you can issue and revoke invites from the admin page, spending one is recorded against the account it created, and the accounts that already exist are untouched.
spec [0015](../specs/0015-invite-only-registration/index.md)
- [x] Design it (spec): `/architect invite only registration`
- [x] Build it: `/develop invite only registration`
  - [x] The `00003` migration and the store layer: the `invites` table, its index, the queries and the adapter methods, purely additive — AC-12
  - [x] The thin thread: the seven day lifetime, `invite_invalid` at 403, the check first in `Register`, and `SpendInviteAndCreateAccount` proving one invite from mint to account — AC-1, AC-3, AC-5, AC-11
  - [x] The race and the taken address: the guarded update on the full live predicate, and the invite surviving an address that is spoken for — AC-4, AC-10
  - [x] Both doors: `invite` on the JSON register, the hidden field, the referrer header, the bare page sentence, and a GET that never validates — AC-2, AC-16, AC-17, AC-18
  - [x] The admin surface and the bootstrap: `/admin/invites` with mint, revoke and derived state, CSRF on both mutations, the startup mint, the audit rows and the leak crawl — AC-6 to AC-9, AC-13 to AC-15, AC-19
code in `internal/store/invites.go`, `internal/identity/invites.go`, `internal/web/invites.go`, `internal/httpapi/inviteroutes.go`
- [x] Verify it: `/check verify invite only registration`
- [x] Test it: `/test invite only registration`
- [x] Review it (fresh model): `/check review invite only registration`
- [x] Document it: `/document invite only registration`

### 17. Per account app cap · done
Nothing counts apps per account, so one account can create as many as the cluster will hold. Every quota built so far bounds what one app consumes, not how many an account may start. With under ten people this is about stopping one runaway account rather than sharing scarce capacity.
**Done when:** an account at its cap is refused a new app with a closed reason code that names the cap, the refusal reads the same through an MCP tool and through the pages, the cap is `DEPLOYER_*` configuration validated at startup, a deleted app frees a slot, and accounts already over the cap keep what they have.
spec [0016](../specs/0016-per-account-app-cap/index.md) · code in `internal/config`, `internal/domain`, `internal/store`, `internal/mcp`, `internal/web`
- [x] Design it (spec): `/architect per account app cap`
- [x] Build it: `/develop per account app cap`
  - [x] The configured number, the reason code, and the live count read — AC-7, AC-8, AC-15
  - [x] The refusal through `deploy_app`, proved over the wire, with the tool description — AC-1, AC-2, AC-3, AC-4, AC-9, AC-13, AC-16
  - [x] Exactness: the count and the insert in one transaction, with a race test — AC-6
  - [x] The surfaces: apps page usage and notice, admin per account count — AC-10, AC-11, AC-12
  - [x] The edges: a delete frees a slot, an over cap account keeps everything, no migration — AC-5, AC-14, AC-17
- [x] Verify it: `/check verify per account app cap`
- [x] Test it: `/test per account app cap`

### 18. Bounded app egress · done
Cluster traffic is fenced, but an app's outbound path to the internet is wide open. That is how a stranger's app mines coins or sends spam, and both cost you a relationship with your internet provider rather than just some CPU. Ports, not hostnames: an allow list by hostname is still deferred because it breaks every app that calls an API until its owner declares it.
**Done when:** a pod cannot open outbound mail or the common mining pool ports, an ordinary outbound HTTPS call still works, the bound is applied to every app namespace including the ones that already exist, and both the block and the allowed call are proved against the real cluster rather than the fake clientset.
spec [0017](../specs/0017-bounded-app-egress/index.md) · code in `internal/config`, `internal/deploy`, `deploy/`, `testdata/probe`, `internal/mcp`
- [x] Design it (spec): `/architect bounded app egress`
- [x] Build it: `/develop bounded app egress`
  - [x] The baseline, before anything changes: the probe gains its four named targets and reads all four as reached, which is the only thing that later makes a block attributable to the policy rather than to your internet provider — AC-7, AC-8
  - [x] The configured list and the complement: `DEPLOYER_APP_EGRESS_BLOCKED_PORTS` with every boot refusal, and the pure function that inverts it into ranges, written test first — AC-1, AC-2
  - [x] The composed rule: the ports list on the existing public egress rule with an explicit UDP protocol, pinned against the eight literal ranges and the single peer — AC-3, AC-4, AC-14
  - [x] Proved live: 25 and 3333 now time out, 443 and 587 still reached, the app still serving and ready, and an older namespace picking it up on restart with no redeploy — AC-5, AC-6, AC-11, AC-12
  - [x] The build namespaces and the contract: both static policies narrowed to TCP 80 and 443 with both build paths still completing, and the bound named in `deploy_app`'s description — AC-9, AC-10, AC-13
- [x] Verify it: `/check verify bounded app egress`
- [x] Test it: `/test bounded app egress`
- [x] Review it (fresh model): `/check review bounded app egress`
- [x] Document it: `/document bounded app egress`

### 19. Account suspension · done
The control you reach for at 2am when one account is the problem. Ownership is already solid, so the hard part is deciding what suspended means for a running app and how it comes back.
**Done when:** an admin can suspend an account from the page that already exists, its apps stop serving, its sessions and API tokens stop working, a deploy or any other tool call is refused with a closed reason code, unsuspending restores serving without a rebuild, and both directions are recorded in the audit trail.
spec [0018](../specs/0018-account-suspension/index.md) · code in `internal/suspend`, `internal/auth`, `internal/mcp`, `internal/kube`, `internal/store`, `internal/reconcile`, `internal/web`, `internal/httpapi`
- [x] Design it (spec): `/architect account suspension`
- [x] Build it: `/develop account suspension`
  - [x] The refusal thread, end to end first: the new closed reason code, token resolution telling a suspended account from a dead token, one gate refusing every MCP tool as a tool result, the upload endpoint at 403, proved through the real HTTP handler — AC-9, AC-10, AC-11, AC-12, AC-13, AC-15
  - [x] The cluster half: `ScaleWorkload` in `internal/kube`, the two store reads, and the `internal/suspend` use case that stops and restores an account's live apps, collecting partial failures as data — AC-1, AC-2, AC-3, AC-4, AC-5, AC-6, AC-16, AC-22
  - [x] The safety net: the sweep holding suspended accounts at zero on the reconcile cadence, re-reading state before each write so a concurrent restore survives — AC-7, AC-8, AC-24
  - [x] The in flight case: the reconcile drive checking suspension at every phase boundary and ending the row with the reason code, build Job deleted — AC-14
  - [x] The surfaces and the leftovers: both admin routes through the one use case, the Suspend and Restore wording, the partial stop message, and the checks that nothing expires and no migration was added — AC-17, AC-18, AC-19, AC-20, AC-21, AC-23
- [x] Verify it: `/check verify account suspension` — three runs against the real cluster, the last one closing AC-12's expired token and AC-3's soft deleted app
- [x] Test it: `/test account suspension`
- [x] Review it (fresh model): `/check review account suspension` — reviewed on sonnet, approve with nits
- [x] Document it: `/document account suspension`

### 20. Open internet hardening: login CSRF & control plane policy · in-progress
Two hardening items were weighed and skipped on tailnet grounds and are named as such in their specs. From spec 0013: the pre authentication posts carry no synchroniser token, because there is no session to bind one to. From spec 0008: nothing stops a workload elsewhere on the cluster reaching the platform API at all, since tokens guard it.
**Done when:** the sign in, register, forgot, reset and resend posts each carry a token bound to a pre session cookie and a forged post is refused, and `deployer-system` accepts ingress only from the tailnet proxy, the two build namespaces, the control plane pod itself and the cluster's own nodes, with the platform still fully working from all of them.
Spec 0019 corrects the line above three times, each one found by a live run rather than by reading. The control plane is not behind ingress-nginx at all, it sits on the `tailscale` ingress class. The peer list is not three but four, because the control plane reads a pushed image back from the registry over the pod network, so `deployer-system` is a peer of itself and leaving that out breaks every deploy while passing every test. And the nodes cannot be named by address: Cilium settles node traffic onto reserved identities before a CIDR rule is read, so the four `/32` entries admitted nothing and image pulls failed on every node but the registry's. They are gone, replaced by a Cilium entity rule, which is also why nothing here needs editing when a node is added.
spec [0019](../specs/0019-open-internet-hardening/index.md) · code in `deploy/deployer-system-networkpolicy.yaml`, `deploy/deployer-system-cilium-networkpolicy.yaml`, `internal/web/pretoken.go`
- [x] Design it (spec): `/architect open internet hardening`
- [x] Build it: `/develop open internet hardening`
  - [x] The policy, riskiest first: the proxy pod's namespace confirmed live, the ingress only v1 pair with its three pod sourced peers against container ports, the Cilium object carrying the node peers by identity, and the parse tests pinning both — AC-11, AC-12, AC-12a, AC-15, AC-18
  - [x] Proved on the real cluster: an app namespace refused on 5000 and 8080, a pull proved both ways on a node that is not the registry's, and every caller in AC-14's list still working, ending with a deploy carried to `healthy` rather than a build Job going green — AC-13, AC-14, AC-19
  - [x] The token machinery: the hex nonce cookie, the name and flags chosen off `s.secure`, the HMAC derive and the constant time compare, plus the form aware refusal beside the existing one — AC-2, AC-2a, AC-3, AC-5, AC-6, AC-9
  - [x] The thin thread on `/login`, then the other four posts with `/resend` taking its cookie from `/unverified` — AC-1, AC-4, AC-7, AC-8, AC-10
  - [x] The leftovers: every existing caller that posts to a page path given a cookie jar, the leak crawl extended, and the two mechanisms written down in `internal/web/AGENTS.md` — AC-4, AC-9, AC-17
- [x] Verify it: `/check verify open internet hardening`
- [x] Test it: `/test open internet hardening`
- [x] Review it (fresh model): `/check review open internet hardening`
- [ ] Document it: `/document open internet hardening`

### 21. Platform backup & restore · needs a decision
The SQLite file is a secret store, not just metadata: every release snapshots the app's configuration in clear, releases are never pruned, and since slice 7 those are live third party credentials rather than hypothetical ones. Right now one lost volume is every account, every app, and every secret anyone ever set.
**Done when:** the database is backed up on a schedule to somewhere that is not the cluster, the backup is encrypted at rest, a restore has actually been rehearsed into a scratch instance rather than assumed, and you can sign in to the restored copy.
- [ ] Design it (spec): `/architect platform backup & restore`

## Slice 13: The public edge

The flip. One manifest change plus the pieces that only matter once the traffic is real, kept apart from slice 12 so the irreversible step lands on top of controls that already work.

### 22. Public edge: tunnel, real certificates & the console hostname · needs a decision
The control plane sits on the `tailscale` ingress class and the wildcard certificate is still issued by the staging authority. This gives the console a name of its own, moves both onto certificates a browser trusts, and puts a tunnel in front so your home address is never in DNS. It also has to solve the smaller thing the tunnel breaks: behind a proxy the rate limiter and the audit trail see the proxy, not the visitor.
From spec 0019: this feature has to add its tunnel's namespace as an ingress peer on the new `deployer-system` policy, or the console goes dark on the flip, since that policy's only inbound peer today is the `tailscale` namespace it is replacing.
**Done when:** from a machine with no Tailscale the console answers on its own public hostname with a trusted certificate and a deployed app answers on its wildcard hostname, the console name is reserved so no app slug can ever claim it, your home address appears in no DNS record or certificate, the rate limiter and audit rows carry the visitor's real address, and cluster administration stays on the tailnet.
- [ ] Design it (spec): `/architect public edge`

### 23. Joining: the ready to paste agent configuration · needs a decision
Joining is still a developer's job even though using the platform is not. Removing Tailscale takes it from four steps to three, and the step that actually goes wrong is the last one: a token is a password that grants deploys on your cluster, and pasting a secret into a configuration file by hand is exactly what a non technical person mishandles. This hands them one block to copy rather than a token and a format to work out.
**Done when:** a newly verified person lands on one page holding a ready to paste MCP client block with a token already in it, the token is minted at that moment and shown once, it appears in their token list afterwards like any other, and the page never shows a token again on a later visit.
- [ ] Design it (spec): `/architect joining`

## Deferred
Out of scope for the current build pass, kept so the plan stays honest.

- **App databases**: a provisioned Postgres database and role per app · needs a decision
- **Persistent volumes**: disk that survives a restart, for apps that are not stateless · needs a decision
- **Public exposure per app**: a real public hostname with a certificate, chosen per app. Narrowed by slice 13: the shared wildcard becomes publicly reachable there, so what is left here is an app choosing its own name rather than taking its slug · needs a decision
- **Image and dependency scanning**: block a release on a critical finding. From spec 0009, base image allowlisting for Dockerfile builds belongs with this rather than on its own · needs a decision
- **Metrics and alerting**: CPU, memory, restart counts, and alerts on repeated failures · needs a decision
- **Platform backup and restore**: back up the metadata database and rehearse the restore. From spec 0002: the file is a secret store, not just metadata, because every release snapshots the app's configuration in clear and releases are never pruned. From spec 0010: once apps carry configuration, those are live third party credentials, not hypothetical ones · promoted into feature 21
- **Build secrets for Dockerfile builds**: BuildKit can mount a secret for one build step without leaving it in a layer, which is what a private package registry needs. From spec 0010, deliberately not built: it exists only on the Dockerfile path, so it would make the two builders behave differently before anyone has asked for it · needs a decision
- **Admission policy on namespace delete**: a Kyverno or Validating Admission Policy rule letting the control plane delete only namespaces carrying its own ownership label, closing the one broad right left in its ClusterRole. From spec 0003. Spec 0012 raises this: the namespace delete right stops being a right nothing uses and becomes an unattended loop, so the fence being a name prefix rather than an ownership label now matters more · needs a decision
- **Registry token auth for per build push credentials**: a token service issuing a per build, per repository, push only credential, closing the one place a write credential sits beside untrusted build code. From spec 0004 · needs a decision
- **Registry garbage collection**: every deploy pushes an image and nothing ever deletes one, so the registry volume grows without bound. From spec 0004. Spec 0012 widens it: a deleted app's images are the clearest case of an image nothing will ever pull again, and deleting an app now frees cluster resources but not registry disk · needs a decision
- **A reason code for an unreachable registry**: `build_no_digest` currently answers both "your build pushed no manifest" and "the control plane could not reach the registry at all", and the second reads to a caller as the first. From spec 0019, where the ambiguity sent a verify run looking at the build for an hour while the build pod log showed a clean push. Splitting it is a change to the closed set in `internal/domain/reason.go` · needs a decision
- **Layer cache for Dockerfile builds**: a registry backed cache, so a rebuild is not cold every time. From spec 0009, left out because it would put unbounded cache images on the same registry that has no garbage collection, so it is only worth deciding together with the item above · needs a decision
- **Kubernetes events for an app that prints nothing**: surfacing image pull failures, out of memory kills, and probe failures, which explain a failed app that produced no output of its own. From spec 0006, only worth building if `state: failed` with an empty log turns out to be a common dead end · needs a decision
- **Push based deploy outcome**: a progress notification on the open call, or a webhook, so an agent that never polls still learns how its deploy ended. From spec 0005, only worth building if deploys start silently going unread · needs a decision
- **Network policy on the control plane namespace**: ingress to `deployer-system` from `ingress-nginx` and `deployer-builds` only, so a workload elsewhere on the cluster cannot reach the platform API at all. From spec 0008, deliberately left out of slice 5; the API is guarded by tokens, so this is defence in depth rather than an open door · promoted into feature 20
- **A written node rule for every app namespace**: each `app-<slug>` namespace denies all ingress and names no node peer, so its readiness probes land only because Cilium permits local host traffic by default. From spec 0019, which decided not to accept that unwritten dependency for `deployer-system` and left it standing everywhere else. It costs nothing today and breaks every app's probes at once if host firewall is ever turned on. The step is larger than it sounds: the app policy is composed in Go at deploy time, so this would be the first Cilium object the control plane generates rather than a static file · needs a decision
- **Egress policy on the control plane namespace**: feature 20 policies ingress only, on purpose. Locking outbound would mean enumerating the Kubernetes API server on node addresses, cluster DNS, the registry, and Resend on a public address that can change, and the API server peer moves whenever a node is added, so a mistake there is a full outage rather than a warning. From spec 0019, worth deciding on its own once the node list stops moving · needs a decision
- **A check that the policy's node addresses match the cluster**: feature 20 writes the four node addresses into static YAML with nothing enforcing them, so adding a node and forgetting the file breaks image pulls on that node alone, which is the hardest kind of failure to attribute. From spec 0019, cheap only if there turns out to be a sensible place to run it, CI having no cluster access and the startup sweep being an odd home for it · needs a decision
- **CSRF on the JSON identity endpoints**: feature 20 guards the page posts only, on the reasoning that the JSON register, login, forgot and reset endpoints are not cookie authenticated, so a forged post achieves nothing curl could not. From spec 0019, worth reopening if a browser surface is ever built on top of them · needs a decision
- **Egress by hostname for apps**: a per app allow list of external hosts, using CiliumNetworkPolicy `toFQDNs`, which would bound exfiltration and not just cluster reach. From spec 0008, only expressible once slice 7 gives an app a way to declare its configuration. Feature 18 takes the cheap half of this, closing the known abuse ports, and leaves the hostname allow list here: it breaks every app that calls an API until its owner declares it, which is a support burden with non technical users · needs a decision
- **Stranded recovery for `pushing` and `deploying`**: from spec 0014, which covers `building` only because a build Job is cheap, unambiguous evidence. The later phases need registry and workload evidence that is more expensive and easier to misread, and mistaking a rollout in progress for a stranded one would end a deploy that was going to succeed. They keep the deploy budget as their backstop, which is no worse than today · needs a decision
- **A test that a tool description matches its behaviour**: every MCP tool description is contract rather than documentation, and nothing checks one against what the code does. From spec 0011, which adds two more, taking the gap to eight tools wide · needs a decision
- **Write actions from the browser**: rollback, delete and configuration editing as page actions, which feature 14 deliberately left out to keep the pages read only for apps. From spec 0013, where rollback is named as the one with a real case behind it, since it is the thing you want at 2am · needs a decision
- **Telling a suspended person anything at all**: from spec 0018, which deliberately says nothing to the account it stops. No email on suspension, no notice page on the app's hostname, and a visitor gets a bare ingress error. Worth reopening if suspension is ever used on someone who was not abusing the platform · needs a decision
- **Reclaiming a suspended account's storage**: from spec 0018. Suspension stops the pods and leaves the uploads, releases, and database rows exactly where they are, so a suspended account still costs disk. That is a retention decision rather than part of the control · needs a decision
- **An audited browser reveal of secret configuration values**: from spec 0013, weighed and refused so that `store.ListConfigForDeploy` keeps exactly two callers and a stolen session stays worth less than a stolen API token. Worth reopening only if reading a secret back becomes a real debugging need · needs a decision
- **Static sites without a Dockerfile**: a plain HTML page uploaded on its own fails Buildpacks detection in about nine seconds, so the simplest thing a non technical person can ask an agent for is the one thing the platform cannot build unaided. Proved on the cluster 2026-08-14, and `testdata/sample-static` records it. `deploy_app`'s description now names the Dockerfile to use, which lets an agent correct itself on the first failure, and that may well be enough. Building it in would mean the platform composing a Dockerfile for a source tree it recognises as static, which is the first time it would author a build rather than run one the caller supplied · needs a decision
- **A per account cap on apps**: nothing counts apps per account, so one account can create as many as it likes and the ceiling is the cluster. Every quota built so far is per app namespace, which bounds what one app consumes and not how many an account may start. Raised by handing a second person an account: isolation between accounts is solid, capacity between them is shared and ungoverned, and registration is open to anyone who can reach the page. It has stayed invisible only because the tailnet has kept the number of accounts at one · promoted into feature 17
- **Getting a new person from nothing to a connected agent**: joining the platform is still a developer's job, even though using it is not. Four steps stand between a new person and their first deploy: install Tailscale and accept a node share, register and verify, mint a token, and paste that token into an MCP client's configuration as a bearer header. Only the middle one is built. The web interface made the platform person friendly at its edges and left the joining unaddressed, which did not show while there was one account. The token paste is the step that actually goes wrong, because a token is a password that grants deploys on the cluster and pasting a secret into a config file is exactly what a non technical person mishandles. Raised 2026-08-14 by working out what handing an account to a friend costs. Worth being clear it may be the wrong problem to solve: anyone running Claude Code or Codex is already using a developer's tool, so the coding agent is a higher wall than the configuration. If it is worth solving, the shape is handing someone one thing rather than four, an invite link that mints the token and returns a ready configuration, and that is a new credential path with its own security model rather than a convenience. Features 16 and 23 take the two halves that do not need a new credential path, the invite and the copyable block; what stays here is the one click version that provisions the client for them · needs a decision
- **Dark mode for the web interface**: from spec 0013, scoped out of feature 14. Cheap as a token swap under `prefers-color-scheme` only because the stylesheet defines every colour as a token from the start · needs a decision
- **A per account override on the app cap**: a nullable `app_limit` on `accounts` plus an admin control, so one person can be given more room than the global number without moving everyone's ceiling. From spec 0016, which chose one global number deliberately: with under ten people the global figure is likely right for everyone, and the shape of the exception is a guess until it is not. Worth building the first time someone genuinely needs a different ceiling · needs a decision
- **A shared guard on app creation**: spec 0016's cap is correct because `deploy_app` is the only path that creates an app row, and nothing enforces that property. A web create route, or any second caller, would silently bypass the cap. From spec 0016, which left it as a wrapper with one caller. It becomes real work the moment a second create path is proposed, and the write actions from the browser entry above is the likeliest source of one · needs a decision
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
