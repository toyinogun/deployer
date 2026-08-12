# 0006. Application logs: a bounded, redacted read of an app's own output

**Date**: 2026-08-12
**Status**: In Progress

## Summary

This adds one MCP tool, `get_logs`, so an agent can read the recent output of an app it deployed without you opening a terminal. It is a snapshot, not a stream: one call returns the newest lines from the app's container, plus a separate smaller block from the previous container when the app crashed and restarted, with a closed set of secret shapes blanked out. Nothing is stored. The platform reads the lines from Kubernetes at the moment of the call and hands them straight back, so there is no new table, no log store, and no schema change.

The one thing to be clear eyed about: redaction of arbitrary application output is best effort, not a guarantee, and the tool says so in its own description.

Reasoning, the options weighed, and the premise note: see [rationale.md](rationale.md).

## Requirements

**User stories**:

- As an AI coding agent, I want to read my deployed app's recent output, so that I can debug why it misbehaves without asking the person to run kubectl.
- As an AI coding agent, I want the crashed container's output when my app is restarting, so that I can see the cause rather than the symptom.
- As an AI coding agent, I want a bounded response, so that reading logs does not swallow my context window.
- As the operator, I want a log read to be an app read only, so that no caller can reach build output, platform logs, or another account's app through it.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

- **AC-1**: `get_logs` takes one argument, `name` (the app's name), and returns the app container's recent output as an ordered list of entries, each carrying the timestamp Kubernetes recorded and the line itself.
- **AC-2**: the caller may pass `tail_lines`. Absent, zero, or negative reads as unset and means 200. Above the maximum of 1000 it is clamped to 1000 rather than refused. The response always echoes the value actually applied, and carries `clamped: true` only when it differed from what was asked.
- **AC-3**: the current container block is capped at 64 KiB regardless of `tail_lines`. When the ceiling is reached the oldest entries are dropped, and the response carries `truncated: true` with the number of entries dropped. Redaction runs on each line first and bounding runs on the redacted lines, so the reported sizes and counts describe what the caller actually receives.
- **AC-4**: when the app container has restarted at least once, the response carries a second block of the previous (terminated) container's output in its own field, capped independently at 100 lines and 16 KiB, so restart noise in the current container can never squeeze it out. With no restart the field is absent from the response rather than present and empty.
- **AC-5**: the pod read is the newest pod by creation time in the app's namespace, whatever phase it is in, so a crash looping or pending pod is the one reported rather than an older healthy one. When that pod is not ready and an older pod still exists, the response note says an older pod may still be serving, so a near empty answer is not misread as a silent app.
- **AC-6**: every returned line, in both blocks, passes through redaction before bounding. The closed set is: `Authorization` header values (Bearer and Basic), JWT shaped triples of base64url segments, URLs carrying `user:password@`, `AKIA` style access key ids, assignment forms whose name contains key, token, secret, or password, and the literal registry credential the platform itself placed in the namespace. High entropy alone is never a trigger, because it blanks legitimate output. The tool description states redaction is best effort.
- **AC-7**: when the app exists but no container has started, the response is an empty entry list plus the app's current deployment state and one plain sentence saying why there is nothing to show. This is a success, not a failure. The case is detected from pod status before the log API is called: no pod at all, no container statuses yet, or a container still in `Waiting`. When there is no pod but the latest deployment is `healthy`, the sentence says the output is no longer available rather than that nothing has started.
- **AC-8**: an app name that does not exist and an app belonging to another account get the same answer, the reason code `app_unknown`, so the tool cannot be used to learn which app names exist.
- **AC-9**: every refusal writes one audit row, with the action and the account. A successful read writes nothing, and neither does an internal fault: a fault is not an access decision, and recording one would write a denial that never happened. This is the line `deployment_status` already drew.
- **AC-10**: a read that cannot complete fails with the reason code `internal` and returns no entries. Only an error from reading a container that has actually run counts here; a container that has not started is AC-7, decided from pod status before the log call. A partial read is never presented as the app's output.
- **AC-11**: `get_logs` never returns build Job output, control plane logs, Kubernetes events, or any object outside the caller's own app namespace. The namespace read is derived from the app's slug, never from a caller supplied value.
- **AC-12**: the feature adds no table and no column. The only database write on this path is the audit row of AC-9.
- **AC-13**: the tool's description carries its contract: that it is a snapshot rather than a stream, the default and maximum line counts, that output is truncated oldest first, that a previous container block appears after a crash, and that redaction is best effort.
- **AC-14**: the control plane needs no new cluster rights. `pods` list and `pods/log` get are already granted in the `deployer-app` ClusterRole, bound only inside app namespaces.

## Decision

**Chosen option**: Option 1: read the pod's log live from Kubernetes on each call, bounded and redacted in the platform, and store nothing.

`get_logs` resolves the caller's app, picks the newest pod in `app-<slug>`, reads its status to decide whether there is anything to fetch, asks the Kubernetes API for that container's tail with timestamps, redacts and then bounds the result in pure Go, and returns it. A second, smaller read of the previous container runs only when the container has restarted.

`app_unknown` is a new code rather than a reuse of `deployment_unknown` because this tool is addressed by app name: `deployment_unknown`'s message names deployments and ids, and giving a caller an answer about a thing they did not ask about is how a closed reason set stops being useful.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `golang-testing` (`~/.claude/skills/golang-testing/`) · `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`) · `security-patterns` (`~/.claude/skills/security-patterns/`)

## Rationale

Reasoning and the options weighed: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

No schema change. The feature reads three things that already exist and writes one row that already has a table:

| Existing table | Used for | Written? |
|---|---|---|
| `apps` | resolving `name` to the owning account and the slug the namespace is named from | no |
| `deployments` | the current state reported in the empty case (AC-7) | no |
| the audit table (spec 0002) | one row per refusal (AC-9) | yes, refusals only |

**State transitions**: none. A log read observes state, it never moves a deployment.

**API surface**:

| Tool | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `get_logs` | MCP tool call | `name`:string (req), `tail_lines`:int (opt, default 200, clamped at 1000) | `app_name`, `state`, `tail_lines`:int (applied), `entries[]{at,message}`, `previous[]{at,message}` (absent with no restart), `truncated`:bool, `dropped`:int, `clamped`:bool (absent when false), `note`:string (absent when there is nothing to say) | platform API token, resolved to an account by the existing middleware | `app_unknown` (unknown app, or another account's), `internal` (the read could not complete) |

Optional fields are omitted rather than returned null or empty, matching `deployment_status`'s shape, so an agent can tell "no previous container" from "a previous container that printed nothing".

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| `get_logs` | the app record and its owner | `apps.ByName(account.ID, name)`, the same read `deployment_status` uses |
| `get_logs` | the namespace to read in | derived, `deploy.NamespaceName(app.Slug)`, never a caller value |
| `get_logs` | which pod | newest by `metadata.creationTimestamp` among pods matching the app's existing selector labels in that namespace |
| `get_logs` | which container | the `deploy.WorkloadName` constant; an app pod has exactly one container |
| `get_logs` | each entry's `at` | the kubelet's own timestamp, requested with `Timestamps: true` and passed through unparsed |
| `get_logs` | each entry's `message` | the log line with the timestamp prefix stripped, then redacted |
| `get_logs` | whether a previous block exists | the app container's `restartCount` in `status.containerStatuses` being above zero. An empty `containerStatuses` means no container has started, which is the AC-7 path, not an index into position zero |
| `get_logs` | whether the empty case applies | pod status read before any log call: no pod, no container statuses, or the container still in `Waiting` |
| `get_logs` | `truncated`, `dropped`, `clamped`, applied `tail_lines` | computed in `internal/logs` from the constants and the redacted lines |
| `get_logs` | `state` and `note` in the empty case | `deployments.LatestForApp(app.ID)`, the read `deployment_status` already makes. `healthy` with no pod produces the "no longer available" wording |
| `get_logs` | the "an older pod may still be serving" note | the newest pod not being ready while the pod list holds more than one entry |
| `get_logs` | the redaction of platform placed secrets | the registry pull secret value the control plane already holds in config |
| refusal | the audit row's account and action | the authenticated account from the MCP middleware, and a new `ActionLogs` |

**Key invariants**:

- The namespace and container read are always derived from the resolved app, so no caller input ever reaches the Kubernetes API as a name.
- Redaction runs on every line of both blocks, and always before bounding. There is no path that returns an unredacted line, and no path where the reported sizes describe lines the caller did not get.
- The two blocks have independent caps. Exhausting one never reduces the other.
- The empty case is decided from pod status, never from an error returned by the log call. A container that has not started is never reported as a fault, and a fault is never reported as an empty log.
- A read either returns a complete bounded answer or fails. There is no partial success.
- Truncation drops the oldest, never the newest.

**Security model**:

Read is scoped to the calling account: an app resolves through `apps.ByName(account.ID, name)`, so another account's app is indistinguishable from one that does not exist (AC-8), matching how `deployment_status` refuses. No compliance scope applies; this is a homelab platform with one operator and no regulated data. The sensitive surface is the app's own output, which the caller already owns the source of, so the risk being managed is accidental exposure of a credential the app printed, not access control. Redaction is defence in depth, and the tool says so rather than implying a guarantee.

**Configuration required**: none. The bounds are Go constants in `internal/logs`, because they are product decisions rather than per deployment tuning, and adding three validated environment variables for values that will not change is cost without benefit.

**Critical test scenarios**:

- Happy path: a healthy app returns its recent lines with timestamps, oldest to newest, under the cap, verifies **AC-1**, **AC-2**.
- Bounding: an app that has printed far more than the ceiling returns the newest entries with `truncated: true` and a `dropped` count, verifies **AC-3**.
- Crash loop: a pod with `restartCount` above zero returns both blocks, and a noisy current container does not shrink the previous block, verifies **AC-4**, **AC-5**.
- Redaction: a line containing a bearer token, a JWT, and a URL holding a password comes back with each blanked, and a line that merely looks long is left alone, verifies **AC-6**.
- Empty case: an app whose deployment is still building returns zero entries plus `state: building` and the note, and does not error, verifies **AC-7**.
- The discrimination: a pod whose container is in `Waiting` returns the empty case without the log API being called at all, while a container that has run and whose log read errors returns `internal`, verifies **AC-7**, **AC-10**.
- Auth/permission: another account's app name, and a name that does not exist, both return `app_unknown` with the same message, and both write an audit row, verifies **AC-8**, **AC-9**.
- Failure case: the Kubernetes API returning an error mid read produces `internal` with no entries and no cluster detail in the message, verifies **AC-10**.

## Build plan

Ordered as a thin thread, in keeping with the project's Tracer Bullet approach: the pure logic is written test first and lands first, then the one cluster read it needs, then the tool that joins them, so the slice is provable end to end in the fewest steps.

1. Create `internal/logs` with the pure functions, written test first and importing no `client-go` and no `net/http`: parse a kubelet timestamped line into an entry, redact an entry against the AC-6 shape set plus a caller supplied set of literal secret values, clamp a requested tail (unset, zero, and negative all meaning the default), and bound a slice of already redacted entries to a byte ceiling reporting what was dropped. The constants live here: default tail 200, maximum tail 1000, current block ceiling 64 KiB, previous block 100 lines and 16 KiB, satisfies **AC-2**, **AC-3**, **AC-6**.
2. Add `PodsForApp` and `PodLog` to `internal/kube`: list pods in the app namespace by the existing selector labels and return them newest first with each one's phase, readiness, container statuses, and restart count, and read one container's tail with `Timestamps: true`, optionally the previous container. The pod status read is what the empty case is decided from, so it returns enough to make that call without touching the log API. Tested against the fake clientset, satisfies **AC-5**, **AC-11**.
3. Add the `app_unknown` reason code and its one line message to `internal/domain/reason.go`, and the `ActionLogs` audit action to `internal/auth`, both with tests pinning the closed set, satisfies **AC-8**, **AC-9**.
4. Add `internal/mcp/logs.go`: the `get_logs` tool, its input and output types with optional fields omitted, account scoped app resolution reusing the `deployment_status` pattern, the pod status gate that decides the empty case before any log call (including the `healthy` with no pod wording and the older pod still serving note), the previous container block when the restart count is above zero, and the failure path returning `internal` with no audit row. Register it on the server, satisfies **AC-1**, **AC-4**, **AC-7**, **AC-9**, **AC-10**, **AC-12**.
5. Write the tool description as contract, covering snapshot semantics, the default and maximum tail, oldest first truncation, the previous container block, and best effort redaction, satisfies **AC-13**.
6. Confirm no manifest change is needed by checking the `deployer-app` ClusterRole already carries `pods` list and `pods/log` get, and record it in the verify steps rather than editing RBAC, satisfies **AC-14**.

## Consequences

**Positive**:

- An agent can debug its own app without a human at a terminal, which is the whole point of the platform.
- No new infrastructure, no log store to operate, no retention policy to get wrong, and no growth in the SQLite file that spec 0002 already flags as an unbacked secret store.
- The crash looping case, the one an agent most needs and the one a naive implementation misses, is covered by design rather than by luck.
- The read path is pure logic plus one narrow cluster call, so most of it is testable without a cluster.

**Negative / tradeoffs**:

- Logs live only as long as the node keeps them. A pod that was replaced, evicted, or garbage collected takes its output with it, and nothing here can recover it.
- Redaction of arbitrary application output is best effort. A secret in a shape the pattern set does not know is returned in clear, and the honest mitigation is the description saying so, not a stronger regex.
- No search, no filter, no time window, no follow. An agent hunting a line from an hour ago cannot get it.
- Only the newest pod is read, so during a rolling update the outgoing pod's output is invisible.
- The `previous` block covers one restart back. An app that has crashed repeatedly shows only the most recent crash.
- One log record is one entry, so a stack trace arrives as many entries. Dropping the oldest can cut a trace in half, which is the honest cost of a fixed ceiling and is not worth machinery to detect.

**Neutral**:

- A new package, `internal/logs`, joining the existing edge split of `build`, `registry`, and `deploy`.
- The reason code set grows from eleven to twelve, and the audit action set gains one entry.
- Reading logs during a deploy is allowed and returns whatever the container has printed so far, which is deliberate: an app that starts and then fails its probe is exactly when the output matters.

## Follow-up

- [ ] Once app environment configuration lands (slice 7), feed the app's own configured secret values into the redactor's literal set, which is the only redaction that can be exact.
- [ ] Consider a `since_seconds` argument if line counts prove the wrong unit in practice. Left out now because a line count is what bounds a context window, and a duration does not.
- [ ] Kubernetes events for an app that never produces output (image pull failure, out of memory kill) remain unexposed. Worth a separate decision if `state: failed` with an empty log turns out to be a common dead end.
