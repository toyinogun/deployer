# 0012. App lifecycle: what a listing reports, and how a delete tears an app down

**Date**: 2026-08-13
**Status**: Proposed

## Summary

An agent can now see every app it has deployed and tear one down cleanly. `list_apps` reports two separate facts per app rather than one blurred state: what the app is serving right now, and how its most recent deploy ended, because an app whose last deploy failed is usually still up on its previous release. `delete_app` writes the delete to the database first and then deletes the app's whole Kubernetes namespace, which cascades every object inside it in one call. If that cluster call fails, a new reaper pass in the reconcile loop finishes the job, so the database and the cluster cannot quietly disagree.

## Requirements

**User stories**:
- As an agent that has deployed several apps, I want to see all of them with their addresses so that I know what exists without keeping my own notes.
- As an agent looking at an app that is misbehaving, I want the listing to tell me both what is running and how my last deploy ended, so that I do not read "failed" and conclude the app is down when it is serving fine.
- As an agent generating throwaway apps, I want to delete one and have its workload, its route, its hostname and its cluster resources all released, so that the cluster does not fill with abandoned apps.
- As the platform owner, I want a delete that half succeeded to be cleaned up without me, so that a failed teardown does not leave a namespace holding a hostname and a quota forever.

**Acceptance criteria**:

