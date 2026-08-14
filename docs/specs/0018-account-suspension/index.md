# 0018. Account suspension: one switch that stops a person and everything they run

**Date**: 2026-08-14
**Status**: Accepted

The decision record (context, options considered, rationale) is in [rationale.md](rationale.md).

## Summary

The platform can already lock a person out. An admin disables an account, every session and password link dies, and every credential stops resolving. What it cannot do is stop what that account is running: the apps keep serving, keep taking traffic, and keep costing the cluster. This spec extends the switch it already has rather than adding a second one. Suspending an account now scales every app it owns to zero pods, restoring scales them back with no rebuild, an agent holding a suspended token is refused with a named reason code instead of a blank credential error, and a sweep keeps a suspended account's apps at zero even if something else tries to bring them up. There is no migration.

## Requirements

**User stories**:
- As the platform owner, I want one control that stops an abusing account and everything it runs, so that the fix at 2am is a click rather than a night of `kubectl`.
- As the platform owner, I want to undo it in a click, so that suspending someone I turn out to be wrong about costs them minutes and no rebuild.
- As an agent holding a suspended account's token, I want to be told the account is suspended, so that I stop retrying and tell the person who owns me.
- As the platform owner, I want both directions recorded, so that I can read back later who was stopped, when, by whom, and which apps actually went down.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):
- **AC-1**: Suspension is `accounts.disabled_at`, the column that already exists. There is no second state, no new column, and no migration. A previous binary runs against the same database unharmed.
- **AC-2**: Suspending an account still does everything it does today in the same transaction: stamps `disabled_at`, revokes every live session, and revokes every live email link.
- **AC-3**: Suspending then scales every live app the account owns to zero replicas, inline, before the response is written. A live app is one with `deleted_at IS NULL` and a non null `current_release_id`; an app that never deployed successfully is neither stopped nor restored.
- **AC-4**: Restoring clears `disabled_at` and scales those same apps back to one replica, the same constant the deploy path composes. It creates no deployment row, no release, and no build, and the app serves the image it was serving before.
- **AC-5**: A Deployment that is not there, because the namespace is gone or the app never reached the cluster, is success on both directions, not an error.
- **AC-6**: A Kubernetes failure on one app never blocks the lockout. The account is suspended, the response names every app that did not stop, and the failure is logged with the app slug. The use case returns that partial outcome as data, a typed result carrying the slugs that did not stop, so it is a third outcome beside success and failure rather than an error string: the page renders those slugs in its message and the JSON route returns them as a field.
- **AC-7**: A sweep re-asserts zero replicas for every live app of every suspended account, at boot and on each tick of `DEPLOYER_RECONCILE_INTERVAL`. A namespace that will not take it is logged and stepped over, leaving the remaining ones swept.
- **AC-8**: The sweep only ever scales down. Nothing but an explicit restore scales an app up, so a bug in the sweep can never let a suspended account back onto the network.
- **AC-9**: Every MCP tool call made with a suspended account's token is refused `account_suspended`, as a tool result, before any tool handler runs. The refusal is returned as a `CallToolResult` with `IsError` set and a nil Go error, because an error returned from a method handler is a protocol error and reaches the agent as a broken connection rather than a decision. Nothing is written: no app, no deployment, no configuration, and no upload is spent.
- **AC-10**: That refusal is proved end to end through the real HTTP handler, a client session over `httptest` against `Server.Handler()`, the way `internal/mcp/ownership_test.go` already drives it. The `callOverTheWire` helper is not sufficient here and must not be the only coverage: it calls `serverFor` directly and so skips the authentication middleware this feature changes, which is exactly where the refusal can break in production while the test passes.
- **AC-11**: The upload endpoint refuses a suspended account with HTTP 403 and `account_suspended` in the body, rather than the 401 it answers an unknown token with.
- **AC-12**: Token resolution tells the two apart. An unknown, revoked, or expired token stays indistinguishable and answers the existing invalid credential error; a good token on a suspended account answers a distinct `ErrAccountSuspended`. Nothing else changes about token handling.
- **AC-13**: The browser is unchanged as a door. A suspended person signing in gets exactly the answer a wrong password gets, so the login form never becomes an account state oracle, and a session belonging to a suspended account still resolves to nothing.
- **AC-14**: A deployment that is queued or in flight when the suspension lands ends `failed` with reason `account_suspended`, and its build Job is deleted. Nothing it built is promoted, and no release is minted. The check is re-evaluated at every phase boundary of the drive, at each place `overBudget` is already consulted, not once when the drive reads its app: a drive blocks in `awaitBuild` and `awaitReady` for minutes, so a single read at entry cannot catch a suspension that lands during a build that was accepted a moment before it.
- **AC-24**: A restore that lands while a sweep is running is never undone by it. The sweep re-reads the owning account's `disabled_at` immediately before each scale down write and skips an account that is no longer suspended, because the sweep's list is a snapshot and the tick is short enough that an admin restore can fall inside one.
- **AC-15**: `account_suspended` is a member of the closed reason set in `internal/domain/reason.go`, carries its own static sanitized line, and `Valid()` accepts it. Because it can land in `deployments.failure_reason`, `internal/web/reason.go` carries a plain sentence for it too.
- **AC-16**: Both directions are audited: one `admin` row naming the target account and which way it went, plus one row per app stopped or restored, against target type `app` and the app's id. An app that failed to stop is audited as not allowed with the same reason.
- **AC-17**: An admin may suspend another admin. Suspending yourself is still refused, because it revokes the session reading the page.
- **AC-18**: The typed email confirmation still gates the suspend action, unchanged, so a misclick on a dense table cannot stop somebody else's apps.
- **AC-19**: Both admin surfaces behave identically, because both call one use case: the admin page and the JSON admin route stop and restore the same apps and write the same audit rows.
- **AC-20**: The admin page reads Suspend and Restore, and the column reads Suspended. The confirmation sentence says the account's apps will stop serving.
- **AC-21**: A suspension never ends on its own. There is no expiry, no timer, and no path back except an admin restoring it.
- **AC-22**: A suspended app keeps its Ingress, Service, Secret, network policies, namespace, and slug. A visitor gets whatever the ingress answers with no pods behind it, and the slug is never freed for someone else.
- **AC-23**: An app deleted while its owner is suspended still deletes, and the reaper is unaffected. Suspension gates callers, never the platform's own cleanup.

