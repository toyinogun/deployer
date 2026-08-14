# 0014. Stranded deployment recovery: ending a row whose drive died, with the reason that is true

**Date**: 2026-08-14
**Status**: In Progress

## Summary

When a deploy's drive ends without writing the row terminal, the deployment sits in `building` and nothing notices. Today the only thing that eventually ends it is the deploy budget, which fires up to ten minutes later and records `timeout`, even when the build pod failed and said so minutes earlier. This decision adds a cheap check at the top of each reconcile tick: for a row still in `building`, ask the cluster what its build Job actually did, then either end the row with the reason the Job justifies, or hand the row back so the loop resumes it. It adds no configuration, no database column, and no second goroutine.

## Context

A deployment's state is driven by one goroutine that claims a row and drives it to a terminal state. If that drive returns without writing the terminal state, nothing is left behind that knows the row is stale. This is not hypothetical: a lost SQLite write did exactly that in production on 2026-08-14, and the row sat in `building` while the app it belonged to could not be deleted, because `delete_app` refuses an app with a deploy in flight.

Spec 0005 anticipated the shape of this. Its AC-15 notes that a crash between deleting a Job and writing the transition "leaves a non terminal row whose Job is gone, which the existing startup sweep already resolves through its `JobGone` path". That is true, and it is the gap: the sweep runs at startup and nowhere else, so between restarts the only backstop is the budget watchdog in AC-14. The watchdog works, but it is a blunt instrument. It measures elapsed time rather than reality, so it waits out the full `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` and then records `timeout`, which is a worse answer than the one the cluster could have given immediately.

The forces pulling against a fix are all in spec 0005 as well. AC-16 decided that the reconcile loop is the only writer of deployment state, that the budget check runs inside the existing loop rather than a second goroutine, and that one deployment is driven at a time platform wide. AC-17 decided the feature would add no new configuration. Any recovery mechanism has to live inside those, which rules out the obvious move of running the existing `Sweep` on a ticker: `Sweep` calls `Drive` on every non terminal row, and `Drive` waits on builds, so a periodic sweep would hold the single goroutine for minutes and starve the claim.

The cost of not deciding is small but real and recurring. Every lost or interrupted terminal write leaves an app that cannot be deleted for up to ten minutes and a deployment record that blames the wrong thing, which is exactly the kind of misleading history that makes a later incident harder to read.

## Requirements

**User stories**:
- As someone whose deploy died halfway, I want the platform to notice within seconds rather than minutes, so my app is not stuck refusing deletes.
- As someone reading a failed deployment later, I want the recorded reason to be what actually happened, so the history does not send me looking in the wrong place.
- As the person who runs this platform, I want that recovery to cost no new configuration, no schema change, and no second writer of deployment state.

**Acceptance criteria**:

- **AC-1**: At the top of each reconcile tick, before `ClaimNext`, the loop inspects every non terminal deployment in state `building` that carries a `build_job_name`, reading that Job's state from the cluster. It reuses the store query `expireOverdue` already makes, so it adds no store round trip, and it issues at most one cluster read per such row.
- **AC-2**: A row whose Job reports failed, or is gone, is failed with the reason that Job state justifies, `build_failed`, using the same mapping `awaitBuild` already applies. It is never recorded as `timeout` when the Job gave a real answer.
- **AC-3**: A row whose Job reports succeeded is not failed and not driven. Its claim is cleared so the loop adopts it on a later tick and resumes it, because a build that succeeded is work worth finishing rather than throwing away. Clearing the claim clears **both** `claimed_at` and `claimed_by`, because `ClaimNextDeployment` decides what is unclaimed from `claimed_at IS NULL` and would never see a row that had only `claimed_by` cleared.
- **AC-4**: A row whose Job state cannot be read is left exactly as it stands, unchanged and unfailed. A Kubernetes API blip is not evidence that a deploy died, which is the same judgement `awaitBuild` already makes when it keeps polling through a read error.
- **AC-4a**: A row whose Job reports **running** is likewise left exactly as it stands. A drive that died while the Job it started keeps building is the case this most often sees, and the Job is still the thing that will answer for it: the next tick asks again, and the deploy budget bounds the wait if it never resolves. This is a stated decision, not a fall through: `build.JobState` has four values and `JobRunning` is one of them.
- **AC-5**: `ClaimNext` prefers work in the order the platform owes it: the oldest unclaimed `queued` deployment first, and only when there is none, the oldest unclaimed non terminal row in any other state. That is what lets a released row be adopted and resumed by `Drive` from whatever state it is in, without ever letting a stray jump the queue ahead of a fresh deploy. This introduces no new state transition: resuming a row from its current state is exactly what the startup sweep already does.
- **AC-5a**: The release is one conditional write, an `UPDATE` guarded on the row still being in `building`, so a supersession landing between the cluster read and the release cannot be written over. `CreateDeployment` runs on the caller's goroutine, not the loop's, so that window is real.
- **AC-5b**: A release write that fails changes nothing and is not retried inside the tick. The next tick reads the Job state again and reaches the same decision, so the recovery is self healing, which matters because a failed write is the exact class of fault this whole spec exists to survive.
- **AC-6**: The check runs before the budget sweep within the same tick, so a row that is both stranded and past its budget is ended with the reason the cluster gave rather than with `timeout`. The true cause wins whenever one is available. What makes this safe is not the ordering alone: `Transition` already refuses to write over a row something else ended, surfacing `ErrTerminal`, which the loop treats as a race to stop on rather than a fault. So even a budget pass working from a listing taken before the check wrote does nothing rather than failing the row a second time. That guard is load bearing here and must not be removed as redundant.
- **AC-7**: The reconcile loop remains the only writer of deployment state, on one goroutine, driving one deployment at a time, preserving spec 0005 AC-16. The check itself never calls `Drive` and never blocks on a build.
- **AC-8**: No new `DEPLOYER_*` setting and no migration. The check's cadence is the existing reconcile tick, and clearing a claim writes an existing column.
- **AC-9**: A row that is adopted, stranded, and released repeatedly is still ended by the deploy budget measured from `created_at`, so no adoption counter is needed and spec 0005 AC-14 and AC-14a are unchanged.

## Options considered

### Option 1: Run the existing `Sweep` on a ticker

Call `Sweep` periodically from `Run`'s select loop, the way `ReapOrphanNamespaces` is already called on `ReapInterval`.

**Pros**:
- Smallest possible diff, one ticker and one case in an existing select.
- Reuses a path already proven at startup.

**Cons**:
- `Sweep` calls `Drive` on every non terminal row, and `Drive` waits on builds, so a sweep can hold the single goroutine for minutes and starve `ClaimNext`. This is the thing spec 0005 AC-16 was written to prevent.
- It drives queued rows without going through `ClaimNext`, bypassing the claim that makes one at a time true.
- It needs a new interval setting, against AC-17's grain.

### Option 2: A cheap liveness check folded into the existing tick

At the top of the tick, over the non terminal rows the budget pass already lists, ask the cluster about the build Job of anything still in `building`. Fail it, release it, or leave it alone, and never drive it.

**Pros**:
- Fits inside spec 0005 AC-16 exactly: same goroutine, same pass, no new writer, and it cannot block because it never drives.
- No new configuration, no schema change, no extra store query.
- Recovers with the reason the cluster actually gives, which is the point.

**Cons**:
- Covers `building` only. A row stranded in `pushing` or `deploying` still waits for the budget.
- Adds a cluster read per stranded row per tick, though in the normal case there are none.
- Leans on the control plane being a single pod. That is true today and enforced by the manifest, but it becomes a load bearing assumption rather than an incidental one.

### Option 3: Claim expiry with a heartbeat

Add `claimed_at`, have a drive heartbeat while it works, and treat a row whose claim went stale as adoptable.

