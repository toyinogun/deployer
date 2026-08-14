# 0014. Stranded deployment recovery: ending a row whose drive died, with the reason that is true

**Date**: 2026-08-14
**Status**: Accepted

## Summary

A deployment can end up sitting in `building` with nothing driving it, and then only the deploy budget ends it, ten minutes later and with the wrong reason recorded. This decision adds a cheap check at the top of each reconcile tick: for a row still in `building`, ask the cluster what its build Job actually did, then either end the row with the reason the Job justifies, or hand the row back so the loop resumes it. It adds no configuration, no database column, and no second goroutine.

Be clear about how narrow that is, because the first draft of this spec was not. The everyday cases are already covered by code that predates this check. A control plane restart is resolved by the startup `Sweep`, which drives every in flight row before the first tick runs. A build Job that dies while the process is up is resolved by `awaitBuild`, which is polling that same Job and reaches the same answer. What is left, and what this check is actually for, is the pair of faults where a row is in `building` and genuinely nobody is driving it: a state write that did not land, and a startup sweep that did not run. Both are silent today and both cost a full deploy budget.

## Requirements

**The faults this exists for**, established by reading every exit from `Drive`, which claims in its own comment that all of them are terminal:

| Fault | What happens today | Does this check help |
|---|---|---|
| A `fail()` write that does not land: `Transition` errors on a busy or broken database, the reason was computed and then lost, and the process carries on | the row sits in `building` until the deploy budget, then records `timeout` | yes, this is the core case |
| A startup `Sweep` whose listing fails: `ListNonTerminal` errors, `Sweep` logs one warning and returns, and the ticker starts with every in flight row unattended | every in flight row sits until its budget | yes, and here the check is the only recovery there is |
| A control plane restart | `Sweep` drives every in flight row before the first tick | no, already covered, and the check never gets to see the row |
| A build Job that fails or vanishes while the process is up | `awaitBuild` is polling that Job and returns `build_failed` itself | no, already covered |
| Two processes at once, from a force deleted pod on an unresponsive node | undefined, both drive the same row | no, and worse: see AC-11 |

**User stories**:
- As someone whose deploy was stranded by a database write that failed, I want the platform to notice on the next tick rather than at the budget, so my app is not stuck refusing deletes for ten minutes over a fault that lasted a second.
- As someone reading a failed deployment later, I want the recorded reason to be what actually happened, so the history does not send me looking in the wrong place.
- As the person who runs this platform, I want that recovery to cost no new configuration, no schema change, and no second writer of deployment state.

**Acceptance criteria**:

- **AC-1**: At the top of each reconcile tick, before `ClaimNext`, the loop inspects every non terminal deployment in state `building` that carries a `build_job_name`, reading that Job's state from the cluster. It reuses the store query `expireOverdue` already makes, so it adds no store round trip, and it issues at most one cluster read per such row. In the ordinary case that list is empty and the check is a filter over rows already in hand: the two faults above are the only ones that put a row in front of it.
- **AC-2**: A row whose Job reports failed, or is gone, is failed with the reason that Job state justifies, `build_failed`, using the same mapping `awaitBuild` already applies. It is never recorded as `timeout` when the Job gave a real answer.
- **AC-3**: A row whose Job reports succeeded is not failed and not driven. Its claim is cleared so the loop adopts it on a later tick and resumes it, because a build that succeeded is work worth finishing rather than throwing away. Clearing the claim clears **both** `claimed_at` and `claimed_by`, because `ClaimNextDeployment` decides what is unclaimed from `claimed_at IS NULL` and would never see a row that had only `claimed_by` cleared.
- **AC-4**: A row whose Job state cannot be read is left exactly as it stands, unchanged and unfailed. A Kubernetes API blip is not evidence that a deploy died, which is the same judgement `awaitBuild` already makes when it keeps polling through a read error.
- **AC-4a**: A row whose Job reports **running** is likewise left exactly as it stands. A drive that died while the Job it started keeps building is the case this most often sees, and the Job is still the thing that will answer for it: the next tick asks again, and the deploy budget bounds the wait if it never resolves. This is a stated decision, not a fall through: `build.JobState` has four values and `JobRunning` is one of them.
- **AC-5**: `ClaimNext` prefers work in the order the platform owes it: the oldest unclaimed `queued` deployment first, and only when there is none, the oldest unclaimed non terminal row in any other state. That is what lets a released row be adopted and resumed by `Drive` from whatever state it is in, without ever letting a stray jump the queue ahead of a fresh deploy. This introduces no new state transition: resuming a row from its current state is exactly what the startup sweep already does.
- **AC-5a**: The release is one conditional write, an `UPDATE` guarded on the row still being in `building`, so a supersession landing between the cluster read and the release cannot be written over. `CreateDeployment` runs on the caller's goroutine, not the loop's, so that window is real.
- **AC-5b**: A release write that fails changes nothing and is not retried inside the tick. The same holds, deliberately, when the check's own `fail()` write fails: the row stays in `building` and the next tick reads the Job and reaches the same decision. That is the check recursing into the exact fault it exists for, and it is correct rather than circular, because each attempt is independent and the budget still bounds the row. The next tick reads the Job state again and reaches the same decision, so the recovery is self healing, which matters because a failed write is the exact class of fault this whole spec exists to survive.
- **AC-6**: The check runs before the budget sweep within the same tick, so a row that is both stranded and past its budget is ended with the reason the cluster gave rather than with `timeout`. The true cause wins whenever one is available. What makes this safe is not the ordering alone: `Transition` already refuses to write over a row something else ended, surfacing `ErrTerminal`, which the loop treats as a race to stop on rather than a fault. So even a budget pass working from a listing taken before the check wrote does nothing rather than failing the row a second time. That guard is load bearing here and must not be removed as redundant.
- **AC-7**: The reconcile loop remains the only writer of deployment state, on one goroutine, driving one deployment at a time, preserving spec 0005 AC-16. The check itself never calls `Drive` and never blocks on a build.
- **AC-8**: No new `DEPLOYER_*` setting and no migration. The check's cadence is the existing reconcile tick, and clearing a claim writes an existing column.
- **AC-9**: A row that is adopted, stranded, and released repeatedly is still ended by the deploy budget measured from `created_at`, so no adoption counter is needed and spec 0005 AC-14 and AC-14a are unchanged.
- **AC-10**: The release write reports whether it actually released anything, and the loop logs the two outcomes apart. `ReleaseBuildingClaim` is guarded on the row still being in `building`, so it legitimately matches zero rows when a supersession beat it there, and today both outcomes log as a success. The interface becomes `ReleaseClaim(ctx context.Context, id string) (bool, error)`, reporting whether a row was released rather than how many: the loop only ever compares the count to zero, so the count stays inside the store where sqlc already produces it. The change touches the `Deployments` interface and its doc comment in `internal/reconcile`, `ReconcileStore.ReleaseClaim`, `Store.ReleaseBuildingClaim`, and every existing caller and test. Without this, AC-5a's race is a thing tests prove and production never shows.
- **AC-11**: This check is correct only while exactly one control plane process runs. Under two processes it is not merely unhelpful, it is wrong: a row the other process is actively driving looks identical to a stranded one, and the check would end it. `replicas: 1` with `strategy: Recreate` is therefore a precondition of this spec rather than a deployment detail, recorded in `deploy/AGENTS.md` beside the manifest that enforces it. Raising the replica count means removing this check or building the claim expiry that Option 3 weighed, and it may not be done as a scaling decision alone.

## Decision

**Chosen option**: Option 2: a cheap liveness check folded into the existing tick.

For a deployment still in `building`, the reconcile tick asks the cluster what its build Job did, then ends the row with the reason that answer justifies, or clears its claim so the loop adopts and resumes it. `ClaimNext` widens to adopt the oldest unclaimed non terminal row rather than only a queued one.

Kept after the reachability finding, deliberately and with the claims cut down. `/check verify` could not trigger the check on the cluster and `/check review` reached the same conclusion independently: the everyday stranding stories belong to `Sweep` and `awaitBuild`. What survives that is a backstop for two silent faults that each cost a full deploy budget today, at the price of a filter over a listing already in hand. That is worth keeping, but it is not the feature the first draft described, and the wording above is the correction rather than a defence.