## Decision

**Chosen option**: Option 1: extend the existing disable into a full suspension, scaling to zero, held by a sweep.

Suspension is `accounts.disabled_at` doing more: the same admin action that revokes credentials now scales the account's live apps to zero replicas, restore scales them back to one, a sweep holds suspended accounts at zero, and a suspended token is refused with the closed reason code `account_suspended` on every agent surface.

**Implementation skills**: `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`) · `security-patterns` (`~/.claude/skills/security-patterns/`)

## Feature design

**Data model sketch**:

No schema change. Suspension is a column that has existed since the first migration.

| Entity | What this feature reads or writes | Why nothing is added |
|---|---|---|
| `accounts` | `disabled_at`, written by the existing `SetAccountDisabled` | One lockout state. A second column would mean two gates in the auth path and two ways for them to disagree |
| `apps` | `account_id`, `slug`, `deleted_at`, `current_release_id` | The set of apps to stop is a query, not a stored list. Nothing records what was stopped, because restore recomputes the same set |
| `deployments` | `failure_reason`, already a closed reason code | An in flight deployment ends through the existing failure path with a new code, not a new state |
| `audit_log` | new rows only | The action set already has `admin`; the app rows use the existing `app` target type |

Deliberately no `suspended_apps` table. Recording which apps were stopped would let restore bring back exactly that set, but it also creates a list that can go stale against reality, and the recomputed set is the correct one anyway: an app deleted while suspended should not come back.

**State transitions**:

Account: `active → suspended` (an admin suspends) and `suspended → active` (an admin restores). No other transition exists, and neither is automatic.

Per app, driven by the account's transition: `serving (1 replica) → stopped (0 replicas)` and back. This is a replica count on the live Deployment, not a stored app state, so nothing new can drift.

Deployment, when its account is suspended mid flight: whatever non terminal state it is in `→ failed`, reason `account_suspended`, through the existing failure write.

**API surface**:

| Surface | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `POST /admin/accounts/{id}/disable` (page) | POST | `confirm_email`, CSRF token | the admin page, with a message naming any app that did not stop | admin session | unchanged, plus a partial stop message |
| `POST /admin/accounts/{id}/enable` (page) | POST | CSRF token | the admin page | admin session | unchanged |
| the JSON admin disable and enable routes in `internal/httpapi` | POST | as today | as today | admin bearer | unchanged |
| every MCP tool | tool call | unchanged | unchanged | bearer token | `account_suspended` (new), refused before the handler |
| `POST /uploads` | POST | unchanged | unchanged | bearer token | 403 with `account_suspended` (new) |
| the app's own hostname | any | none | whatever the ingress answers with no pods | none | not this platform's concern |

Nothing new is exposed. Suspension has no agent surface at all: there is no tool that suspends, reports suspension, or asks about another account.

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| suspend, restore | the target account | the path value, already read and checked by `adminSession` and the typed email confirmation |
| suspend, restore | the lockout write | the existing `identity.Service.SetDisabled`, unchanged, which revokes sessions and links in its own transaction |
| suspend, restore | the apps to stop or start | a new store read, `ListLiveAppsByAccount(account_id)`, returning id and slug where `deleted_at IS NULL AND current_release_id IS NOT NULL` |
| suspend, restore | the namespace to write into | `deploy.NamespaceName(slug)`, the same composer the deploy path uses. Never stored, never composed by hand |
| suspend, restore | the Deployment to scale | `deploy.WorkloadName`, the existing constant |
| restore | the replica count to restore to | the constant `1` in `internal/deploy`, exported for this purpose so the two places cannot drift. Not a stored per app number, because no per app number exists |
| suspend, restore | which apps failed | the per app error from `ScaleWorkload`, collected rather than returned on the first failure, and returned as `Result{NotStopped []string}` from the use case. Both surfaces read that field: the page composes its message from it, the JSON route returns it as `apps_not_stopped`. Neither surface parses an error string |
| the sweep | every app to hold at zero | a new store read, `ListLiveAppsOfSuspendedAccounts()`, the same predicate joined to `accounts.disabled_at IS NOT NULL` |
| the sweep | whether that account is still suspended at write time | a per app re-read of `accounts.disabled_at` immediately before the scale down, because the list is a snapshot and a restore can land inside one tick |
| MCP refusal | the account's suspended state | `auth.Authenticate` returning `ErrAccountSuspended`, which requires `ResolveToken` to stop filtering `disabled_at IS NULL` and to return the stamp instead |
| MCP refusal | the one line a caller reads | `domain.ReasonAccountSuspended.Message()`, composed into a `CallToolResult` with `IsError` set by the receiving middleware itself, not returned as a Go error. The middleware sits above the per tool wrapper that normally does that conversion, so it has to compose the result shape by hand |
| MCP refusal | how a suspended caller reaches that middleware at all | `internal/mcp/middleware.go`. `authenticate` answers 401 on every error today; it gains one branch that puts a suspended account into the request context and calls through, so the refusal happens at the protocol layer rather than the transport |
| upload refusal | the status and body | `internal/httpapi`, mapping `ErrAccountSuspended` to 403 with the reason code, beside the existing 401 |
| in flight deployment | whether its account is suspended | the reconcile loop's existing app read, widened with `AccountSuspended bool` from a join on `accounts.disabled_at`, and re-read at each phase boundary rather than carried from the read at entry. Widening `reconcile.App` from `{ID, Slug}` touches the `Apps` interface, its store adapter in `internal/store/reconcileadapter.go`, and every fake in `internal/reconcile`'s suite; the field is read by the drive only and never reaches a composed manifest, because `internal/deploy` composes from `Input`, which does not gain it |
| in flight deployment | the failure written | the existing failure path with `domain.ReasonAccountSuspended`, plus the existing build Job delete |
| the page a person reads on a failed deployment | the plain sentence | `internal/web/reason.go`, a new entry |
| audit rows | the actor, target, and direction | `auth.ActionAdmin` with a reason of `suspend` or `restore` for the account row, and one `app` target row per app with the same reason |