**Pros**:
- The most precise notion of "nothing is driving this", and the only one that survives more than one control plane replica.
- Independent of what kind of evidence the current phase happens to leave behind, so it covers every state rather than just `building`.

**Cons**:
- A migration, a heartbeat write on a hot path, and a staleness threshold to pick and tune, all to answer a question the single pod deployment already answers for free.
- The heartbeat writes are the same contended writes that caused the incident this spec exists to fix.

### Option 4: Leave it to the deploy budget

Change nothing. The watchdog already ends every stranded row within the budget.

**Pros**:
- No work, no new risk.

**Cons**:
- Up to ten minutes of an app that refuses deletes, on the default budget.
- Records `timeout` when the real cause was known and different, leaving misleading history.

## Decision

**Chosen option**: Option 2: a cheap liveness check folded into the existing tick.

For a deployment still in `building`, the reconcile tick asks the cluster what its build Job did, then ends the row with the reason that answer justifies, or clears its claim so the loop adopts and resumes it. `ClaimNext` widens to adopt the oldest unclaimed non terminal row rather than only a queued one.

**Implementation skills**: `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`)

## Rationale

The constraint that decides this is spec 0005 AC-16, which is not a style preference but the reason the platform's state machine is easy to reason about: one writer, one goroutine, one deployment at a time. Option 1 breaks it in practice even though it looks like it respects it, because holding the goroutine inside `Drive` is exactly how the claim gets starved. Option 2 respects it by construction, because the check performs bounded reads and makes one decision per row without ever entering a drive.

The reason Option 2 can be this small is an invariant the deployment already gives us. The control plane runs `replicas: 1` with `strategy: Recreate`, so a second control plane pod never overlaps with the first, and `Drive` runs synchronously on the same goroutine as the tick. Therefore any row observed in `building` at the top of a tick is, by construction, not being driven by anybody: the drive that put it there has already returned. That is what makes claim timestamps unnecessary, and it is why Option 3's machinery buys nothing today. It also means the invariant must be written down rather than assumed, because the day someone scales the control plane past one replica, this check would start ending deployments another pod is driving.

Covering `building` only is a deliberate trade rather than an oversight. A build Job is a cheap, unambiguous witness that the platform already knows how to read, and building is where a deployment spends nearly all of its time, so this catches almost every real occurrence. Extending to `pushing` and `deploying` would need registry and workload evidence that is both more expensive and easier to misread, and misreading `deploying` risks ending a rollout that is genuinely progressing. Those phases keep the budget as their backstop, which is no worse than today.

Releasing a successful build rather than failing it follows from the same logic that makes the startup sweep resume rather than discard. A build that succeeded is real work, and `Drive` already knows how to pick a row up from its current state. Widening `ClaimNext` is what lets the existing loop do that, and it introduces no new transition, which keeps the state machine's shape intact.

## Feature design

**Data model sketch**: unchanged. No new table, no new column. The check reads `state`, `build_job_name`, `claimed_by` and `created_at`, all of which exist, and writes `claimed_by` back to null when it releases a row.

**State transitions**: no new transitions. The check either drives a row to `failed` through the existing `Transition` path, or leaves the state untouched and clears the claim. Resuming a released row uses the same `Drive` entry the startup sweep uses.

**Job state to action mapping** (the whole decision table, so no case falls through a default arm):

| `build.JobState` | Action | Why |
|---|---|---|
| `JobFailed` | fail the row `build_failed` | the Job answered, and the answer is a failure |
| `JobGone` | fail the row `build_failed` | nothing is behind the row any more |
| `JobSucceeded` | clear the claim, leave the state | real work worth resuming, not discarding |
| `JobRunning` | no change | the build is alive; the next tick asks again and the budget bounds it |
| read error | no change | absence of an answer is not evidence |

