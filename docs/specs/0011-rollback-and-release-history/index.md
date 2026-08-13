# 0011. Rollback and release history

**Date**: 2026-08-13
**Status**: In Progress

## Summary

An agent can now list an app's recent releases and roll back to one of them. A release is one image plus the exact configuration it was running with, recorded the moment a deploy became healthy, so going back re promotes the stored image without building anything and restores that configuration too. Two new MCP tools do it: `list_releases` reads the newest twenty releases, and `rollback_app` starts a rollback for a release number the caller names. A rollback that fails never becomes the app's known good state.

## Requirements

**User stories**:
- As an agent that just broke an app, I want to see which releases were known good so that I can choose one to go back to without guessing at image digests.
- As an agent, I want a rollback to restore the whole app, both its image and its environment variables, so that what comes back is a system that actually worked rather than an old image beside today's configuration.
- As an agent, I want a rollback to be fast, so that recovering from a bad deploy does not mean waiting out another build.
- As the platform owner, I want a failed rollback to leave the app's recorded known good release alone, so that one bad recovery attempt cannot poison the history.

**Acceptance criteria**:

- **AC-1**: `list_releases` takes the app's `name` and returns that app's newest twenty releases, newest first. Each row carries `release_number`, `image_digest`, `created_at`, `current`, and `deployment_id`. There is no caller supplied limit and no cursor.
- **AC-2**: Exactly one row in a non empty listing has `current` true: the release `apps.current_release_id` points at. An app whose pointer is null (never healthy) has no such row.
- **AC-3**: `list_releases` on an app that exists and is owned by the caller but has no releases returns an empty list successfully, not a refusal.
- **AC-4**: `list_releases` never reads `releases.config_snapshot`. It reads through its own query projecting only `id`, `release_number`, `image_digest`, `deployment_id`, and `created_at`, so the snapshot never enters the process, and no configuration key or value can appear in its response. The existing `SELECT *` read stays for the callers that need the whole row.
- **AC-5**: `list_releases` for a name that does not exist, and for an app belonging to another account, is refused with `app_unknown` and the same message. The two cases are indistinguishable to the caller.
- **AC-6**: `rollback_app` takes the app's `name` and a required `release_number`, writes a queued rollback deployment, and returns straight away with `deployment_id`, `state` `"queued"`, `name`, `slug`, and `url`. It does not wait for the rollout.
- **AC-7**: A `release_number` that does not exist for that app, or that is not a positive integer, is refused with `release_unknown`. Nothing is written: no deployment row, no supersession, no configuration change.
- **AC-8**: `rollback_app` for a name that does not exist, and for an app belonging to another account, is refused with `app_unknown`, decided before the release number is looked at.
- **AC-9**: A rollback deployment carries `source_release_id` set, `upload_id` null, and its `image_digest` copied from the source release at creation rather than from a push.
- **AC-10**: A rollback supersedes whatever deployment was in flight for that app, cancelling it with reason `superseded` in the same transaction, exactly as a `deploy_app` call does. A later deploy or rollback supersedes a rollback in flight the same way.
- **AC-11**: The reconcile loop drives a rollback from `queued` straight to `deploying`. No build Job is created, no fetch token is minted, no upload is read, and no registry digest resolution runs. Its timeline holds `queued`, `deploying`, `healthy` and nothing else.
- **AC-12**: A rollback composes the app's configuration Secret, and the pod template checksum spec 0010 added, from the source release's config snapshot rather than from current `app_config`. The checksum change is what rolls the pods when the image digest is unchanged.
- **AC-13**: On success, `app_config` is rewritten to exactly the snapshot's keys, in the same transaction that marks the deployment healthy: a key in the snapshot takes the snapshot's value, and a key not in the snapshot is removed. `get_config` afterwards agrees with what is running.
- **AC-14**: Each restored key's `is_secret` flag comes from the snapshot when the snapshot carries one. A snapshot written before this feature carries no flag, and every key restored from such a snapshot is marked secret.
- **AC-15**: New snapshots record `{value, secret}` per key, on every deploy and not only on a rollback. `Apps.ConfigForDeploy` and `store.MarkHealthy` carry the flag alongside the value rather than the bare `map[string]string` they take today, because a bare map has no flag to record and a snapshot written from one would silently be an old shape snapshot forever. Reading accepts both shapes, decided per key, with no migration and no schema change.
- **AC-16**: A healthy rollback mints a new release with the next `release_number` for that app, the same `image_digest` as its source release, and the snapshot it actually deployed with. The source release row is untouched.
- **AC-17**: A rollback that fails leaves the deployment `failed` with its reason, mints no release, leaves `apps.current_release_id` unchanged, and leaves `app_config` unchanged. The platform attempts no undo of the cluster objects it already applied.
- **AC-18**: A rollback naming the release that is already current runs as a normal rollback and is not refused. It re promotes the same digest and rewrites the Secret from the snapshot.
- **AC-19**: A rollback runs under the same deploy budget as a build deploy. No new `DEPLOYER_*` variable and no second timeout constant is added.
- **AC-20**: An allowed `rollback_app` writes an `audit_log` row for the action with outcome `allowed`; a refused one writes `denied` with the reason code, matching how the existing tools audit.
- **AC-21**: `release_unknown` is a closed reason code in `internal/domain/reason.go` carrying its own one line message, covered by `Reason.Valid`. The set becomes nineteen codes.
- **AC-22**: Both tool descriptions carry their contract: `list_releases` says the listing is the newest twenty and that older releases are not reachable through MCP; `rollback_app` says the call does not wait, that it replaces both the image and the app's configuration, and that it supersedes a deploy in flight.
- **AC-23**: Both tools are exercised end to end through a real MCP client and server session in `internal/mcp/wire_test.go`, so a refusal that the argument schema would catch first cannot pass the suite as a reason code.
- **AC-24**: `reconcile.Deployment` carries `SourceReleaseID`, populated by `ClaimNext`, `ListNonTerminal`, and the resume sweep. `Drive` branches on it before it reads an upload, so a rollback never resolves an upload id, never removes a tarball, and a control plane restart that picks a rollback back up resumes it as a rollback rather than failing it as a build deploy with a missing upload.
- **AC-25**: A `set_config` that commits during a rollback's readiness wait is reverted when that rollback succeeds. `MarkHealthy` replaces the whole configuration set from the snapshot with no version check and no conflict signal to either caller. This is specified behaviour rather than a race to close, and `rollback_app`'s description says so.

