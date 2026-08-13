# Rationale: app lifecycle, list and delete · spec 0012

Reasoning and options for [0012](index.md). Not read during a build.

## Context

> ⚠️ Premise note: this topic holds two decisions, what a listing reports and how a teardown works. They stay in one spec because scope feature 13 ships them together, they share the same app row and the same ownership path, and neither is large on its own. If either grows a second surface, split it.

Eleven slices in, an agent can deploy an app, watch it, configure it, read its logs, and roll it back. It cannot see what it has, and it cannot get rid of anything. Every app ever deployed is still running, still holding a hostname, a namespace, a ResourceQuota and a pod. For the case this platform was built for, an agent generating apps it will throw away, that is the missing half of the loop.

Two forces shape the listing. The first is that the platform already has a precise vocabulary for deployment state, seven states in `internal/domain/state.go`, and an obvious cheap move is to reuse it as the app's state. But a deployment state is not an app state. A rolling update keeps the previous pods serving while the new ones start, and a deploy that never becomes healthy leaves the previous release running untouched: that is exactly what spec 0011's rollback safety rests on. So the app whose last deploy failed is, in the ordinary case, an app that is up. Reporting one word for it has to pick which truth to tell.

The second is cost. A listing is the call an agent makes most casually, often just to find a URL. It must not fan out into one database read per app, and it must not fan out into Kubernetes at all, because a listing that fails when the API server is busy is a listing an agent stops trusting.

The teardown has a different shape. An app is not one object; it is a namespace containing a Deployment, a Service, an Ingress, a Secret, two NetworkPolicies, a ResourceQuota, a LimitRange and a RoleBinding, all composed field by field in `internal/deploy`. The database side is already designed: spec 0002 gave `apps` a `deleted_at`, made the slug unique across every row that ever existed so a retired hostname is never handed to someone else, and gave `SoftDeleteApp` a transaction that refuses while a deployment is in flight. The cluster side is not designed at all. The rights exist, the ClusterRole carries `delete` on namespaces and `admission-policy.yaml` fences it to names starting `app-`, but nothing has ever used them.

That leaves the real problem: two systems, one platform rule. The project's rule is that a state transition is a database write before it is an action, which fixes the order and therefore fixes the failure mode. The row goes first, so the failure that can happen is a namespace with no live app row: an app still serving, still holding its hostname, that the platform no longer believes in. Deciding what happens to that orphan is as much of this decision as the delete itself.

## Options considered

### How the state is reported

#### Option 1: One state field, from the newest deployment

A single `state` on each row, taken from the app's newest deployment, plus `never_deployed` when there is none.

**Pros**:
- One field, one vocabulary, identical to `deployment_status`.
- The cheapest possible read and the smallest response.

**Cons**:
- Reports `failed` for an app that is up and serving, which is the single most common interesting case in this listing and the one an agent most needs to get right.
- An agent acting on it would redeploy or panic over an app that needs neither.

#### Option 2: Two independent facts, serving and last deployment (chosen)

`serving` from `apps.current_release_id`, `last_deployment` from the newest deployment row, reported side by side, both read in the same statement. Teardown by a single namespace delete, database first, with an orphan reaper behind it.

**Pros**:
- Tells the truth in the case that actually occurs, without an agent having to reconcile anything.
- Still one query: both facts are a join and a correlated read over indexes that already exist.
- Every field maps to a column, so nothing can go stale.

**Cons**:
- Two fields for an agent to read instead of one, and a tool description that has to explain why.
- `serving` says what the platform recorded as current, not what pods are answering right now; a manually scaled or evicted app can disagree with it.

#### Option 3: Live cluster readiness per app

Read each app's Deployment readiness from Kubernetes at call time.

**Pros**:
- The only option that reports what is genuinely running rather than what was recorded.

**Cons**:
- One cluster call per app, so a listing gets slower exactly as an account gets more apps, which is when it is most needed.
- A busy or unreachable API server turns a cheap read into a failure.
- Introduces a third state vocabulary, neither the deployment states nor the release history.

#### Option 4: A stored state column on `apps`

An `apps.state` column the reconcile loop maintains, read directly by the listing.

**Pros**:
- The cheapest read of all, one column, no join.

**Cons**:
- A derived value stored without a measured performance problem, which is how state goes stale: every path that changes a deployment now has to remember to write it too.
- Adds a second writer of app state next to the deployments table, and the two can disagree with nothing to say which is right.
- Needs a migration for a value already derivable in the same query.

### How the teardown happens

#### Option A: Delete the namespace, database first, with a reaper (chosen)

Soft delete the row, then one namespace delete, with an orphan pass in the reconcile loop.

