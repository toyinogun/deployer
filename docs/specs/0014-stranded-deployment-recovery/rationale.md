# 0014. Stranded deployment recovery: why

The decision record behind [index.md](index.md): what forced this, what else was on the table, and why the chosen option won.

## Context

A deployment's state is driven by one goroutine that claims a row and drives it to a terminal state. If that drive returns without writing the terminal state, nothing is left behind that knows the row is stale. This is not hypothetical: a lost SQLite write did exactly that in production on 2026-08-14, and the row sat in `building` while the app it belonged to could not be deleted, because `delete_app` refuses an app with a deploy in flight.

Spec 0005 anticipated the shape of this. Its AC-15 notes that a crash between deleting a Job and writing the transition "leaves a non terminal row whose Job is gone, which the existing startup sweep already resolves through its `JobGone` path". That is true, and it is the gap: the sweep runs at startup and nowhere else, so between restarts the only backstop is the budget watchdog in AC-14. The watchdog works, but it is a blunt instrument. It measures elapsed time rather than reality, so it waits out the full `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` and then records `timeout`, which is a worse answer than the one the cluster could have given immediately.

The forces pulling against a fix are all in spec 0005 as well. AC-16 decided that the reconcile loop is the only writer of deployment state, that the budget check runs inside the existing loop rather than a second goroutine, and that one deployment is driven at a time platform wide. AC-17 decided the feature would add no new configuration. Any recovery mechanism has to live inside those, which rules out the obvious move of running the existing `Sweep` on a ticker: `Sweep` calls `Drive` on every non terminal row, and `Drive` waits on builds, so a periodic sweep would hold the single goroutine for minutes and starve the claim.

The cost of not deciding is small but real and recurring. Every lost or interrupted terminal write leaves an app that cannot be deleted for up to ten minutes and a deployment record that blames the wrong thing, which is exactly the kind of misleading history that makes a later incident harder to read.

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

## Rationale

The constraint that decides this is spec 0005 AC-16, which is not a style preference but the reason the platform's state machine is easy to reason about: one writer, one goroutine, one deployment at a time. Option 1 breaks it in practice even though it looks like it respects it, because holding the goroutine inside `Drive` is exactly how the claim gets starved. Option 2 respects it by construction, because the check performs bounded reads and makes one decision per row without ever entering a drive.

The reason Option 2 can be this small is an invariant the deployment already gives us. The control plane runs `replicas: 1` with `strategy: Recreate`, so a second control plane pod never overlaps with the first, and `Drive` runs synchronously on the same goroutine as the tick. Therefore any row observed in `building` at the top of a tick is, by construction, not being driven by anybody: the drive that put it there has already returned. That is what makes claim timestamps unnecessary, and it is why Option 3's machinery buys nothing today. It also means the invariant must be written down rather than assumed, because the day someone scales the control plane past one replica, this check would start ending deployments another pod is driving.

Covering `building` only is a deliberate trade rather than an oversight. A build Job is a cheap, unambiguous witness that the platform already knows how to read, and building is where a deployment spends nearly all of its time, so this catches almost every real occurrence. Extending to `pushing` and `deploying` would need registry and workload evidence that is both more expensive and easier to misread, and misreading `deploying` risks ending a rollout that is genuinely progressing. Those phases keep the budget as their backstop, which is no worse than today.

Releasing a successful build rather than failing it follows from the same logic that makes the startup sweep resume rather than discard. A build that succeeded is real work, and `Drive` already knows how to pick a row up from its current state. Widening `ClaimNext` is what lets the existing loop do that, and it introduces no new transition, which keeps the state machine's shape intact.