**API surface**: none. This is internal to the reconcile loop and changes no MCP tool, HTTP route or page.

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| Select rows to check | the candidate set | the non terminal listing `expireOverdue` already makes, narrowed to `state = building` with a non null `build_job_name` |
| Check a row | the Job's state | a cluster read of the Job named `build.JobName(dep.ID)`, in the namespace derived from `dep.BuildPath` per spec 0005 AC-19 |
| End a stranded row | the failure reason | the Job state, through the same mapping `awaitBuild` uses: failed or gone becomes `build_failed` |
| Release a row | the cleared claim | `claimed_at` and `claimed_by` both set to null, on existing columns. `claimed_at` is the one that matters: it is what `ClaimNextDeployment` tests |
| Adopt a released row | which row is next | `ClaimNext` ordering: oldest unclaimed `queued` row by `id`, and only if there is none, oldest unclaimed non terminal row by `id`. The column is `id`, not `created_at`; ULIDs order by time already |
| Resume a released row | the phase it resumes from | the row's own `state`, read by `Drive`, exactly as the startup sweep does |
| Bound a flapping row | when to give up | the deploy budget from `created_at`, spec 0005 AC-14 and AC-14a, unchanged |

**Key invariants**:
- The reconcile loop is the only writer of deployment state, one goroutine, one deployment at a time (spec 0005 AC-16, preserved).
- The control plane runs exactly one pod. `replicas: 1` plus `strategy: Recreate` in `deploy/deployment.yaml` is what makes "a `building` row at the top of a tick is not being driven" true. Raising the replica count invalidates this check and must not be done without revisiting this spec. `Recreate` is a rollout guarantee rather than a fencing one: if a node stops answering and Kubernetes force deletes the pod object while that process is still running and still driving a deployment, two processes can act on the same row. That failure already existed under spec 0005's single writer assumption, and this check inherits it rather than introducing it.
- New work is never overtaken by recovery. A stray row is adopted only when there is no queued deployment waiting, so a row that keeps stranding cannot monopolise the single worker ahead of fresh deploys.
- A row is only ever ended on positive evidence from the cluster. Absence of an answer is never evidence.
- A deployment something else already ended is not a fault: a check that finds its row terminal stops quietly rather than writing over it, per the platform rule in `AGENTS.md`.

**Security model**: unchanged. No new caller facing surface, no new permission. The check reads Jobs in the build namespaces the loop already reads.

**Configuration required**: none. This is the point of AC-8.

**Critical test scenarios**:
- Happy path: a row left in `building` whose fake Job reports failed is ended as `build_failed` on the next tick, not `timeout`, verifies **AC-1**, **AC-2**.
- Happy path: a row left in `building` whose fake Job is gone is ended as `build_failed`, verifies **AC-2**.
- Resume case: a row left in `building` whose fake Job reports succeeded has its claim cleared, is adopted by the next `ClaimNext`, and is driven on to a terminal state, verifies **AC-3**, **AC-5**.
- Failure case: a fake clientset that errors on the Job read leaves the row exactly as it was, in `building`, unfailed, verifies **AC-4**.
- Failure case: a row whose Job reports running is left in `building`, unfailed and still claimed, verifies **AC-4a**.
- Fairness: with one released `building` row and one newer `queued` row both unclaimed, `ClaimNext` returns the queued one first even though the released row is older, verifies **AC-5**.
- Race: a supersession that lands between the Job read and the release leaves the superseded row terminal, and the release writes nothing, verifies **AC-5a**.
- Store: clearing a claim clears `claimed_at`, and the widened `ClaimNext` then adopts that row when no queued work exists. A test that only asserts `claimed_by` would pass against a broken implementation, verifies **AC-3**, **AC-5**.
- Failure case: a row that is both stranded and past its budget is ended with `build_failed` rather than `timeout`, which pins the ordering, verifies **AC-6**.
- Failure case: a row whose Job succeeded but which keeps being stranded is still ended by the budget from `created_at`, verifies **AC-9**.
- Invariant: while a deployment is being driven, the check writes no state for it, and the loop still drives one at a time, verifies **AC-7**.

## Build plan