- **AC-1**: `list_apps` takes no arguments and returns the caller's live apps, newest first, at most `MaxAppListing` (50). There is no caller supplied limit and no cursor.
- **AC-2**: Each row carries `name`, `slug`, `url`, `created_at`, `last_deployed_at`, `serving`, and `last_deployment`. `url` is composed from the slug by the existing `appURL` helper, the same address `deploy_app` and `deployment_status` report.
- **AC-3**: `serving` carries `release_number`, taken from the release `apps.current_release_id` points at. An app that has never been healthy has no `serving` object at all, rather than a zero release number.
- **AC-4**: `last_deployment` carries `deployment_id`, `state`, and `reason`, where `reason` is present only when the state is `failed` or `cancelled`. It is the app's newest deployment by `created_at`, an in flight one included, so a deploy still running shows as `building` here. An app with no deployments has no `last_deployment` object.
- **AC-5**: The two are independent. An app whose newest deployment is `failed` and whose `current_release_id` is set reports both: a `serving` release number and a `failed` `last_deployment`. Nothing in the response collapses them into one state.
- **AC-6**: `last_deployed_at` is the newest deployment's `finished_at`, whatever its outcome. It is absent when no deployment for that app has finished.
- **AC-7**: No configuration key or value appears anywhere in the response. The listing's query projects only the columns above and never reads `app_config` or `releases.config_snapshot`.
- **AC-8**: The whole listing is one SQL statement. It does not read the apps and then loop reading each app's newest deployment, and it makes no Kubernetes call at all.
- **AC-9**: An account with no live apps gets an empty list, returned successfully, not a refusal. A soft deleted app never appears, and there is no argument that brings one back.
- **AC-10**: The listing is scoped to the caller's account for every caller, an account with `is_admin` set included. No MCP tool widens on the admin flag.
- **AC-11**: `list_apps` writes no `audit_log` row when it succeeds. A refused call writes a `denied` row with action `app_list`, matching how `list_releases` and `deployment_status` audit.
- **AC-12**: `list_apps`'s tool description says the listing is capped at the newest 50 apps and that there is no way to page past them, the same contract wording `list_releases` carries for its own bound. The response carries no `truncated` field.
- **AC-13**: `delete_app` takes the app's `name`, with no confirmation flag and no slug. On success it returns `name`, `slug`, and `deleted` true.
- **AC-14**: The database write happens before the cluster action. `store.SoftDeleteApp` runs first, inside its existing transaction, and only then is the namespace deleted.
- **AC-15**: A deployment in flight for that app refuses the whole call with the new reason code `deployment_in_flight`. Nothing is written and nothing is torn down: the existing `ErrDeploymentInFlight` from `SoftDeleteApp` is what decides it, inside the same transaction that would have set `deleted_at`.
- **AC-16**: The teardown is one call: `kube.DeleteNamespace` on `app-<slug>`. Kubernetes cascades the Deployment, Service, Ingress, Secret, both NetworkPolicies, the ResourceQuota, the LimitRange and the RoleBinding inside it. No object is deleted individually.
- **AC-17**: `delete_app` does not wait for the namespace to finish terminating: the handler issues one API call and has no polling loop, the same way `deploy_app` and `rollback_app` return without waiting. Nothing asserts an elapsed time.
- **AC-18**: A namespace that is already gone, or already terminating, is success rather than a fault. The tolerance lives in `internal/kube.DeleteNamespace`, which returns nil for a `NotFound` and for a namespace already `Terminating` and a plain wrapped error otherwise: no handler in `internal/mcp` or `internal/reconcile` ever inspects a Kubernetes error, the same way `ignoreExists` already keeps that knowledge inside `internal/kube`. An app registered but never deployed has no namespace and deletes cleanly.
- **AC-19**: A namespace delete that fails for any other reason is refused to the caller with `internal`, with the row already soft deleted. The platform log carries the cluster error at error level; the reason code never carries it. The reaper (AC-23) is what finishes the teardown.
- **AC-20**: `delete_app` on a name that does not exist, on an app belonging to another account, and on an app already deleted are all refused with `app_unknown` and the same message, indistinguishable to the caller. Two concurrent deletes of the same app resolve the same way: the second one's `SoftDeleteApp` updates no row, and that `ErrNotFound` maps to `app_unknown`.
- **AC-21**: A delete removes nothing else. The app's `deployments`, `deployment_events`, `releases` and `app_config` rows stay, `apps.current_release_id` is left as it is, and no image is removed from the registry.
- **AC-22**: The slug stays reserved forever, so the hostname is never handed to another app. The app's `name` becomes free again for the same account, and a new app created under it gets a new slug and therefore a new hostname.
- **AC-23**: The orphan reaper runs on its own ticker, not on the reconcile tick. `Run` keeps its existing ticker for claiming deployments and gains a second one at `DEPLOYER_REAP_INTERVAL_SECONDS` (default 600, ten minutes), selected on in the same loop, and it also runs one pass at startup beside `PolicySweep` and `Sweep`. The reconcile tick is a claim one deployment loop firing seconds apart, so a cluster wide namespace list has no business on it. Each pass deletes every app namespace whose slug has no live app row and that is older than the grace.
- **AC-24**: The reaper reads the live slugs once per pass, as a single query. Any error reading them aborts the whole pass and reaps nothing: a database that cannot answer must never be read as "no app owns this".
- **AC-25**: The reaper only ever considers namespaces that the `AppNamespaces` selector returns, which is the `app.kubernetes.io/managed-by=deployer` label plus the app slug label, both set in `internal/deploy.Namespace` when the namespace is composed. A namespace named `app-something` that the platform did not create is never touched, and `deployer-system` and `deployer-builds` are outside the selector. A namespace missing either label is invisible to the reaper and so is never reaped, which is the safe direction of that failure.
- **AC-26**: A namespace younger than `DEPLOYER_ORPHAN_GRACE_SECONDS` (default 900, fifteen minutes) is skipped, so a namespace a deploy created seconds ago cannot be reaped by a pass that raced it. Both new variables are validated in `internal/config` at startup, like every other `DEPLOYER_*` variable, and neither has any required relationship to the deploy timeout (see the invariant below).
- **AC-27**: Each reap is logged at info level with the slug, matching how `PolicySweep` logs a policed namespace. A namespace that will not delete is logged and stepped over, leaving the rest of the pass to run.
- **AC-28**: `deployment_in_flight` is a closed reason code in `internal/domain/reason.go` with its own one line message, covered by `Reason.Valid`. The set becomes twenty codes, and the package doc comment that today reads "The nineteen codes" is updated in the same commit.
- **AC-29**: `delete_app` writes an `allowed` `audit_log` row on success and a `denied` row carrying the reason code on every refusal, with action `app_delete`. An allowed row and a `deployment_in_flight` refusal carry the app id as the target; an `app_unknown` refusal carries no target, because no app was resolved, which is the shape `denyConfig` already writes for the same case.
- **AC-30**: `delete_app`'s tool description carries what a caller cannot discover by trying: that it is not reversible, that the hostname is never reused, that it does not wait for termination, that a deploy in flight refuses it, and that release history and configuration rows are kept rather than purged.
- **AC-31**: Both tools are exercised end to end through a real MCP client and server session via the `callOverTheWire` helper, in their own test file, so a refusal the argument schema would catch first cannot pass the suite as a reason code.
- **AC-32**: After a delete, `deployment_status`, `get_logs`, `get_config`, `list_releases` and `rollback_app` for that app answer `app_unknown` (or `deployment_unknown` for a status read by id). This needs no code change, because every one of them resolves through a live app read; the suite pins it rather than assuming it.

