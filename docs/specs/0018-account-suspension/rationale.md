# 0018. Account suspension: decision record

The build spec is in [index.md](index.md).

## Context

Spec 0007 gave the platform an admin surface with one lever on it: disable an account. Disabling stamps `accounts.disabled_at`, revokes every live session and email link in the same transaction, and every credential path then refuses that account. Token resolution filters on the column, session resolution filters on it, and `Account.usable()` holds the rule again in Go so it is readable in one place. That half is solid and has been since slice 4.

The other half was never built. An account's apps are Deployments in their own namespaces, driven by the reconcile loop and reached over the wildcard hostname. Nothing in the disable path touches them. A disabled account cannot sign in, cannot deploy, and cannot call a tool, and every app it already deployed carries on serving traffic, holding pods, and making outbound connections, indefinitely.

That gap was tolerable while the tailnet was the fence and the platform had one user. Slice 12 removes both conditions: registration is invite only rather than impossible, apps are about to be reachable from the open internet, and the remaining controls in this slice, the app cap and the egress bounds, are all about a stranger who turns out to be a problem. Suspension is the one that has to work at 2am, when the answer to a running abuse is not a code change.

Two details of the existing code shape the decision. The first is that a refusal a caller sees is a closed reason code, and a disabled account currently produces none: the auth layer collapses unknown, revoked, expired, and disabled into one invalid credential error, deliberately, so a suspended agent gets a blank error and retries forever. The second is that everything about an app's identity, its namespace, its Service, its Ingress, its configuration Secret, and its slug, is composed from the slug and stored in the cluster, so there are several places to cut and only one of them is cheap to undo.

Not deciding leaves the platform in the state where the only way to stop a running abuser is `kubectl`, by hand, per namespace, on a laptop, at the exact moment nobody wants to be composing commands.

## Options considered

### Option 1: Extend the existing disable into a full suspension, scaling to zero, held by a sweep

`accounts.disabled_at` stays the single state. The same admin action now also scales every live app the account owns to zero replicas, restore scales them back to one, a sweep re-asserts zero for suspended accounts on the reconcile cadence, and a suspended token is refused with a new closed reason code on every agent surface.

**Pros**:
- No migration and no second state, so no way for two lockout flags to disagree.
- Restore is a scale up: no rebuild, no push, no new release, and the app comes back on the same digest.
- Scaling to zero leaves every other object in place, so the slug, the hostname, the configuration, and the release history survive a suspension untouched.
- The sweep gives the cluster half a retry loop, which is what makes a best effort write acceptable.
- The refusal reuses the existing reason code machinery rather than inventing a new failure shape.

**Cons**:
- Lockout and app shutdown become one action, so suspending sign in alone is no longer possible.
- The cluster half is best effort, so there is a window between a failed scale and the next tick where a suspended app still serves.
- `ResolveToken` has to stop filtering disabled accounts to tell a suspended token from a dead one, which moves that gate from two places to one.

### Option 2: A separate suspension state beside the existing disable

A new `suspended_at` column, so disable keeps meaning sign in lockout and suspend means apps down as well.

**Pros**:
- Two levers for two situations: lock out a person who forgot their password without taking their production apps down.
- The existing disable semantics stay untouched, so no existing gate changes.

**Cons**:
- A migration and two states, each of which has to be checked in the auth path, the sweep, and every read that cares. Two flags eventually disagree.
- The scope's done when line does not ask for two levers, and nobody has yet wanted the weaker one.
- Every future gate has to answer which state it cares about, and getting that wrong is silent.

### Option 3: Tear the apps down rather than stopping them

Suspension deletes the app namespaces, the way `delete_app` already does, and restore redeploys from the current release.

**Pros**:
- Nothing of the suspended account runs or occupies the cluster at all, which is the strongest possible answer.
- Reuses the delete path that already exists and is already tested.

**Cons**:
- Restore is a rebuild or at best a full redeploy, which the scope explicitly rules out: it asks for restoring serving without a rebuild.
- Deleting namespaces destroys configuration and anything the app wrote, so a wrongly suspended account loses real state.
- It is not reversible in any honest sense, which is a bad property for a control exercised on suspicion at 2am.

### Option 4: Fence the account at the network instead

Suspension applies a deny all NetworkPolicy over the account's namespaces, or removes their Ingress, leaving the pods running.

**Pros**:
- Instant, one object per namespace, and trivially reversible.
- Nothing about the workload changes, so restore is exact.

**Cons**:
- The pods keep running and keep costing CPU, memory, and the account's share of the cluster, which is the resource abuse the control exists to stop.
- The policy sweep already owns the NetworkPolicies in those namespaces, so two writers would fight over the same objects.
- Removing the Ingress instead makes restore more than a scale up and risks the hostname being reclaimed elsewhere.

## Rationale

Option 1 wins on the two forces that actually bind. The scope's done when line asks for apps that stop serving and come back without a rebuild, which rules out tearing namespaces down (Option 3) and rules out anything that leaves pods burning capacity (Option 4). And the platform's own rule that a state is one column read in one place, held again in Go, argues strongly against a second lockout flag (Option 2) for a product with one admin and under ten users. A second lever is machinery for a situation nobody has had.

The uncomfortable part of Option 1 is honest and worth stating plainly: to tell a suspended token from a dead one, `ResolveToken` stops filtering `disabled_at IS NULL`, so the gate that was enforced twice, in SQL and in Go, is enforced once. That is a real reduction in margin on the most sensitive path in the platform. It is accepted because the alternative is an agent that retries a blank error forever, which the scope explicitly rejects, and because the Go side of the gate was already written as the readable authority rather than as a backstop. The credential shapes get their own test as a result: unknown, revoked, and expired must stay indistinguishable, and only a valid token on a suspended account may say so.

The sweep is what makes the cluster half acceptable. A single inline scale that fails leaves an abuser serving with nothing to correct it, and refusing to suspend because Kubernetes is unhappy would break the control at exactly the moment it is needed. So the database write lands first and always, the scale is attempted inline for immediacy, the admin is told which apps did not stop, and the sweep keeps trying. That mirrors what the policy sweep already does for network policies in spec 0008, which is a pattern this codebase has and understands rather than a new one.

Refusing at one gate in front of every tool, rather than in each tool or at the transport, comes from the same instinct as the closed reason codes themselves: the agent has to be able to act on the answer. A transport level 403 reads to most MCP clients as a broken server, and eleven per tool checks is eleven chances to forget the twelfth. One receiving middleware on the per request server refuses `tools/call` with the code and inherits every tool added later.