## Decision

**Chosen option**: Option 2: Two tools over the release table, with configuration restored inside `MarkHealthy`.

Expose `list_releases` and `rollback_app` as separate MCP tools, address a rollback target by its per app `release_number`, let a rollback supersede anything in flight, and restore the release's configuration into both the running Secret and the `app_config` table, the table write happening only in the transaction that marks the rollback healthy.

**Implementation skills**: `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`)

## Rationale

Reasoning, the options weighed, and why each was rejected: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

No migration. Every table and column this feature needs was designed in spec 0002 and already exists.

| Table | Column | Role in this feature |
|---|---|---|
| `releases` | `release_number` | The per app integer a caller names. Unique per app, never reused |
| `releases` | `image_digest` | What a rollback re promotes, copied onto the deployment at creation |
| `releases` | `config_snapshot` | JSON, the configuration that release ran with. Format changes shape, not type (below) |
| `releases` | `deployment_id` | Unique, the deployment that minted this release, returned by the listing |
| `apps` | `current_release_id` | Which release is serving. Moved only by `MarkHealthy` |
| `deployments` | `source_release_id` | Set on a rollback, null on a build deploy. CHECK pairs it with `upload_id` |
| `deployments` | `image_digest` | On a rollback, filled at creation from the source release |
| `app_config` | `key`, `value`, `is_secret` | Rewritten to the snapshot when a rollback succeeds |

**Config snapshot format change** (no migration, the column is JSON text):

```
old, written before this feature:   {"DATABASE_URL": "postgres://..."}
new, written from now on:           {"DATABASE_URL": {"value": "postgres://...", "secret": true}}
```

Decoding is per key, not per document: unmarshal into `map[string]json.RawMessage`, then for each value try a bare string first and fall back to the object. A bare string is an old snapshot's key and is restored as secret. No envelope, no version field, no rewrite of existing rows.

**Go contracts that change** (no schema change, but the build cannot satisfy the criteria above without these):