## Decision

**Chosen option**: Option 2: Two independent facts per app, and a namespace delete backed by a reaper.

`list_apps` reports what an app is serving and how its last deploy ended as two separate fields read in one query; `delete_app` soft deletes the app row first and then deletes its namespace in a single cascading call, with an orphan reaper in the reconcile loop closing the gap when that second step does not happen.

**Implementation skills**: `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`)

## Rationale

Reasoning, the options weighed, and why each was rejected: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

No schema change. The migration count stays at two. Everything this feature needs is already in place from spec 0002:

| Table | Column | Role here |
|---|---|---|
| `apps` | `deleted_at` | The soft delete stamp. Null means live; every read in the platform filters on it |
| `apps` | `slug` UNIQUE across all rows | Why a retired hostname is never reused, deleted rows included |
| `apps` | `current_release_id` FK to `releases` | The source of `serving.release_number` |
| `apps` | partial index `apps_live_name` | Why the app's name is free again after a delete while the slug is not |
| `deployments` | index `deployments_by_app (app_id, created_at DESC)` | What makes the newest deployment per app a cheap correlated read |
| `deployments` | partial index `deployments_one_in_flight` | What `CountInFlightDeploymentsForApp` uses to refuse a delete |
| `releases` | `release_number` | Reported as `serving.release_number` |
| `audit_log` | `action`, `target_id`, `outcome` | Where the delete is recorded |

**State transitions**:

The app itself has only two states, and one move between them. The deployment state machine in `internal/domain/state.go` is untouched.

```
live  ──delete_app (refused while a deployment is in flight)──▶  deleted
```

`deleted` is terminal. There is no undelete, no code path that clears `deleted_at`, and no tool that reads a deleted app.

**API surface**:

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `list_apps` | MCP tool | none | `apps[]`: `name`, `slug`, `url`, `created_at`, `last_deployed_at`, `serving{release_number}`, `last_deployment{deployment_id, state, reason}` | account token | none beyond a bad token; an empty account returns `{"apps": []}` |
| `delete_app` | MCP tool | `name`: string (req) | `name`, `slug`, `deleted` | account token, owner only | `app_unknown`, `deployment_in_flight`, `internal` |

Response shape, for the case the whole listing exists to get right:

```json
{ "apps": [ {
    "name": "notes", "slug": "notes-a1b2c3",
    "url": "https://notes-a1b2c3.<DEPLOYER_APP_DOMAIN>",
    "created_at": "2026-08-11T09:14:02Z",
    "last_deployed_at": "2026-08-13T18:02:44Z",
    "serving": { "release_number": 4 },
    "last_deployment": { "deployment_id": "dep_...", "state": "failed", "reason": "build_failed" }
} ] }
```

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| `list_apps` | `name`, `slug`, `created_at` | `apps.name`, `apps.slug`, `apps.created_at` |
| `list_apps` | `url` | Derived from `apps.slug` by the existing `Server.appURL`, which composes it from `DEPLOYER_APP_DOMAIN` (spec 0003) |
| `list_apps` | `serving.release_number` | `releases.release_number` joined through `apps.current_release_id`; the object is omitted when that pointer is null |
| `list_apps` | `last_deployment.state`, `.reason`, `.deployment_id` | The newest `deployments` row for the app by `created_at`, projecting `state`, `failure_reason`, `id`. `reason` is omitted unless the state is `failed` or `cancelled` |
| `list_apps` | `last_deployed_at` | `MAX(deployments.finished_at)` for the app, null when nothing has finished |
| `list_apps` | the 50 row bound | `mcp.MaxAppListing`, a Go constant, not a `DEPLOYER_*` variable, for the reason `MaxReleaseListing` is one: it is a product decision about an agent's context window (spec 0011) |
| `list_apps` | which account's apps | `auth.Account.ID` from the token on the call, never an argument |
| `delete_app` | the app being deleted | `apps` row resolved by `resolveOwned(account, name)`, the same path every other named tool uses |
| `delete_app` | `deleted_at` | `Store.now()`, the store's clock, not the handler's |
| `delete_app` | the namespace to delete | `deploy.NamespaceName(app.Slug)`, the same helper that composed it on the way in, never a string built in the handler |
| reaper | the set of live slugs | One `LiveAppSlugs` query over `apps WHERE deleted_at IS NULL` |
| reaper | a namespace's age | `metav1.ObjectMeta.CreationTimestamp`, compared against a `now` the caller passes in: `AppNamespacesOlderThan(ctx, now, grace)`. `internal/kube` never calls `time.Now()` itself, so the grace boundary is testable to the second |
| reaper | `now` | The reconciler's injected clock, the same source every timestamp in the platform comes from |
| reaper | the grace and the cadence | `DEPLOYER_ORPHAN_GRACE_SECONDS` and `DEPLOYER_REAP_INTERVAL_SECONDS`, both validated at startup |