**Implementation skills**: `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`)

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
- The control plane runs exactly one pod. `replicas: 1` plus `strategy: Recreate` in `deploy/deployment.yaml` is what makes "a `building` row at the top of a tick is not being driven" true. Raising the replica count invalidates this check and must not be done without revisiting this spec (AC-11). `Recreate` is a rollout guarantee rather than a fencing one: if a node stops answering and Kubernetes force deletes the pod object while that process is still running and still driving a deployment, two processes can act on the same row. Spec 0005's single writer assumption is already broken in that case, but this check makes the consequence worse rather than inheriting it unchanged: before, two drives raced over the same row and one lost; now one process can end a deployment the other is successfully building. The honest statement is that the check trades correctness under two processes for recovery under one, knowingly, because one is what runs.
- New work is never overtaken by recovery. A stray row is adopted only when there is no queued deployment waiting, so a row that keeps stranding cannot monopolise the single worker ahead of fresh deploys.
- A row is only ever ended on positive evidence from the cluster. Absence of an answer is never evidence.
- A deployment something else already ended is not a fault: a check that finds its row terminal stops quietly rather than writing over it, per the platform rule in `AGENTS.md`.

**Security model**: unchanged. No new caller facing surface, no new permission. The check reads Jobs in the build namespaces the loop already reads.

**Configuration required**: none. This is the point of AC-8.

**Critical test scenarios**:
- Reachability, the case the check exists for: a store whose `Transition` fails leaves a row in `building` after a drive, and the next tick ends it with the reason its Job gives. This is the fault the whole spec rests on and nothing tests it today, verifies **AC-1**, **AC-2**.
- Reachability, the second case: a store whose `ListNonTerminal` fails on the startup `Sweep` and succeeds afterwards leaves in flight rows unattended, and the tick recovers them. This proves the check is the only recovery when the sweep does not run, verifies **AC-1**.
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
- Store: a release whose guard matches no row reports zero rows affected rather than success, and the loop logs it as not released, verifies **AC-10**.

## Build plan

Ordered as a thin thread first, per the project's Tracer Bullet approach: make one stranded row recover end to end before widening the cases.

1. Widen `ClaimNext` to fall back to the oldest unclaimed non terminal row when no `queued` row is waiting, keeping queued work first. Store tests: a queued row wins over an older released `building` row, and the released row is adopted once the queue is empty. Satisfies **AC-5**.
2. Add releasing a claim to the store as one conditional write, clearing `claimed_at` and `claimed_by` where the row is still `building`, so a supersession in the window is not written over. Satisfies **AC-3**, **AC-5a**.
3. Add the check to the tick, ahead of the budget pass, driving the full Job state table: failed or gone to `build_failed`, succeeded to a released claim, running or unreadable to no change. The cadence is the existing tick and the listing is the one already in hand, so nothing here adds a setting or a column. Satisfies **AC-1**, **AC-2**, **AC-3**, **AC-4**, **AC-4a**, **AC-5b**, **AC-6**, **AC-7**, **AC-8**.
4. Cover the scenarios above against the fake clientset, including the ordering test that proves the true reason beats `timeout`, the running and read error tests that prove a live build is left alone, and the fairness test that proves a stray never overtakes queued work. Satisfies **AC-1** through **AC-7**, **AC-9**.
5. Record the single replica invariant where it is enforced rather than only where it is relied on: a note in `deploy/AGENTS.md` beside the manifest, so raising `replicas` is visibly a decision that touches this spec. Satisfies **AC-7**.
6. ~~Confirm on the cluster with `/check verify`: strand a real deployment by killing its build Job mid build.~~ Attempted 2026-08-14 and abandoned as unprovable: killing the Job while the drive is alive is answered by `awaitBuild`, and killing the control plane pod is answered by the startup `Sweep`, both confirmed on the cluster by log line rather than by outcome. There is no external way to strand a row on a healthy single pod cluster, because both faults that do it are internal. Replaced by steps 7 and 8.
7. Surface the release write's affected row count through the store and log the two outcomes apart, so a supersession that beat the release is visible in production rather than only in a test. Satisfies **AC-10**.
8. Test the two faults the check actually exists for: a `Transition` that errors, leaving a row in `building` for the next tick to end, and a `ListNonTerminal` that errors on the startup sweep and succeeds after. The seam is a test type in `internal/reconcile` that embeds the real `store.ReconcileStore`, delegates every call to it, and fails one named call once. Real SQLite still answers everything else, and no store semantics are faked, which is what the never mock the store rule protects: the rule is there so a test cannot pass against store behaviour that does not exist, and a passthrough that returns a real error on one call invents no behaviour at all. Note the reason in the test file, because the next reader will check it against the rule. Forcing a genuine SQLite failure instead does not work: `Tick` reads `ListNonTerminal` before the check and skips the check when that read fails, so a broken connection fails the read the test needs to succeed. Satisfies **AC-1**, **AC-2**.
9. Rewrite `verify.md` around what can be shown: drop the two cluster steps, keep the replica and configuration checks, and let the fault injection tests carry **AC-1** and **AC-2**. Satisfies **AC-11**.