| Contract | Today | After |
|---|---|---|
| `reconcile.Apps.ConfigForDeploy` | returns `map[string]string` | returns value plus `is_secret` per key, since the flag is dropped today and a snapshot has nowhere to get it |
| `store.MarkHealthy` | takes `config map[string]string` | takes the same value plus flag map, so the release it mints records the real flags on every deploy, not only a rollback |
| `reconcile.Deployment` | has no field naming the deployment's kind | gains `SourceReleaseID`, set by `ClaimNext`, `ListNonTerminal`, and the sweep |
| `mcp.App` | `ID`, `Slug`, `Name` | gains `CurrentReleaseID`, which is the only source for the listing's `current` flag |
| `store` release reads | `ListReleasesByApp` is `SELECT *` | a second, narrow query for the listing, projecting five columns and never the snapshot |

**State transitions**:

A rollback uses the existing deployment state machine and takes a shorter path through it:

```
build deploy:  queued → building → pushing → deploying → healthy
                                                       ↘ failed
rollback:      queued →                       deploying → healthy
                                                       ↘ failed
either:        any non terminal → cancelled  (superseded by a later deploy or rollback)
```

`domain.CanTransition` already permits `queued → deploying`, so nothing in the state machine changes. What does change is where the loop branches, and it is earlier than "skip a phase":

- `Drive` resolves the upload and removes the tarball unconditionally today, both outside `run`. A rollback has a null `upload_id`, so an unbranched `Drive` fails it as `upload_invalid` before any phase check is reached. The branch therefore goes at the top of `Drive`, not inside `run`.
- `run` dispatches on state alone, and a claimed rollback is `queued`, the same state a build deploy starts in. So the `queued` block becomes an explicit two way branch (rollback moves straight to `deploying`; anything else calls `startBuild`), not a fall through of the existing phase checks.
- The resume sweep rebuilds a `reconcile.Deployment` from the store, so the new `SourceReleaseID` field has to be populated there too, or a restart mid rollback resumes it as a build deploy.

**API surface**:

| Tool | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|
| `list_releases` | `name`: string (req) | `releases[]`: `{release_number, image_digest, created_at, current, deployment_id}`, newest first, at most 20 | bearer, account scoped | `app_unknown` |
| `rollback_app` | `name`: string (req), `release_number`: integer (req) | `name`, `slug`, `url`, `deployment_id`, `state` (always `"queued"`) | bearer, account scoped | `app_unknown`, `release_unknown`, `internal` |

Neither tool takes an account: the server is built per request bound to the caller's account, so an account can never arrive as an argument. Progress and outcome are read back through the existing `deployment_status`, which already reports a rollback correctly because a rollback is an ordinary deployment row.

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| `list_releases` | `release_number`, `image_digest`, `created_at` | `releases` columns, via `store.ListReleasesByApp` |
| `list_releases` | `deployment_id` | `releases.deployment_id` column |
| `list_releases` | `current` | true when the row's `id` equals `apps.current_release_id`, carried on the new `mcp.App.CurrentReleaseID` field. The app is already read for the ownership check, so this costs no extra query |
| `list_releases` | the twenty row bound | `MaxReleaseListing`, a Go constant in `internal/mcp`, not `DEPLOYER_*` configuration. It is a product decision about an agent's context window, the same rule the bounds in `internal/logs` follow |
| `list_releases` | the empty list on a never healthy app | `store.ListReleasesByApp` returning no rows, which is not an error |
| `rollback_app` | the release id `source_release_id` is set to | resolved from (`app_id`, `release_number`) by a new store read; the caller never sends an id |
| `rollback_app` | `image_digest` on the new deployment | copied from the source release inside `store.CreateDeployment`, which already does this |
| `rollback_app` | `url` | `Server.appURL(slug)` from `DEPLOYER_APP_DOMAIN`, the same source `deploy_app` uses |
| `rollback_app` | `state` | always the literal `"queued"`: the handler observes and never acts, so there is nothing else it could truthfully report |
| rollback drive | whether this deployment is a rollback at all | `reconcile.Deployment.SourceReleaseID`, the new field, read by `Drive` before it resolves an upload and by `run` inside the `queued` block. It is not inferable from the state, because a rollback and a build deploy are both `queued` when claimed |
| rollback drive | the repo half of the `repo@digest` the workload is given | `build.ImageRepo(DEPLOYER_REGISTRY_HOST, app.Slug)`, recomputed rather than stored. Slugs are permanent (spec 0002), so it is deterministic. `releases` has no repo column and gains none. `resolveImage` is the only thing that fills `deployments.image_repo` today, and a rollback skips it |
| rollback drive | the configuration the Secret and checksum are composed from | `releases.config_snapshot` of `deployments.source_release_id`, read once in `deployApp`. A build deploy still reads `app_config` |
| either drive | each key's `is_secret` when a release is minted | `Apps.ConfigForDeploy` for a build deploy and the source snapshot for a rollback, both now carrying the flag through `MarkHealthy` rather than losing it at the map boundary |
| rollback drive | each restored key's `is_secret` | the snapshot's `secret` field when present; true when the snapshot is the old bare string shape |
| rollback drive | the release the rollback itself mints | `MarkHealthy`, given the same snapshot map `deployApp` composed with, which is the existing `MarkHealthy` contract |
| refusal | the message a caller reads | `domain.Reason.Message`, never a wrapped error string |