**Pros**:
- One API call tears down eight objects atomically from the caller's point of view; there is no partial teardown inside a single delete.
- Follows the project's write before act rule, and the orphan it can leave is self healing.
- The reaper also cleans up namespaces orphaned by anything else, not only by a failed delete.

**Cons**:
- Adds the first unattended destructive loop in the platform, where a bug in one query is a data loss bug.
- Namespace deletion is asynchronous and can stall on a finalizer, so "accepted" is not "gone".

#### Option B: Delete each object, then the namespace

Delete the Deployment, Service, Ingress, Secret, policies, quota, limits and binding individually.

**Pros**:
- Each failure names exactly which object survived.
- Never depends on namespace deletion semantics.

**Cons**:
- Eight or nine calls, each of which can fail, so it invents the partial teardown state that Option A does not have.
- Every object composed in future has to be remembered here too, and forgetting one is silent.

#### Option C: Hard delete the database rows

Remove the app row and everything referencing it.

**Pros**:
- No soft delete filter to get wrong in every future query, and no ghost rows.

**Cons**:
- The hostname guarantee depends on the app row surviving: the slug is unique across every row that ever existed precisely so a retired hostname cannot be reissued.
- Every foreign key in the schema is `ON DELETE RESTRICT` on purpose, so this means either loosening them or cascading deletes through deployments, events and releases.
- Destroys the audit trail of what the account ran, including the release history a person may need after the fact.

## Rationale

The listing is chosen against the failure that the deployment vocabulary invites. Because a rolling update keeps the previous pods serving, and because spec 0011 deliberately leaves `current_release_id` untouched when a deploy fails, "the newest deployment failed" and "the app is down" are different statements, and the platform already stores both separately. Collapsing them at the response boundary would be inventing an ambiguity the data model does not have. Option 2 costs one extra field and no extra query, so the honest answer is also close to the cheapest one. Option 4 was the tempting alternative and is the one to name explicitly: storing a derived state buys a marginally cheaper read in exchange for a second writer that can drift, on a platform where the reconcile loop is already the trickiest code to reason about.

The teardown follows the same instinct: keep one authority. Kubernetes already knows how to delete everything in a namespace, and every object this platform composes for an app lives in exactly one. Reimplementing that cascade object by object, as Option B does, is more code that has to stay in step with `internal/deploy` forever, and it trades one asynchronous delete for eight synchronous ones that can each half fail. Hard deleting the rows, Option C, fails on a constraint that predates this feature: the slug uniqueness rule that protects hostnames is only meaningful while the row survives.

The reaper is what makes the ordering rule affordable. Writing the row first is not optional here, it is how the whole codebase works, and it means the only reachable inconsistency is an orphan namespace. Without a reaper that orphan keeps a hostname and a quota indefinitely and needs a human with `kubectl`; with one, the platform converges on its own within the grace period. The guards were chosen to make the dangerous direction structurally hard rather than carefully avoided: reading the live slugs as one query means a database failure aborts the pass rather than being mistaken for an empty set, the label selector means only namespaces the platform created are candidates, and a fifteen minute grace is far longer than the slowest build, so no plausible race reaches it. It also matters that slugs are never reused, which means a namespace name is never reused, which removes the one scenario where a reap could hit a new app that inherited an old name.

Two alternatives to the reaper were weighed and rejected. Retrying the namespace delete a few times before giving up would make the reaper fire less often, which is attractive for a loop flagged as the riskiest part of this feature, but it only helps the failure the handler is still alive for: it does nothing for a control plane that dies between the two steps, which is the orphan that matters most, so the reaper has to exist anyway and the retries only delay a call. Logging the failure and leaving it is cheaper still and is what leaves a dead app holding a hostname and a quota until a human notices.

The cadence took a correction. The reaper was first written onto the reconcile tick, on the assumption that the tick was a periodic sweep. It is not: `PolicySweep` runs once at `Run` entry, and the tick is a claim one deployment loop firing every few seconds, so a cluster wide namespace list on it would land in the path of every deploy claim. The reaper gets its own ten minute ticker in the same select, plus one pass at startup so a crash mid delete is cleaned up on the way back rather than ten minutes later. That is a second timer in a loop that has had one since slice 2, which is the cost of not slowing the claim path down.

Two smaller calls are worth recording. `internal/mcp` gains a single method Kubernetes port, which crosses a boundary that had held until now, every cluster action living in the reconcile loop. The alternative was to let the reaper do all teardown and have the tool only write the row, which needs no new dependency but leaves a deleted app serving traffic for up to the grace period; a delete that does not stop serving is not a delete. And a namespace delete that fails after the row is gone is reported as `internal` rather than as success, because the caller's app is still up and still reachable at that moment, and telling them otherwise would be the one place this feature lies to them.