**Key invariants**:

- A delete is a database write before it is a cluster action. There is no path that deletes a namespace for an app whose row is still live.
- `serving` and `last_deployment` are never derived from each other. Either can be present without the other, and the pair is the honest answer.
- The reaper only ever deletes a namespace whose slug has no live app row, read in the same pass, from a query that succeeded.
- A slug is never reused, so a namespace name is never reused, so a reap can never race a new app onto the same name.
- An app's row exists from `CreateApp`, long before any namespace is composed for it, and it stays live for the whole of every build. So an app mid deploy always has a live slug and is never a reaper candidate, whatever the grace is set to. The grace guards only the window between the namespace being created and the reaper reading the slug list; it has no required relationship to `DEPLOYER_DEPLOY_TIMEOUT_SECONDS`, and nothing should be built as though it does.
- No app output, no configuration key, and no configuration value crosses either tool's response.
- A refusal a caller sees is one of the closed reason codes, never a cluster error string.

**Security model**:

Both tools are account scoped through the existing `resolveOwned` path, so an app belonging to another account is indistinguishable from one that does not exist. `is_admin` widens nothing here; a platform wide view is the web interface slice's decision, not this one. `delete_app` is the second destructive tool on the server after `rollback_app` and is audited on both outcomes. The reaper runs unattended with the cluster right to delete namespaces, which the ClusterRole already grants and `admission-policy.yaml` already fences to names starting `app-`; its own guards (the label selector, the single live slug query, the grace period, abort on read failure) are what keep that right pointed at orphans only.

**Configuration required**:
- `DEPLOYER_ORPHAN_GRACE_SECONDS`: how old an app namespace must be before the reaper will consider it orphaned. Default 900, fifteen minutes.
- `DEPLOYER_REAP_INTERVAL_SECONDS`: how often the reaper pass runs, on its own ticker. Default 600, ten minutes. Deliberately far slower than `DEPLOYER_RECONCILE_INTERVAL_SECONDS`, because a pass lists every app namespace on the cluster.

**Critical test scenarios**:
- Happy path: two apps, one healthy and one that has never deployed, listed in one call with the right fields and the right omissions, verifies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-9**
- The case the design exists for: an app with `current_release_id` set and a newest deployment that is `failed` reports a `serving` release and a `failed` `last_deployment` at the same time, verifies **AC-5**
- Failure case: `delete_app` while a deployment is in flight, refused with `deployment_in_flight`, with the app row still live and the namespace still present afterwards, verifies **AC-15**
- Failure case: the namespace delete returns `NotFound` (an app that never deployed) and the call still reports `deleted` true, verifies **AC-18**
- Failure case: the namespace delete returns a server error, the call is refused with `internal`, and the row is nonetheless deleted, verifies **AC-19**
- Failure case: the reaper's live slug query errors and the pass deletes nothing, with the fake clientset recording no delete, verifies **AC-24**
- Failure case: a labelled namespace younger than the grace whose app row is gone is left alone; the same namespace past the grace is deleted. Both cases seed the fake namespace with an explicit `CreationTimestamp` and pass a fixed `now`, because the fake clientset leaves that field zero unless it is set, and a zero timestamp would pass the test for the wrong reason, verifies **AC-26**, **AC-23**
- Auth/permission: `list_apps` from a second account returns only that account's apps, and `delete_app` naming the first account's app is refused with `app_unknown`, word for word the message a missing name gets, verifies **AC-10**, **AC-20**
- Contract: both tools driven through a real MCP session, including every refusal, verifies **AC-31**

## Build plan

Tracer Bullet, as the project builds: each of the first two tasks is one tool end to end from SQL to a real MCP session, rather than a layer at a time across both. No migration: this feature adds no schema.