**Key invariants**:
- One suspension state. `accounts.disabled_at` is the whole truth, and every gate reads it rather than a copy.
- The lockout is a database write and lands first. The cluster work is best effort behind it, so a Kubernetes outage can never leave an abuser signed in.
- The sweep only scales down. There is exactly one caller that scales up, the restore path, and it runs only after `disabled_at` is cleared.
- The sweep decides per app, at write time, from a fresh read. Its list is a snapshot and its writes are not instantaneous, so the account's state is confirmed immediately before each write and never assumed from the list.
- The drive checks suspension where it checks the deploy budget. Both are facts that can change under a running drive, so both are re-read at the same points rather than carried from entry.
- Restore never rebuilds. It touches replicas and nothing else, so the running image, the digest, the configuration Secret, and the release history are all untouched.
- Stopping is idempotent in both directions. Scaling a Deployment that is already at zero, or already at one, is a no op, and a missing Deployment is success, so the sweep can run forever without accumulating anything.
- The set of apps is recomputed, never remembered. Suspend and restore both derive it from the same query.
- A suspended token is refused before a handler runs, in one place, so a tool added later inherits the refusal rather than needing to remember it.
- Refusal is a tool result on the tool surface and a status code on the upload surface. It never becomes a transport error on the tool surface, because an agent reads that as a broken connection rather than a decision.
- The browser stays a blank door. Only a caller who already presented a valid credential learns the account is suspended.

**Security model**:
- Suspension is an availability control an admin exercises over another account. Only an admin session or an admin bearer token reaches either surface, through the guards that already exist.
- An admin may suspend any account including another admin, and may not suspend themselves. That single rule already exists and is not extended.
- The typed email confirmation is a confirmation, never an authorization: the authorization is the admin session, and the typed value only proves the admin meant this row.
- Telling an agent `account_suspended` leaks nothing: the caller already holds a valid credential for that account, so it learns only about itself. The anonymous surface, the login form, learns nothing at all.
- Suspension gates callers, not the platform. The reconcile loop, the reaper, and the sweeps keep running against a suspended account's rows, because leaving cleanup blocked behind a suspension is how a suspended namespace outlives its app.
- No regulated data, so no compliance scope is triggered.

**Configuration required**: none. The sweep runs on the existing `DEPLOYER_RECONCILE_INTERVAL`, and the replica count is the existing constant.