## Consequences

**Positive**:
- A deployment stranded by a failed state write, or by a startup sweep that did not run, is ended at the top of the next tick rather than at the deploy budget, so an app stops refusing deletes far sooner. Those two faults, not the everyday ones the first draft claimed. "The next tick" is seconds away on an idle platform and no sooner than the end of the current drive otherwise, because `Tick` calls `Drive` synchronously and a drive runs for as long as its build does. The recovery is bounded by the active drive, not by the budget, which is the improvement; it is not unconditionally within seconds.
- The recorded reason becomes the true one, which makes deployment history worth reading during an incident.
- A build that succeeded is resumed rather than discarded, so an interrupted deploy can still finish.
- No new configuration, no migration, and no second writer, so the operational surface does not grow.

**Negative / tradeoffs**:
- The value is much smaller than a reader of the first draft would have believed, and the code carries machinery (a widened `ClaimNext`, a conditional release, a decision table) sized for the larger story. Two faults justify it. Someone reading `recoverStranded` cold will overestimate what it does unless the comments keep saying what this spec now says.
- It cannot be verified against a real cluster at all. Both triggering faults are internal, so the proof is fault injection in tests and nothing else, which is exactly the class of proof this project's rules distrust most.
- `pushing` and `deploying` are not covered and still rely on the budget. This is a known gap, taken knowingly, and recorded in Follow-up.
- One cluster read per stranded `building` row per tick. In the normal case there are none, so the common cost is a filter over rows already in hand, but a pathological case with many stranded rows would read once per row every two seconds.
- The single replica deployment becomes load bearing rather than incidental. Scaling the control plane past one pod would make this check able to end deployments another pod is actively driving.
- Widening `ClaimNext` means a row can now be adopted by a fresh drive at any non terminal state, so `Drive` must stay safe to resume from every one of them. It is today, since that is what the startup sweep depends on, but it is now depended on from a second place.
- `ClaimNext` gains a second, conditional query path (queued first, strays only when idle). That is more logic in the one query the whole loop's fairness rests on, and its ordering now has to be tested rather than assumed.
- A stray row on a permanently busy platform may not be adopted until the queue empties. Its budget still ends it, and a genuinely broken row is failed by the check without needing adoption at all, so only the resumable case waits.

**Neutral**:
- This amends spec 0005 rather than replacing it: AC-15's note that the startup sweep resolves a stranded row is now only half the story, AC-16's tick gains a step while keeping its guarantee, and AC-14's watchdog stays exactly as it is but stops being the first thing to notice.
- The startup `Sweep` stays as it is. It still resumes everything after a restart, and this check is the between restarts complement.

## Rationale

Reasoning, the options weighed, and what the gates found on 2026-08-14: see [rationale.md](rationale.md).

## Follow-up

- [ ] The never mock the store rule, in root `AGENTS.md` and `internal/store/AGENTS.md`, reads as absolute and this spec's step 8 relies on a reading of its intent: no faked semantics, a passthrough over the real store allowed. `/sync` should make that distinction explicit in the rule, or the next reader will treat the test as a violation and delete it.
- [ ] Consider whether a failed startup `Sweep` deserves to be louder than one warning line. It is now a named fault this check recovers from, and today the only sign it happened is a log entry nobody is watching for.
- [ ] Decide whether `pushing` and `deploying` deserve the same treatment, and what evidence would be trustworthy enough for `deploying` in particular, where a rollout in progress must not be mistaken for a stranded one.
- [ ] Revisit this spec if the control plane ever needs more than one replica. That change turns Option 3's claim expiry from unnecessary machinery into the only correct answer.
- [ ] Consider whether the same reasoning applies to a rollback left in flight, which shares the drive path.
