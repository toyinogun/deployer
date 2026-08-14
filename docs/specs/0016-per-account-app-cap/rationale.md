# 0016. Per account app cap: decision record

The build spec is in [index.md](index.md).

## Context

Every ceiling the platform has built so far is per app. Spec 0003 gives each app namespace a `ResourceQuota` and a `LimitRange`, so one app cannot take more CPU, memory, or pods than its share. Nothing counts apps. An account can call `deploy_app` with a new name as many times as it likes, and each one gets its own namespace, its own quota, and its own claim on the cluster. The ceiling is the cluster.

This stayed invisible because the tailnet kept the number of accounts at one. Slice 12 removes that fence: registration is now invite only rather than open, which means invited strangers, and the scope's open questions already recorded that isolation between accounts is solid while capacity between them is shared and ungoverned. The realistic failure here is not a malicious account, it is an agent in a loop deploying under a slightly different name each time.

Three facts about the existing code shape the decision. An app row is created in exactly one place, `resolveApp` in `internal/mcp/mcp.go`, reached from `deploy_app` after the upload check. `delete_app` soft deletes by stamping `deleted_at`, and every app read already filters on it. And a caller facing failure is one of a closed set of reason codes in `internal/domain/reason.go`, whose messages are a static map, so a refusal that wants to say a number has nowhere obvious to put it.

Not deciding means the first time two people use the platform seriously, the way one of them runs out of room is a cluster that stops scheduling, which is the worst possible way to find out.

## Options considered

### Option 1: A configured ceiling counted live at the one place an app is created

One `DEPLOYER_*` number, a `COUNT(*)` over the account's live apps at deploy time, refused with a new closed reason code before `resolveApp` runs, and enforced exactly inside the store transaction that inserts the row.

**Pros**:
- No schema change, no migration, no counter to keep true.
- A soft deleted app frees a slot for free, because the count uses the same `deleted_at IS NULL` predicate every other read uses.
- The rule sits at the only place an app is born, so there is nothing to bypass today.
- The refusal is an ordinary refusal: same `deny` path, same audit row, same closed code discipline.

**Cons**:
- One number for everybody. There is no way to give one person more room short of raising it for everyone.
- It counts apps, not capacity, so it is a crude proxy for the thing actually worth bounding.
- One more read on the deploy path, small but real.

### Option 2: The same ceiling, plus a per account override column

`accounts` gains a nullable `app_limit`; null falls back to the global number, and an admin control sets it.

**Pros**:
- Handles the real case where one person genuinely needs more room, without moving everyone's ceiling.
- The column is cheap and the fallback is obvious.

**Cons**:
- A migration, an admin page change, and a second source for a number that currently has one, for a platform with fewer than ten people on it.
- Builds the exception before the rule has been used once, so the shape of the exception is a guess.

### Option 3: Let Kubernetes enforce it

Give each account a parent construct, a hierarchical namespace or a quota on a shared account namespace, and let the cluster refuse the eleventh app.

**Pros**:
- The enforcement lives where the capacity lives, so it cannot be bypassed by anything that talks to the cluster directly.
- No count in the control plane at all.

**Cons**:
- The refusal arrives from the Kubernetes API partway through a deploy, long after the app row, the deployment row, and possibly a build all exist. The platform would then have to unwind them and translate an admission error into a closed reason code, which is exactly the error translation the closed set exists to avoid.
- It needs a new cluster construct, either a controller or a namespace layout change, for a number that a `COUNT(*)` answers.
- It breaks the project rule that every state transition is a database write before it is an action.

### Option 4: No cap, alert instead

Count apps on a schedule and log or notify when an account crosses a threshold.

**Pros**:
- Nothing on the deploy path, and it never refuses a legitimate deploy.
- Gives the real picture, including which accounts are growing.

**Cons**:
- A loop can create fifty apps between two checks, so it detects the incident rather than preventing it.
- The cluster runs no monitoring stack yet, as spec 0003's deferred certificate alert already recorded, so there is nowhere for the alert to go.

## Rationale

Option 1 wins on the shape of the existing code more than on theory. There is one create path, the soft delete predicate is already universal, and the refusal machinery already exists, so the whole control is a number, a count, a code, and one `if`. Option 3 is where this rule would live in a larger platform, but here it would push the refusal past the point where rows have already been written, and then ask the platform to translate an admission error into a closed reason code. That is the exact boundary spec 0004's AC-16 drew, and crossing it to enforce a count is a bad trade. Option 4 does not solve the problem the Context names: an agent in a loop outruns any schedule.

The override column was left out on purpose, and this is the choice most likely to be revisited. With under ten people the honest read is that the global number will be right for everyone, and the moment it is not, the shape of the exception will be obvious in a way it is not now. It is recorded as a Follow-up rather than built, because a nullable column plus an admin control plus a fallback rule is real surface area for a case that has not happened.

Two smaller calls are worth recording. The numbers travel in the composed refusal line rather than in a new structured output, because every other refusal in the platform is a code and a sentence, and a single feature that answers differently is the thing a caller has to special case. The domain message stays static and numberless, so `internal/domain` never learns about configuration and the closed set keeps its shape; the numbers are appended by an optional detail on `deny` and `toolError` that every existing refusal leaves empty.

And the check is exact rather than best effort: the count and the insert run in one store transaction. The reason that is enough is narrower than "SQLite is a single writer", and worth stating precisely, because a deferred transaction that reads first and upgrades to a write later would let two callers both read one slot free and both then write. The store's `inTx` opens with `BEGIN IMMEDIATE`, so it holds the write lock before the count statement runs, and that ordering is what makes the check exact. A cap that can be raced past by one is a cap that has to be explained, and the store already owns a transactional create for deployments with supersession, so this is a known pattern here rather than new machinery.

The Follow-up about a shared guard is the real fragility. This design is correct because `deploy_app` is the only way an app row is created. That is true today and is not a property anything enforces, so a future web create route would silently bypass the cap. It is recorded rather than pre built, because a guard with one caller is a wrapper, and the moment there are two callers the right shape is obvious.