Ordered as a thin thread first, per the project's Tracer Bullet approach: make one stranded row recover end to end before widening the cases.

1. Widen `ClaimNext` to fall back to the oldest unclaimed non terminal row when no `queued` row is waiting, keeping queued work first. Store tests: a queued row wins over an older released `building` row, and the released row is adopted once the queue is empty. Satisfies **AC-5**.
2. Add releasing a claim to the store as one conditional write, clearing `claimed_at` and `claimed_by` where the row is still `building`, so a supersession in the window is not written over. Satisfies **AC-3**, **AC-5a**.
3. Add the check to the tick, ahead of the budget pass, driving the full Job state table: failed or gone to `build_failed`, succeeded to a released claim, running or unreadable to no change. The cadence is the existing tick and the listing is the one already in hand, so nothing here adds a setting or a column. Satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-4a**, **AC-5b**, **AC-6**, **AC-7**, **AC-8**.
4. Cover the scenarios above against the fake clientset, including the ordering test that proves the true reason beats `timeout`, the running and read error tests that prove a live build is left alone, and the fairness test that proves a stray never overtakes queued work. Satisfies **AC-1** through **AC-7**, **AC-9**.
5. Record the single replica invariant where it is enforced rather than only where it is relied on: a note in `deploy/AGENTS.md` beside the manifest, so raising `replicas` is visibly a decision that touches this spec. Satisfies **AC-7**.
6. Confirm on the cluster with `/check verify`: strand a real deployment by killing its build Job mid build, and watch the overview end it with `build_failed` within a tick or two rather than at the budget. The fake clientset cannot prove this one, since it resolves nothing and fails nothing on its own. Satisfies **AC-1**, **AC-2**.

## Consequences

**Positive**:
- A stranded deployment is ended within seconds instead of up to the full deploy budget, so an app stops refusing deletes almost immediately.
- The recorded reason becomes the true one, which makes deployment history worth reading during an incident.
- A build that succeeded is resumed rather than discarded, so an interrupted deploy can still finish.
- No new configuration, no migration, and no second writer, so the operational surface does not grow.

**Negative / tradeoffs**:
- `pushing` and `deploying` are not covered and still rely on the budget. This is a known gap, taken knowingly, and recorded in Follow-up.
- One cluster read per stranded `building` row per tick. In the normal case there are none, so the common cost is a filter over rows already in hand, but a pathological case with many stranded rows would read once per row every two seconds.
- The single replica deployment becomes load bearing rather than incidental. Scaling the control plane past one pod would make this check able to end deployments another pod is actively driving.
- Widening `ClaimNext` means a row can now be adopted by a fresh drive at any non terminal state, so `Drive` must stay safe to resume from every one of them. It is today, since that is what the startup sweep depends on, but it is now depended on from a second place.
- `ClaimNext` gains a second, conditional query path (queued first, strays only when idle). That is more logic in the one query the whole loop's fairness rests on, and its ordering now has to be tested rather than assumed.
- A stray row on a permanently busy platform may not be adopted until the queue empties. Its budget still ends it, and a genuinely broken row is failed by the check without needing adoption at all, so only the resumable case waits.

**Neutral**:
- This amends spec 0005 rather than replacing it: AC-15's note that the startup sweep resolves a stranded row is now only half the story, AC-16's tick gains a step while keeping its guarantee, and AC-14's watchdog stays exactly as it is but stops being the first thing to notice.
- The startup `Sweep` stays as it is. It still resumes everything after a restart, and this check is the between restarts complement.

## Follow-up

- [ ] Decide whether `pushing` and `deploying` deserve the same treatment, and what evidence would be trustworthy enough for `deploying` in particular, where a rollout in progress must not be mistaken for a stranded one.
- [ ] Revisit this spec if the control plane ever needs more than one replica. That change turns Option 3's claim expiry from unnecessary machinery into the only correct answer.
- [ ] Consider whether the same reasoning applies to a rollback left in flight, which shares the drive path.
