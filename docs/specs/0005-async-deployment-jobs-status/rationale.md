# 0005. Async deployment jobs and status: rationale

The decision record for [index.md](index.md): the problem, the options weighed, and why this one.

## Context

Spec 0004 deliberately built `deploy_app` as a blocking call. The handler writes a `queued` deployment, then polls committed state every couple of seconds until the row is terminal, and returns the success payload or a reason code. That was the right shape for a tracer bullet, because it proved the whole pipe with one tool and no readback surface at all. Spec 0004 also wrote down what it cost, in its own Follow-up: until a status tool exists, a client that times out first leaves the agent with no way to learn the outcome of a deploy that is still running.

That cost is not theoretical. A first build with no cache runs for minutes, because Cloud Native Buildpacks resolve and compile from cold every time. MCP clients apply their own request timeouts, and the platform does not control them. So there are two clocks: the platform's `DEPLOYER_DEPLOY_TIMEOUT_SECONDS`, default ten minutes, and whatever the client enforces, which may be a minute. When the client's clock wins, the deployment carries on perfectly happily and the agent has nothing, no id, no state, no way back. The person waiting is told the deploy failed, when it very likely succeeded.

There is a second force underneath. Because the caller was the only thing timing a deploy, the platform had no deadline of its own for the whole. Individual phases are bounded, the build Job by `activeDeadlineSeconds` and the readiness wait by its own timeout, but a deployment stuck in `queued` or `pushing` had nothing counting against it. The blocking call was accidentally covering that: when the caller gave up, at least somebody had. Take the caller away and the gap is visible.

Everything needed to answer "how is it going" is already being written. The reconcile loop transitions state through the store, so every move leaves a `deployment_events` row; `deployments` carries the reason, the digest, and the lifecycle stamps; `releases` carries the number. Nothing reads any of it back except the handler's own poll loop, which throws it away. The question this spec answers is therefore not how to track a deploy, it is what surface exposes what the platform already knows, and who owns the clock once no caller does.

## Options considered

### Option 1: Return the id immediately, add a polling status tool, move the deadline into the loop

`deploy_app` keeps everything up to writing the `queued` row and returns the id plus the hostname it will get. A second tool, `deployment_status`, reads `deployments` and `deployment_events` and reports state, reason, release, and the timeline. The reconcile loop gains one condition: a deployment older than the deploy budget is failed with `timeout`.

**Pros**:

- The client's request timeout stops mattering. Every call is milliseconds, and the outcome is durable and readable afterwards from an id the agent already holds.
- It is almost entirely a read. No new state, no new table, no new dependency, no new cluster object.
- The invariant that the reconcile loop is the only writer of deployment state survives, because the deadline is a condition inside the loop rather than a second actor.
- The queue stops being invisible. A deploy waiting its turn is a `queued` state a caller can see, not a hung connection.

**Cons**:

- The agent has to poll, and nothing compels it. A deploy whose status is never asked for is a deploy nobody learns the outcome of, which the blocking call at least forced.
- Two round trips minimum for the common case where a build is quick.
- Two tool descriptions now carry one workflow between them, so they can drift from each other as well as from the endpoints.

### Option 2: Keep the call blocking, add MCP progress notifications

The call stays open and streams progress notifications as the deployment moves, which a capable client renders as a live status.

**Pros**:

- One call, no polling, and the nicest experience where the client supports it.
- No new tool and no new read surface at all.

**Cons**:

- It does not solve the actual problem. A client timeout still ends the call, and the agent is still left with nothing, because there is no id to come back with. Progress notifications make waiting pleasant; they do not make it survivable.
- Support varies by client, so the platform would be relying on a capability it cannot check.
- It leaves the platform with no deadline of its own, because the caller is still the thing timing a deploy.

### Option 3: Job id plus an optional bounded wait parameter

As Option 1, but `deploy_app` takes an optional capped `wait_seconds` so a fast deploy can return finished in one call.

**Pros**:

- One call for the common quick case, and the escape hatch is still there for a slow one.
- Strictly more capable than Option 1.

**Cons**:

- Two code paths through the same tool, and the interesting one, the long build, is the one the parameter does not help.
- The blocking path has to be kept working, tested, and reasoned about forever, for a saving of one round trip.
- It invites a caller to pass a large value and reintroduce exactly the problem being fixed.

## Rationale

Option 1, because the problem in Context is that the outcome is not durable, and only a readable id fixes that. Option 2 is a better waiting experience for a call that should not be waiting; it leaves an agent whose client timed out in exactly the position spec 0004 complained about. Option 3 is Option 1 plus a path that helps least where it hurts most, and every future change to the deploy tool would have to be correct on both paths.

The deadline goes inside the reconcile loop rather than into a watchdog goroutine of its own for one specific reason from Context: the loop being the only writer of deployment state is a load bearing invariant, and it is what makes the restart sweep and the serial claim reasonable to think about. A second goroutine that can fail a row would need to coordinate with a loop that is mid phase on that same row, and the coordination is the bug. Checking the budget where the loop already checks state costs nothing and keeps one writer.

That invariant has a price, and it is worth stating exactly rather than glossing. The loop drives one deployment at a time on one goroutine, and a drive blocks for as long as a build takes, so a per tick sweep does not fire while a drive is in progress. Two enforcement points follow, and neither is redundant. The tick sweep, one cheap query before the claim, catches every non terminal row the loop is not currently holding, which is the case a queued deployment starving behind a long build falls into. The phase boundary check catches the row the loop is holding, which is the only row the sweep cannot reach. Together they mean no row outlives its budget by more than the drive ahead of it, and that drive is itself bounded by the same budget. A separate goroutine would tighten that to the instant the budget passes, at the cost of the one writer rule, which is not a trade worth making at a ten minute budget.

The same reasoning is why `Drive`'s existing timeout is replaced rather than left in place. It already builds a full budget window, but from when the drive started rather than from when the deployment was created, so a deployment resumed after a restart quietly received a second full budget. Deriving the remaining time from the row's age fixes that and removes the second, disagreeing clock, rather than adding a third.

Measuring the budget from `created_at` rather than from `claimed_at` follows from one build at a time. A deployment that is never claimed would otherwise have no deadline at all, which is the exact failure the watchdog exists to close. Queue time counting against the budget is the honest tradeoff, and it is stated in the Consequences rather than hidden, because it becomes wrong the moment build concurrency changes.

Two smaller calls worth recording. `superseded_by` is derived rather than stored because the unique in flight index already guarantees at most one non terminal deployment per app, so the app's next deployment after a cancelled one is the one that cancelled it; a column would be a migration to persist something the schema already implies. The ordering is by `id` rather than `created_at` because ids are ULIDs and therefore monotonic, while two rows can carry the same timestamp string, particularly under the injected clock the tests use. And `deployment_events.detail` is dropped at the projection rather than sanitized at each write site, because there is one projection and an unbounded number of future write sites, so the boundary belongs where it can be held.