1. `list_apps` end to end: the single statement query and its store method, `MaxAppListing`, the `Apps` port method, the tool, its output shape with the omissions, the `app_list` audit action, the description, and the wire test. Satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-5**, **AC-6**, **AC-7**, **AC-8**, **AC-9**, **AC-10**, **AC-11**, **AC-12**
2. `delete_app` end to end: the `deployment_in_flight` reason code, `kube.DeleteNamespace` tolerating `NotFound` and `Terminating`, a one method `Cluster` port on `internal/mcp`, the tool over the existing `SoftDeleteApp`, the `app_delete` audit rows, the description, and the wire test including all three refusals. Satisfies **AC-13**, **AC-14**, **AC-15**, **AC-16**, **AC-17**, **AC-18**, **AC-19**, **AC-20**, **AC-21**, **AC-22**, **AC-28**, **AC-29**, **AC-30**, **AC-31**
3. The orphan reaper: both new variables in `internal/config`, `LiveAppSlugs` on the store, `AppNamespacesOlderThan(ctx, now, grace)` in `internal/kube`, `ReapOrphanNamespaces` on the reconciler, and its wiring into `Run` as a startup pass plus a second ticker in the existing select, with the abort, label, grace and logging guards each pinned by a test. Satisfies **AC-23**, **AC-24**, **AC-25**, **AC-26**, **AC-27**
4. The behaviour after a delete: a test that `deployment_status`, `get_logs`, `get_config`, `list_releases` and `rollback_app` all answer `app_unknown` or `deployment_unknown` for a deleted app, pinning what the live app reads already give for free. Satisfies **AC-32**

## Consequences

**Positive**:
- The loop closes: an agent can see what it deployed and remove it, which is what makes throwaway apps actually throwaway.
- The listing tells the truth in the case that matters. An app serving release 4 with a failed deploy 5 reads as exactly that, and no agent concludes its app is down when it is up.
- One namespace delete replaces eight object deletes, so there is no partial teardown state to reason about inside a single delete.
- The reaper makes the database and the cluster self healing on this path for the first time. It also cleans up namespaces orphaned by any earlier bug, not just by a failed delete.
- No schema change, no migration, and no new table. Every column this feature needs was designed in spec 0002.

**Negative / tradeoffs**:
- The reaper is the first unattended destructive loop in the platform. Its guards are what stop it deleting a live app's namespace, and a bug in the live slug read is a data loss bug, not a display bug. It needs the most careful review in this feature.
- `internal/mcp` gains a Kubernetes dependency it did not have. Until now every cluster action lived in the reconcile loop, and an MCP tool only wrote rows. That boundary is now crossed by one method.
- `AppNamespaces` gains a sibling with an age filter, so `internal/kube` now owns a small piece of the reaper's rule rather than the reconciler owning all of it.
- `Run` grows a second ticker and two more `DEPLOYER_*` variables. The concurrency model is still one goroutine and one deployment at a time, but the loop is no longer the single ticker it has been since slice 2.
- A delete whose namespace delete fails hands the caller `internal` for an app that is, from the database's point of view, gone. That asymmetry is deliberate and documented, but it is a confusing minute for whoever hits it.
- Registry images for a deleted app are never removed, so a delete frees cluster resources but not registry disk. That widens the already deferred garbage collection gap by exactly the apps people now delete.
- The 50 app cap has no escape hatch. An account past it cannot reach its older apps through MCP at all, the same corner `list_releases` already has at 20.

**Neutral**:
- `apps.deleted_at`, dormant since spec 0002, becomes load bearing: every read that filters on it is now a read that can be wrong in a way a user notices.
- Two new audit actions, `app_list` and `app_delete`, and one new reason code take the closed set to twenty.
- The reconcile tick does slightly more work per pass, one extra query and one extra namespace list.

## Follow-up

- [ ] Registry garbage collection (already deferred in the scope) is now a bigger gap than it was, because a deleted app's images are the clearest case of an image nothing will ever pull again. Worth reconsidering its priority after this ships.
- [ ] The deferred admission policy item asks for namespace delete to be fenced by ownership label rather than by the `app-` name prefix. The reaper makes that fence load bearing for an unattended loop rather than only for a tool call, so it is worth pulling forward.
- [ ] `list_apps` at 50 and `list_releases` at 20 are both unreachable past their cap. The web interface slice already owns paged release history; it should own paged apps too.