**Critical test scenarios**:
- Happy path: suspend an account with two live apps, both Deployments read zero replicas, sessions and tokens stop working, verifies **AC-2**, **AC-3**.
- Restore: restore the same account, both Deployments read one replica, no new deployment or release row exists, and the app serves the same digest, verifies **AC-4**.
- Refusal over the wire: a suspended account's token calls a tool against `Server.Handler()` over `httptest` and gets `account_suspended` as a tool result carrying `IsError`, not an HTTP 401 and not a protocol error, with nothing written, verifies **AC-9**, **AC-10**.
- Upload refusal: the same token posts to the upload endpoint and gets 403 with the code, verifies **AC-11**.
- Credential shapes: an unknown token, a revoked token, and an expired token all still answer the same invalid credential error, verifies **AC-12**.
- Login is not an oracle: a suspended account and a wrong password produce identical responses, verifies **AC-13**.
- Partial stop: the fake clientset refuses one namespace, the account is still suspended and the response names that app, verifies **AC-6**.
- Missing workload: an app whose namespace does not exist is skipped as success on both directions, verifies **AC-5**.
- Sweep: an app scaled back to one behind the platform's back is returned to zero on the next tick, and no app of an active account is ever touched, verifies **AC-7**, **AC-8**.
- In flight: a deployment already past its first phase when the suspension lands ends failed with `account_suspended` and its build Job deleted, proving the check runs at a later phase boundary and not only at drive entry, verifies **AC-14**.
- Sweep against restore: a restore that lands after the sweep has read its list but before it writes leaves the app at one replica, verifies **AC-24**.
- Partial outcome shape: the same partial stop is rendered by both admin surfaces from the same `NotStopped` field, verifies **AC-6**, **AC-19**.
- Audit: one admin row plus one row per app, in both directions, and a not allowed row for an app that failed to stop, verifies **AC-16**.
- Auth/permission: an ordinary session is refused `admin_required` on both routes; an admin suspending themselves is refused, verifies **AC-17**.
- No migration: no file added under `internal/store/migrations/`, and the previous image starts against the same database, verifies **AC-1**.

## Build plan

Ordered as a Tracer Bullet: the thinnest thread that refuses a real suspended caller end to end comes first, then the cluster half, then the safety net, then the surfaces.

1. [x] Add `ReasonAccountSuspended` to `internal/domain/reason.go` with its static line, update the count in the package comment and the `Valid()` doc, and add its plain sentence to `internal/web/reason.go`, satisfies **AC-15**.
2. [x] Split the credential outcomes: drop `AND accounts.disabled_at IS NULL` from `ResolveToken` in `internal/store/queries/accounts.sql`, return the stamp, regenerate with `sqlc generate`, and have `auth.Authenticate` answer the new `ErrAccountSuspended` for a good token on a suspended account while every other case stays `ErrTokenInvalid`. Leave the session query's filter exactly as it is, satisfies **AC-12**, **AC-13**.
3. [x] Refuse every tool in one place, in two edits that only work together: give `authenticate` in `internal/mcp/middleware.go` a branch for `ErrAccountSuspended` that puts the account into the request context and calls through instead of answering 401, and register one receiving middleware on the per request server that intercepts `tools/call` for a suspended account and returns a `CallToolResult` with `IsError` set and the `account_suspended` line, with a nil Go error so it is a tool result and not a protocol error, satisfies **AC-9**.
4. [x] Prove that thread end to end through `Server.Handler()` over `httptest`, following `internal/mcp/ownership_test.go` rather than `callOverTheWire`, so the authentication branch is inside the test and a regression there cannot pass, satisfies **AC-10**.
5. [x] Refuse the upload endpoint with 403 and the code, beside the existing 401, satisfies **AC-11**.
6. [x] Add `ScaleWorkload(ctx, namespace, name string, replicas int32) error` to `internal/kube`, reading the Deployment before writing it and treating not found as success, satisfies **AC-5**.
7. [x] Add the two store reads, `ListLiveAppsByAccount` and `ListLiveAppsOfSuspendedAccounts`, both over `deleted_at IS NULL AND current_release_id IS NOT NULL`, satisfies **AC-3**, **AC-7**.
8. [x] Write the use case in a new `internal/suspend`: `Suspend` and `Restore`, each calling the existing `SetDisabled` first, then scaling every app of the account, collecting per app failures rather than stopping at the first, and returning `Result{NotStopped []string}` so a partial outcome is data both surfaces can render. It writes the account audit row and the per app rows, and declares its own narrow interfaces, so `client-go` and the store stay at the edges, satisfies **AC-2**, **AC-3**, **AC-4**, **AC-6**, **AC-16**, **AC-22**.
9. [x] Point both admin surfaces at it: `internal/web/admin.go` and the JSON admin route in `internal/httpapi`, each rendering `NotStopped` in its own idiom, the page in its message and the route as an `apps_not_stopped` field, and each keeping the typed email confirmation and the self check exactly as they are. Both today model pass or fail only, so both gain the partial success branch, satisfies **AC-6**, **AC-17**, **AC-18**, **AC-19**.
10. [x] Add `SweepSuspended` to `internal/suspend` and tick it from `cmd/deployer` at boot and on `DEPLOYER_RECONCILE_INTERVAL`. It re-reads the account's `disabled_at` immediately before each scale down and skips an account no longer suspended, and it logs and steps over a namespace that refuses, satisfies **AC-7**, **AC-8**, **AC-24**.
11. [x] End an in flight deployment: widen `reconcile.App` with `AccountSuspended` from a join on `accounts.disabled_at`, updating the `Apps` interface, its store adapter, and the package's fakes, then check it at every phase boundary in `run` alongside the existing `overBudget` calls, failing the row with `account_suspended` and deleting the build Job through the existing path. A single read at drive entry is not enough and is the specific bug this step exists to avoid, satisfies **AC-14**.
12. [x] Change the admin page copy to Suspend, Restore, and Suspended, including the confirmation sentence and the partial stop message, satisfies **AC-6**, **AC-20**.
13. [x] Cover the leftovers: a delete of a suspended account's app still works, the reaper is unaffected, nothing expires a suspension, and no migration was added, satisfies **AC-1**, **AC-21**, **AC-23**.

