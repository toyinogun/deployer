# 0005. Async deployment jobs and status: the deploy call stops waiting

**Date**: 2026-08-12
**Status**: Accepted

## Summary

Today `deploy_app` holds the MCP call open for the whole build, which can run for minutes, so a client that times out first leaves the agent with no way to learn what happened. This changes the call to return a deployment id straight away, and adds a second tool, `deployment_status`, that reports where that deployment has got to and how it got there. Nothing about how a deploy actually runs changes: the reconcile loop already owns every state transition and `deployment_events` already records every move, so this slice is a new readback surface plus a deadline, not new machinery.

The one genuinely new piece of behaviour is a watchdog inside the loop. With nobody holding a connection open, no caller is counting the seconds any more, so the loop itself now enforces the overall deploy budget and fails a deployment that runs past it.

Reasoning and the options weighed: see [rationale.md](rationale.md).

## Requirements

**User stories**:

- As an AI coding agent, I want the deploy call to come back immediately with an id, so that my client's request timeout has nothing to do with how long a build takes.
- As an AI coding agent, I want to ask how a deploy is going, so that I can tell the person whether to keep waiting or whether it failed.
- As an AI coding agent, I want to know when my deploy was replaced by a later one, so that I stop polling an id that will never finish.
- As the operator, I want a deployment that stalls to end by itself, so that no row sits in flight forever now that no caller is timing it.

**Acceptance criteria**:

- **AC-1**: `deploy_app` returns without waiting for the build. It still validates the upload, resolves or creates the app, writes the `queued` deployment, and writes its one `audit_log` row, then returns. The whole call completes in well under a second on a warm platform, and its duration does not vary with build time.
- **AC-2**: The `deploy_app` response carries `deployment_id`, `name`, `slug`, `url` (the deterministic `https://<slug>.<DEPLOYER_APP_DOMAIN>`), and the current `state`, which is `queued`. It no longer carries `release_number` or `image_digest`, because neither exists yet.
- **AC-3**: The MCP handler no longer reads deployment state after creating the row. The polling wait is deleted, and `DEPLOYER_RECONCILE_INTERVAL_SECONDS` is no longer read by the MCP package.
- **AC-4**: `deploy_app`'s tool description states that the call returns immediately with a `deployment_id`, that the caller must poll `deployment_status` to learn the outcome, and roughly how long a first build takes. The existing upload contract, the `PORT` rule, and the non root rule stay in it unchanged.
- **AC-5**: The MCP server exposes a second tool, `deployment_status`, taking `deployment_id` (optional) and `name` (optional). Exactly one must be given; neither, or both, fails with `deployment_unknown` without reading anything.
- **AC-6**: Called with `deployment_id`, it reports that deployment. Called with `name`, it resolves the caller's app under that name and reports that app's most recent deployment by `created_at`. An app with no deployments, or no app under that name, fails with `deployment_unknown`.
- **AC-7**: The status payload always carries `deployment_id`, `app_name`, `slug`, `url`, and `state`. When the state is `healthy` it also carries `release_number` and `image_digest`. When it is `failed` it also carries `reason` (the code) and `message` (that code's one sanitized line). When it is `cancelled` it carries `reason` `superseded` and `superseded_by`, the id of the deployment that replaced it.
- **AC-8**: The payload carries a `timeline`: one entry per `deployment_events` row for that deployment, in `occurred_at` order, each holding `state` (the row's `to_state`), `at` (`occurred_at`), and `reason` when the row has one. The `detail` column never crosses into a response, at any state.
- **AC-9**: A `deployment_id` that does not exist, and one belonging to another account, both fail with `deployment_unknown` and byte identical wording, so status cannot be used to learn which ids exist. The same holds for a `name` that is unknown versus one belonging to another account.
- **AC-10**: An allowed `deployment_status` call writes no `audit_log` row. A denied one writes exactly one, with action `status`, outcome `denied`, and the reason code. This adds an `ActionStatus` constant to `internal/auth`, which today knows only upload, deploy, and fetch source, and the constant joins the test asserting that the action names are the ones the log is queried by.
- **AC-11**: `internal/domain/reason.go` gains exactly two codes, `deployment_unknown` and `superseded`, each with its own sanitized message and each reported by `Valid()`. Its doc comment, which today says nine failures, is updated to eleven codes and states that `superseded` describes a cancellation rather than a failure. No other code is added, removed, or reworded.
- **AC-12**: When `CreateDeployment` supersedes an in flight deployment, the cancelled row's `failure_reason` column holds `superseded`, not only its event row's reason, so a status read of the cancelled deployment needs no special case.
- **AC-13**: `superseded_by` is derived, not stored: it is the app's next deployment after the cancelled one, ordered by `id`, which is a ULID and so already monotonic (`internal/ids`). Ordering is never by `created_at`, because two rows can share one timestamp. No migration, no new column, no schema change anywhere in this feature.
- **AC-14**: The reconcile loop fails any non terminal deployment whose `created_at` is older than `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` with reason `timeout`. It is enforced in two places, because the loop drives one deployment at a time on one goroutine: a sweep at the top of each tick, before `ClaimNext`, over every non terminal row, using one store query and no cluster calls; and a check at each phase boundary of the deployment currently being driven. The sweep therefore fires between drives rather than during one, and what bounds how long a drive can hold the tick is that drive's own budget check.
- **AC-14a**: `Drive`'s existing `context.WithTimeout` is repointed rather than left beside the new mechanism: its budget is `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` minus the deployment's age at claim time, and a deployment whose budget is already spent is failed with `timeout` without being driven at all. A resumed deployment never gets a fresh full budget.
- **AC-15**: When the watchdog fails a deployment that has a `build_job_name`, it deletes that Job in `DEPLOYER_BUILD_NAMESPACE` first, with background propagation so the pod goes too. This adds `DeleteJob` to the `Cluster` interface in `internal/reconcile` and to `internal/kube`, which have no delete today. A Job that is already gone is not an error and does not stop the deployment being failed. A crash between the delete and the transition leaves a non terminal row whose Job is gone, which the existing startup sweep already resolves through its `JobGone` path.
- **AC-16**: The reconcile loop remains the only writer of deployment state. The budget check runs inside the existing loop, not in a second goroutine, and one deployment is still driven at a time platform wide.
- **AC-17**: `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` keeps its name and its startup validation, and now bounds a deployment from `created_at` to a terminal state rather than bounding one MCP call. No new configuration setting is added by this feature.
- **AC-18**: A tarball is still deleted from `DEPLOYER_UPLOAD_DIR` when its deployment reaches a terminal state, including when the watchdog is what made it terminal (spec 0004, AC-22).
- **AC-19**: From an agent session against the real cluster: `deploy_app` returns in under a second, successive `deployment_status` calls report `queued`, then a building state, then `healthy`, the timeline holds the five transitions in order, and the final payload's `url`, `release_number`, and `image_digest` match what the old blocking call returned for the same app.
- **AC-20**: Deploying the same app twice in a row still supersedes: the first deployment ends `cancelled` with reason `superseded` and a `superseded_by` naming the second, and the second runs to `healthy`.

## Decision

**Chosen option**: Option 1: Return the id immediately and add a polling status tool, with the deadline moved into the reconcile loop.

`deploy_app` keeps everything it does up to writing the `queued` row and then returns. `deployment_status` is a pure read over `deployments` and `deployment_events`, rows the platform is already writing. The loop gains one condition, the budget, evaluated where it already evaluates state, so the invariant that the loop is the only writer survives intact.

**Implementation skills**: `golang-patterns` (`.claude/skills/golang-patterns/`) · `golang-testing` (`.claude/skills/golang-testing/`) · `mcp-server-patterns` (`.claude/skills/mcp-server-patterns/`) · `mcp-builder` (`anthropics/skills`, `.agents/skills/mcp-builder/`) · `senior-kubernetes-engineer` (`.claude/skills/senior-kubernetes-engineer/`)

## Rationale

The options weighed, and why the deadline lives in the loop rather than in a watchdog of its own: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**: no new tables, columns, indexes, or migrations. Spec 0002's schema already carries every value this feature reads. `deployments` holds `state`, `failure_reason`, `image_digest`, `created_at`, `claimed_at`, `started_at`, `finished_at`, and `build_job_name`; `deployment_events` holds the timeline; `releases` holds the number and digest; `deployments_by_app` indexes `(app_id, created_at DESC)`, which is exactly the lookup both the name path and the `superseded_by` derivation need.

**State transitions**: spec 0002's machine, unchanged. No state is added, removed, or given a new trigger. What changes is who ends a deployment when nothing else does:

| Transition | Trigger | New in this spec |
|---|---|---|
| any non terminal to `failed` (`timeout`) | the loop finds `created_at` older than the budget | yes, this is the watchdog |
| any non terminal to `cancelled` (`superseded`) | `CreateDeployment` supersedes it | no, exists; only `failure_reason` is now written too |
| every other arrow | unchanged from spec 0004 | no |

**API surface**:

| Endpoint or tool | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `deploy_app` (MCP) | tool | `name` (required), `upload_id` (required) | `deployment_id`, `name`, `slug`, `url`, `state` | bearer | unchanged from spec 0004, AC-16 |
| `deployment_status` (MCP) | tool | `deployment_id` (optional), `name` (optional), exactly one | `deployment_id`, `app_name`, `slug`, `url`, `state`, `timeline`, plus `release_number` and `image_digest` when healthy, `reason` and `message` when failed, `reason` and `superseded_by` when cancelled | bearer | `deployment_unknown` |
| `/v1/uploads`, `/v1/uploads/{id}`, `/healthz`, `/readyz` | | unchanged | | | |

Both tool descriptions are contract, not decoration (`AGENTS.md`). `deploy_app`'s must now say the call returns straight away and name `deployment_status` as the way to learn the outcome. `deployment_status`'s must say that a first build takes minutes, that polling every few seconds is the intended shape, and that a `cancelled` result means a later deploy replaced this one.

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| `deploy_app` | `deployment_id` | `DeploymentStore.Create`, as today |
| `deploy_app` | `url` | `"https://" + slug + "." + DEPLOYER_APP_DOMAIN`, derived the moment the app row exists, so it is known before anything is built |
| `deploy_app` | `state` | the constant `queued`; the row was just written in that state and the handler does not read it back |
| `deployment_status` | the deployment, by id | `deployments` row by primary key, then its `account_id` compared to the caller's before anything else is read |
| `deployment_status` | the deployment, by name | the caller's `apps` row for `(account_id, name)`, then its most recent `deployments` row by `created_at`, over the `deployments_by_app` index |
| `deployment_status` | `state` | `deployments.state` |
| `deployment_status` | `reason`, `message` | `deployments.failure_reason` mapped through `domain.Reason`, and that code's `Message()`. Never a wrapped error, never `deployment_events.detail` |
| `deployment_status` | `release_number`, `image_digest` | the `releases` row for that deployment, the same read the old blocking success path used. Absent until the deployment is healthy, because `MarkHealthy` mints the release |
| `deployment_status` | `superseded_by` | derived: the app's next deployment after the cancelled one, ordered by `id`, which is a monotonic ULID, never by `created_at`. Empty when none exists, which means either the cancel came from something other than supersession, or the read landed in the moment before the superseding row committed; the next poll resolves it |
| `deployment_status` | `timeline` | `deployment_events` for that deployment in `occurred_at` order, projected to `to_state`, `occurred_at`, and `reason`. `detail` and `from_state` are dropped at the projection, so no write site can leak through them |
| `deployment_status` | `app_name`, `slug`, `url` | the `apps` row the deployment belongs to; `url` recomposed from the slug and `DEPLOYER_APP_DOMAIN`, never stored |
| Watchdog | the deadline | `deployments.created_at` plus `DEPLOYER_DEPLOY_TIMEOUT_SECONDS`, compared against the loop's clock. `reconcile.Deployment` carries neither `created_at` nor `build_job_name` today, so both are added to that struct and mapped in the store's reconcile adapter |
| Watchdog | the Job to delete | `deployments.build_job_name`, in `DEPLOYER_BUILD_NAMESPACE`, through a new `DeleteJob` on the `Cluster` interface. Null means no Job was ever created, so there is nothing to delete |
| Watchdog | the driven deployment's remaining budget | `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` minus the row's age at claim, replacing `Drive`'s current fixed window |
| Watchdog | the reason | the constant `timeout`, an existing code |
| Supersession | the cancelled row's reason | the constant `superseded`, written into `failure_reason` in the same transaction that writes the cancel and its event |

**Key invariants**:

- The reconcile loop is still the only writer of deployment state. The watchdog is a condition inside it, not a second writer.
- One build at a time, platform wide. Unchanged, and now visible: a deploy waiting its turn sits in `queued` where a caller can see it, rather than in a hung connection where nobody could.
- The MCP handler never transitions a row. It writes the `queued` row and, from this slice on, reads nothing back at all.
- Every failure a caller sees is one of the closed reason codes. This feature adds two codes to that set and no other kind of error.
- `deployment_events.detail` is internal. It never appears in a tool response.
- A status answer for an id the caller does not own is identical to the answer for an id that does not exist.
- No deployment can stay non terminal past the deploy budget, so every row reaches a terminal state without anyone watching it. Past it by how much is bounded by the tick, not by zero: the sweep runs between drives, so a queued row's failure can wait for the drive ahead of it, whose own budget check bounds that wait.
- No deployment reaches `healthy` without a digest and a release. Unchanged; `MarkHealthy` still owns that transaction.

**Security model**:

- Both tools authenticate identically, a bearer token on `Authorization`, resolved by the existing middleware. There is still one account and one token until feature 8.
- `deployment_status` is scoped to the caller's account at the first read, before any field is projected. The rule is written now so that feature 8 adds accounts rather than adding a check.
- Unknown and forbidden are the same answer. There is one account today, so this changes nothing observable and costs nothing to write correctly.
- Reads are not audited; denials are. A polling tool audited on every call would fill a table that has no retention sweep, for no signal.
- The failure boundary is unchanged and now has one more surface to hold: build output, cluster messages, and wrapped errors reach neither the status payload nor the timeline. The `detail` projection is the specific defence.
- No regulated data class is in scope, so no compliance regime applies.

**Configuration required**: none new. `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` keeps its name, its default of 600, and its startup validation, and changes what it bounds: a deployment from creation to terminal, rather than one MCP call. Its doc comment in `internal/config` changes with it.

**Critical test scenarios**:

- Happy path, no cluster: a fake clientset plus a real SQLite file drives one deployment to `healthy` while `deployment_status` is called at each phase, asserting the payload's state, the timeline's order and length, and that `release_number` and `image_digest` appear only once healthy. Verifies **AC-6**, **AC-7**, **AC-8**.
- Happy path, real cluster: an agent session deploys `testdata/sample-go`, times the `deploy_app` return, polls to `healthy`, and compares the final payload to spec 0004's blocking response. Verifies **AC-1**, **AC-19**.
- Return shape: `deploy_app` against a store whose reconcile loop is not running still returns in well under a second, carrying the url and `queued`. Verifies **AC-1**, **AC-2**, **AC-3**.
- Supersession: two deploys of the same app, then status on the first id, reports `cancelled`, reason `superseded`, and a `superseded_by` equal to the second id; the cancelled row's `failure_reason` column holds `superseded`. Verifies **AC-12**, **AC-20**, and the cancelled branch of **AC-7**.
- Watchdog: a deployment whose `created_at` is backdated past the budget is failed with `timeout` on the next tick, its Job is deleted from the fake clientset, and its tarball is gone. A backdated deployment whose `build_job_name` is null is failed just the same. Verifies **AC-14**, **AC-15**, **AC-18**.
- Watchdog and the driven deployment: the budget is also enforced on the deployment the loop is currently driving, at a phase boundary rather than only between ticks, and the loop writes no state after failing it. A claimed deployment whose budget is already spent is failed without being driven. Verifies **AC-14**, **AC-14a**, **AC-16**.
- Watchdog and a blocked loop: with one deployment driving long (a fake clientset that never reports the Job complete) and a second app's deployment sitting `queued` past its own budget, the queued one is failed once the drive returns, and the driving one is failed at its own phase boundary. This is the scenario that proves what AC-14's two enforcement points actually cover. Verifies **AC-14**, **AC-14a**.
- Supersession race: a status read of a cancelled deployment taken before its successor's row is visible returns an empty `superseded_by` rather than an error, and the next read carries it. Verifies **AC-7**.
- Auth and permission: status for an unknown id, for another account's id, for an unknown name, and for another account's app all return byte identical errors, each writing one denied `audit_log` row; a successful status call writes none. Verifies **AC-9**, **AC-10**.
- Argument validation: neither `deployment_id` nor `name`, and both together, each fail with `deployment_unknown` and touch no store. Verifies **AC-5**.
- Leak boundary: an event row written with a `detail` holding a raw cluster message does not appear in any field of the status payload. Verifies **AC-8**.
- Reason set: table test over the closed set asserts exactly eleven codes, all `Valid()`, all with a non empty message. Verifies **AC-11**.
- Schema: the store test suite passes with no migration added, and `sqlc generate` produces no diff beyond any new read query. Verifies **AC-13**.

## Build plan

Tracer Bullet, so the new thread is threaded before anything is hardened: the shortest path to an agent deploying without blocking and reading back a real state, then the deadline and the reporting edges.

1. Add `deployment_unknown` and `superseded` to `internal/domain/reason.go` with their messages, correct the doc comment's count from nine to eleven and note that `superseded` is a cancellation, and extend the reason table test. Satisfies **AC-11**.
2. Add the status reads to `internal/store` and the narrow interface they satisfy in the consuming package: a deployment by id, an app's latest deployment, the events list projected without `detail`, and the app's next deployment after a given one, ordered by `id`. No migration. Satisfies **AC-8**, **AC-13**, and the read half of **AC-6**.
3. Add the `deployment_status` tool: argument validation, the `ActionStatus` audit constant, the account scope check, the payload assembly per state, and its description. Satisfies **AC-5**, **AC-6**, **AC-7**, **AC-9**, **AC-10**.
4. Cut the wait out of `deploy_app`: delete the polling loop, return the queued response, drop the poll interval from the MCP options, and rewrite the tool description. Satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**.
5. Write `superseded` into the cancelled row's `failure_reason` in `CreateDeployment`'s transaction, and derive `superseded_by` in the status path. Satisfies **AC-12**, **AC-20**.
6. Add the budget to the reconcile loop. In order: carry `created_at` and `build_job_name` on `reconcile.Deployment` and map them in the store adapter; add `DeleteJob` to the `Cluster` interface and `internal/kube`, treating not found as success; replace `Drive`'s fixed `context.WithTimeout` with the remaining budget computed from the row's age; add the tick sweep over non terminal rows before `ClaimNext`; and confirm the terminal cleanup that already deletes the tarball also runs on this path. Repoint `DEPLOYER_DEPLOY_TIMEOUT_SECONDS`'s meaning and doc comment. Satisfies **AC-14**, **AC-14a**, **AC-15**, **AC-16**, **AC-17**, **AC-18**.
7. Run the real deploy from an agent session, timing the return and polling through to `healthy`, and compare the final payload to spec 0004's. Satisfies **AC-19**.

## Consequences

**Positive**:

- The sharpest edge spec 0004 named is closed. A client timeout is now a client's problem, not a lost deploy, because the outcome is readable afterwards from a durable id.
- The queue becomes visible. One build at a time was always a platform wide serialisation point; it was invisible when waiting meant a hung connection, and it is now a state a caller can see.
- Every deployment now ends. The watchdog closes the one way a row could sit in flight forever, which the blocking call was accidentally covering by giving up.
- No migration, no new configuration, no new dependency, and nothing new in the cluster. The whole slice is Go changes in four packages.
- The status tool is the read path that features 12 and 13 will extend, so releases and app listing land beside a surface that already exists rather than inventing one.

**Negative / tradeoffs**:

- The agent has to poll, and nothing makes it. An agent that calls `deploy_app` and never calls `deployment_status` gets no outcome at all, where the blocking call at least forced the question. The tool descriptions are the only thing carrying that instruction, and nothing tests that they say it.
- Two tools now describe one workflow, so their descriptions can drift from each other as well as from the endpoints. The upload contract already had this problem; this doubles the surface.
- The success response moves. `release_number` and `image_digest` no longer come back from `deploy_app`, so anything reading them from that call breaks in one commit. There is one caller today and it is an agent reading a description, which is why this is a straight cut rather than a versioned one.
- The watchdog measures from creation, so queue time counts against a deployment's budget. A deploy sitting behind a long queue can be failed with `timeout` having never been built. Correct while one build runs at a time and the budget is ten minutes, and the first thing to revisit if concurrency ever changes.
- `superseded` lives in a type whose doc comment says it is why a deployment failed. The comment now has to carry an exception, which is a small honesty cost paid to keep one closed set rather than two.

**Neutral**:

- `superseded_by` is derived rather than stored, so it is only ever as right as the assumption that the next deployment for an app is the one that superseded it. That holds because the unique in flight index permits exactly one non terminal deployment per app.
- The status payload varies by state rather than carrying null fields for everything. It reads better for an agent and it means the shape is not fixed, which a strongly typed client would dislike. There is no such client.
- `DEPLOYER_RECONCILE_INTERVAL_SECONDS` loses one of its two jobs. It still paces the loop; it just no longer paces a handler that no longer polls.

## Follow-up

- [ ] Nothing forces an agent to poll. If deploys start silently going unread, the next step is a progress notification on the still open call, or a webhook, rather than more words in a description.
- [ ] `AGENTS.md` should record that the tool description contract now covers two tools, and that `deployment_events.detail` is internal and never reaches a response. Both are project wide rules once this lands.
- [ ] The watchdog counts queue time against the budget. Revisit if slice 5 or later ever raises build concurrency, and consider measuring from `claimed_at` with a separate queue ceiling.
- [ ] Feature 8 turns the account scope check from a formality into the real boundary. Until then it is enforcing a rule with one subject, exactly as spec 0004 said of ownership.
- [ ] Spec 0004's Follow-up item calling slice 2 not optional is discharged by this spec. Tick it there when this ships.