**Key invariants**:

- A deployment is unambiguously a build deploy or a rollback, never both and never neither. The schema CHECK on `upload_id` and `source_release_id` enforces it; nothing in Go may relax it.
- A release records what actually ran. A rollback's release snapshots the configuration `deployApp` composed the Secret from, which for a rollback is the source release's snapshot, so the same map reaches `MarkHealthy`. This is spec 0010's invariant, unchanged in wording and now with a second path through it.
- `apps.current_release_id` moves only inside `MarkHealthy`. A failed rollback therefore cannot replace a healthy current release, and neither can a failed deploy.
- `app_config` is rewritten only inside the same transaction, so stored configuration and the running app never disagree because a rollback failed halfway.
- A release row is immutable once written. A rollback reads a snapshot and never updates one.
- Release numbers are never reused. A rollback to release 3 mints release 5; the history is append only and reads as what happened, not as a pointer that moved.
- A refusal a caller sees is one of the closed reason codes. `release_unknown` answers a bad number, a number for another account's app, and a non positive number with the same words.
- No user supplied value is merged into a pod spec. A rollback composes the same manifests field by field that a deploy does.

**Security model**:

- Both tools are account scoped through the per request server, the same as every existing tool. There are no roles: an account either owns an app or has no visibility of it.
- Ownership is decided before anything else. `rollback_app` resolves the app and refuses with `app_unknown` before it looks at `release_number`, so a caller cannot probe which release numbers exist on somebody else's app.
- A release number for an app the caller does own but that has no such release is `release_unknown`. Within an owned app there is nothing to hide, so the code can be precise.
- `list_releases` is a read that must not leak configuration. Its own query projects five named columns, so the snapshot is never loaded into the process at all. The existing `ListReleasesByApp` is `SELECT *` and stays that way for the callers that need the whole row; the listing does not reuse it, because "the handler remembers not to serialize a field it is holding" is a much weaker guarantee than never holding it.
- The snapshot holds secret values in clear, which spec 0002 already accepted and this feature does not widen. The one new reader is the reconcile loop composing a Secret, which is where secret values already flow.
- Restoring a key with an unknown flag defaults to secret. A key wrongly marked secret hides a value `get_config` used to show; the reverse leaks one. The safe direction is the one that does not leak.
- Audit: both an allowed rollback and every refusal write an `audit_log` row, so the log records the actions that replaced what was running, not only the ones that were denied.

**Configuration required**:

None. No new `DEPLOYER_*` variable. The twenty row listing bound is a Go constant, and the deploy budget is the existing one.

**Critical test scenarios**:

- Happy path: deploy an app, set configuration, deploy again with different configuration, then roll back to release 1. The new deployment carries release 1's digest, walks `queued`, `deploying`, `healthy` in three events, mints release 3 with release 1's digest and snapshot, and `get_config` reports release 1's configuration. Verifies **AC-6**, **AC-9**, **AC-11**, **AC-13**, **AC-16**.
- Listing bound and shape: an app with twenty five releases returns twenty rows, newest first, with `current` true on exactly one and no configuration anywhere in the payload. Verifies **AC-1**, **AC-2**, **AC-4**.
- Configuration actually rolls the pods: the rollback's pod template checksum differs from the running one even though the image digest is identical, so the Deployment rolls. Verifies **AC-12**.
- Old snapshot compatibility: a release whose snapshot is the old bare string shape restores every key marked secret, and a release written after this feature restores each key's recorded flag. Verifies **AC-14**, **AC-15**.
- Failure case: a rollback whose pods never become ready ends `failed`, mints no release, and leaves both `current_release_id` and `app_config` exactly as they were. Verifies **AC-17**.
- Concurrency: a rollback issued while a build deploy is in flight cancels that deploy with `superseded` and starts, in one transaction. Verifies **AC-10**.
- Concurrency, the other direction: a `set_config` that commits between the rollback's Secret being applied and `MarkHealthy` running is gone afterwards, and `get_config` reports the snapshot. Verifies **AC-25**.
- Restart mid rollback: a rollback left in `deploying` by a killed control plane is picked up by the sweep, still recognised as a rollback, and finishes without resolving an upload or creating a build Job. Verifies **AC-24**, **AC-11**.
- Refusal, unknown release: `rollback_app` with `release_number` 99 on an app with three releases returns `release_unknown` through a real MCP session, and no deployment row exists afterwards. Verifies **AC-7**, **AC-21**, **AC-23**.
- Auth/permission: another account's app name returns `app_unknown` from both tools, with the same message a name that never existed gets, and an `audit_log` denial is written. Verifies **AC-5**, **AC-8**, **AC-20**.

## Build plan

The project builds by Tracer Bullet, so the order below drives one rollback end to end through every layer before any of it is widened. Tasks 1 to 7 are that thread: name a release, write the row, teach the loop the deployment's kind, skip the build, apply the old image with the old configuration, become healthy. Configuration fidelity, the listing, and the refusals thicken it afterwards. There is no migration in this feature; spec 0002 already shipped the schema, and the contract changes in tasks 8 and 10 are Go signatures, not columns.

1. Add `release_unknown` to `internal/domain/reason.go` with its message, and update the package comment's count to nineteen. Satisfies **AC-21**.
2. Add `store.GetReleaseByNumber(ctx, appID, number)` returning `ErrNotFound`, and expose it through the MCP store adapter. Satisfies **AC-7**.
3. Add the `rollback_app` tool: resolve the app (`app_unknown`), resolve the release number (`release_unknown`), call the existing `store.CreateDeployment` with `SourceReleaseID`, return the queued output. Audit both outcomes. Satisfies **AC-6**, **AC-8**, **AC-9**, **AC-10**, **AC-18**, **AC-20**.
4. Add `SourceReleaseID` to `reconcile.Deployment` and populate it in `ClaimNext`, `ListNonTerminal`, and the resume sweep, so every path that builds one knows the deployment's kind. Satisfies **AC-24**.
5. Branch `Drive` on it ahead of the upload read and removal, and make `run`'s `queued` block an explicit two way branch that moves a rollback straight to `deploying` instead of calling `startBuild`. Satisfies **AC-11**, **AC-19**, **AC-24**.
6. Have `deployApp` compose a rollback's image reference from `build.ImageRepo(registryHost, app.Slug)` plus the digest already on the row, since `resolveImage` never runs to fill `image_repo`. Satisfies **AC-11**.
7. Have `deployApp` read the source release's snapshot instead of `app_config` for a rollback, and pass that same configuration to `MarkHealthy`. Satisfies **AC-12**, **AC-16**.
8. Widen `Apps.ConfigForDeploy` and `store.MarkHealthy` to carry `is_secret` beside each value, and change the snapshot encoding to `{value, secret}` per key with a per key decoder that reads an old bare string as a secret key. Satisfies **AC-15**, **AC-14**.
9. Rewrite `app_config` from the snapshot inside `MarkHealthy`'s transaction, replacing the whole set, only on a rollback. The flags come from the decoded snapshot, which the transaction already holds. Satisfies **AC-13**, **AC-17**, **AC-25**.
10. Add a narrow `ListReleaseSummariesByApp` query projecting `id`, `release_number`, `image_digest`, `deployment_id`, `created_at`, and add `CurrentReleaseID` to `mcp.App` and its adapter. Satisfies **AC-4**, **AC-2**.
11. Add the `list_releases` tool over that query, bounded by the `MaxReleaseListing` constant, marking the current release. Satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-5**.
12. Write both tool descriptions as contract, covering the twenty row bound, that a rollback replaces image and configuration together, that it does not wait, and that configuration set while it runs is reverted. Satisfies **AC-22**, **AC-25**.
13. Extend `internal/mcp/wire_test.go` to drive both tools through a real client and server session, including the `release_unknown` and `app_unknown` refusals. Satisfies **AC-23**, and closes the loop on every reason code this feature adds.