## Consequences

**Positive**:
- The control that exists becomes the control the scope asked for, without a second state to keep consistent.
- Restore is seconds and costs nothing: no build, no push, no new release, and the app comes back on the exact image it was serving.
- A suspended account stops costing the cluster CPU, memory, and egress, which is the point of the control.
- An agent finally gets a real answer instead of a dead token, so a suspended person hears about it from their own tooling.
- Nothing about apps, releases, or the schema moves, so this ships to a running platform with no data work.

**Negative / tradeoffs**:
- Locking somebody out of the browser and stopping their apps are now one action. There is no way to suspend sign in while leaving an app up, and getting that back means the second state this spec declined.
- The cluster half is best effort. Between a failed scale and the next sweep tick, a suspended account's app is still serving, and the honest answer to that is the sweep, not a guarantee.
- `ResolveToken` stops filtering disabled accounts, so the gate that used to be enforced twice, in the query and in Go, is now enforced once in Go. That is a smaller safety margin on the most sensitive path in the platform, and it is why the credential shapes get their own test.
- One more sweep on the reconcile cadence, which means one more query per tick even when nobody is suspended.
- A visitor to a suspended app gets a bare ingress error, not an explanation. Their user has no idea why the site is down.
- Suspension is invisible to the account it applies to until they try something. There is no email, no notice page, nothing.

**Neutral**:
- The closed reason set grows from twenty one codes to twenty two, so the package comment and the `Valid()` doc change with it.
- `internal/web/reason.go` gains an entry, unlike the `config_` and `app_limit_reached` codes, because this one can reach `deployments.failure_reason` through the in flight failure path.
- A new small package, `internal/suspend`, exists so that the browser, the JSON API, and the sweep share one implementation rather than three.
- `internal/kube` gains its first write that is neither a create nor a full spec replacement.
- The word disable survives in the schema and in `SetDisabled`; only the person facing words change.

## Follow-up

- [ ] A suspended person is told nothing. If the platform ever grows an email on suspension or a notice page on the app's hostname, that is its own decision, not a quiet addition here.
- [ ] The sweep queries on every tick whether or not anyone is suspended. If suspensions stay rare and the tick gets faster, gate the query on a cheap count first.
- [ ] Suspension does not stop an account's storage: uploads, releases, and the database rows all stay. If suspension ever needs to reclaim disk, that is a retention decision, not this one.
- [ ] Nothing tests that the two admin surfaces stay in step beyond both calling the use case. If a third surface ever suspends, it goes through `internal/suspend` or the surfaces diverge.