## Consequences

**Positive**:
- Recovery stops depending on a rebuild. Rolling back re promotes a stored digest, so it costs a rollout rather than the several minutes a cold build takes.
- A release finally means the whole app. The snapshot stops being a column nothing reads and becomes the thing a rollback restores, which is what spec 0002 designed it for.
- No migration and no new configuration. Every table, column, constraint, and index this needs already shipped, which is exactly the bet spec 0002 made when it put the whole schema in one migration.
- The reason set stays closed and grows by one. A caller still gets a code and a line, never a wrapped error.
- `get_config` and the running app cannot disagree after a rollback, because the table write and the release both land in one transaction.

**Negative / tradeoffs**:
- A rollback overwrites configuration the caller set since that release, without asking. That is what a rollback means, but an agent that set a key an hour ago and then rolls back loses it with no warning beyond the tool description.
- Release history through MCP is capped at twenty and has no way to reach past it. An app with a long history has older releases that exist in the database and are unreachable by any tool. Rolling back to release 3 of two hundred needs a database read by hand.
- Keys restored from a pre existing snapshot all come back marked secret, so `get_config` stops showing values it used to show for those apps. It is the safe direction, but it is a real and silent behaviour change for apps deployed before this feature.
- A failed rollback leaves the cluster mid change: old image applied, pods not ready, nothing restored. The caller has to act again. Choosing no automatic undo means the platform never digs a deeper hole, at the cost of not getting out of the first one by itself.
- Configuration set while a rollback is in flight is reverted, and the call that set it already returned success. The readiness wait is exactly the window AGENTS.md says a `set_config` can land in, and `MarkHealthy` replaces the whole set with no version check. Neither caller is told. This is the honest cost of the snapshot winning, but it is a call that succeeded and then silently did not.
- The reconcile loop branches on deployment kind in three places, not one: `Drive` before it reads an upload, `run` inside the `queued` block, and `deployApp` for both the image repo and the configuration source. Until now every deployment was driven identically, and that is no longer true.
- Two contracts widen to carry `is_secret`: `Apps.ConfigForDeploy` and `store.MarkHealthy`. That touches the build deploy path, which has no rollback in it, so a feature about rollback changes code every deploy runs through.

**Neutral**:
- The snapshot format now has two shapes in the same column, decided per key at read time. It is a few lines and needs no migration, but anything that ever reads a snapshot has to go through the one decoder rather than unmarshalling the column directly.
- Release numbers keep climbing through rollbacks. Rolling back to 1 from 4 produces 5, so the numbers are a log rather than a version, and a reader who expects them to be versions will misread the history.
- `deployment_status` needs no change: a rollback is an ordinary deployment row, so its timeline, reason codes, and `superseded_by` already work.

## Follow-up

- [ ] Spec 0010's open item is closed by this spec: a rollback rewrites the Secret from the release snapshot and the pod template checksum is what rolls the pods. Mark it done there when this feature ships.
- [ ] The twenty row listing bound leaves older releases unreachable through MCP. If that bites, the answer is the web console in feature 14 reading the same store method with paging, not a cursor on the MCP tool.
- [ ] Nothing tests that a tool description matches the behaviour it promises, and this feature adds two more descriptions carrying contract. That gap is now four tools wide and worth its own decision.
- [ ] Restoring pre existing snapshots as all secret is a one way door for those apps. Consider whether a later `set_config` re declaring a key as plain is enough of an escape hatch, or whether the platform should say so in the rollback's response.
