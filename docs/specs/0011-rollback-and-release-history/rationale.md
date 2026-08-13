# 0011. Rollback and release history: rationale

The decision record for [index.md](index.md). `/develop` does not read this file.

## Context

Every healthy deploy already mints a release: an image digest plus the configuration that image was running with, recorded at the moment the app became healthy. Spec 0002 designed that table, the `source_release_id` column, and the CHECK that makes a deployment unambiguously a build deploy or a rollback. Spec 0010 made the snapshot real by giving apps configuration worth snapshotting. Nothing has ever read either one. An agent that deploys a broken version today has exactly one route back: find the old source, upload it again, and wait out a full build.

Three forces shape what to do about that.

The first is that an image is not an app. The same image with different environment variables is a different running system, so re promoting a digest against today's configuration restores something that was never known good. Spec 0010 said this in its own follow up and left the work here. It also left the mechanism: `envFrom` values are read once at container start, and Kubernetes does not roll a pod when a referenced Secret's contents change, so the pod template checksum spec 0010 added is the only thing that makes a configuration only change actually take effect. A rollback to the same digest with different configuration is precisely the case where the image digest does not change, which is the case the checksum exists for.

The second is that this is a recovery tool, and recovery tools are used badly. They are reached for under pressure, by an agent that has just broken something, often with incomplete information about what it broke. Every refusal, every ambiguity, and every silent partial action costs more here than in a tool used calmly. That argues for the target being explicit rather than inferred, for the action winning over whatever is in flight, and for a failure leaving the recorded history exactly as it was.

The third is that the surface an agent sees is small on purpose. The tool set is six today, every description is contract rather than documentation, and the reason codes are a closed set of eighteen that a caller can branch on. Anything added here is added to a context window that also holds the agent's actual task. A listing that pages, a tool with a mode argument, or three new codes where one does are each a real cost paid on every call.

Not deciding leaves the release table as dead weight: a snapshot written on every deploy, holding secret values in clear, that nothing ever reads.

## Options considered

### Option 1: A single rollback tool, no listing

Add only `rollback_app`, taking a release number, and let the caller learn which numbers exist from `deployment_status` responses they already collected.

**Pros**:
- The smallest possible addition: one tool, one reason code, no read path to design or bound.
- Nothing new can leak configuration, because nothing new reads a release row.

**Cons**:
- An agent that did not deploy the earlier versions, which is the normal case for a fresh session, has no way to discover a valid release number at all. It can only guess, and a wrong guess replaces what is running.
- It makes `release_unknown` the routine answer rather than the exceptional one, which is how a closed reason set stops being useful.

### Option 2: Two tools over the release table, configuration restored inside `MarkHealthy`

`list_releases` reads the newest twenty releases by app name, bounded by a constant, projecting named columns and never opening the snapshot. `rollback_app` takes the app name and a required release number, resolves it to a release id, and calls the existing `store.CreateDeployment` with `SourceReleaseID`. The reconcile loop recognises a rollback and enters at `deploying`, composes the Secret from the source release's snapshot, and `MarkHealthy` mints the new release and rewrites `app_config` in one transaction.

**Pros**:
- Discovery and action are separate tools with separate contracts, which is the shape the rest of the surface already uses (`get_config` and `set_config`, `deploy_app` and `deployment_status`).
- The store does almost all of it already: `CreateDeployment` accepts a source release, copies the digest at creation, and supersedes what is in flight. The reason codes, the transaction, and the numbering are all existing behaviour reached through a new door.
- Restoring configuration inside `MarkHealthy` means the table write and the release land together, so stored configuration cannot describe a state that is not running.
- No migration, no new environment variable, no new timeout.

**Cons**:
- Two more tools in the agent's context, and two more descriptions that carry contract and can drift from behaviour.
- `deployApp` gains its first branch on deployment kind, so the drive is no longer identical for every deployment from `deploying` onward.
- Rewriting `app_config` overwrites configuration the caller set since the release, silently.

### Option 3: One `manage_releases` tool with an action argument

A single tool taking `action: "list" | "rollback"` plus the arguments each mode needs.

**Pros**:
- One tool slot rather than two.

**Cons**:
- A mode argument is the MCP tool shape agents get wrong most often: the schema cannot express which arguments are required in which mode, so a wrong combination is caught by handler validation rather than by the schema.
- One description has to carry two contracts, including a bound that applies to one mode and a supersession rule that applies to the other.

### Option 4: Two tools, but configuration restored at `rollback_app` time

The same two tools, except `app_config` is rewritten to the snapshot when the deployment row is created. The reconcile loop then needs no change to its configuration read at all: the existing `ConfigForDeploy` naturally picks up the restored values.

**Pros**:
- The reconcile loop is untouched on the configuration side. `deployApp` keeps one code path.
- Slightly simpler to reason about at the moment of the call: the configuration is restored, then it is deployed.

**Cons**:
- A rollback that fails leaves `app_config` holding the old release's configuration while the running app still has today's. That is exactly the drift spec 0010 closed, reopened at the worst moment.
- It also breaks the rule that every state transition is a database write before it is an action, in the other direction: it makes a configuration change happen before the action it belongs to is known to succeed.

## Rationale

Option 2 wins on the second force above, the one about recovery tools being used badly. A listing exists because an agent under pressure must not have to guess a release number, and Option 1's whole cost is that it forces exactly that guess. Between Option 2 and Option 3, the mode argument in Option 3 buys one context slot and pays for it with a schema that cannot describe its own contract, which matters more than usual here because a rollback replaces what is running and a malformed call is not a cheap mistake.

Option 4 is the one worth arguing about, and it loses on the first force. Spec 0010's whole point was that configuration and the running pod must not disagree, and that a release records what actually ran rather than what the table happens to hold. Restoring configuration before the rollback succeeds means a failed rollback leaves `get_config` describing a system that is not running, in the exact situation where the caller most needs to trust what it reads. Putting the write inside `MarkHealthy` costs one branch in `deployApp` and one store write, and buys the guarantee that stored configuration only ever changes when the thing it describes actually became healthy.

The remaining calls follow from the same two forces. The target is a required `release_number` rather than an optional one defaulting to "previous", because "previous" stops being well defined the moment a rollback mints its own release, and an agent that guesses wrong replaces production. A rollback supersedes what is in flight rather than being refused, because refusing means a broken deploy in flight blocks the action that fixes it, and because `store.CreateDeployment` already supersedes unconditionally, so refusing would be new logic added to make the platform less useful. The listing is capped at twenty with no cursor, following the same rule the `internal/logs` bounds follow: the number is a product decision about what fits an agent's context window, not an operator's knob, and release history is for choosing a target rather than for auditing.

One code rather than three is the same discipline `deployment_unknown` and `app_unknown` already set. `release_already_current` and `release_image_missing` only exist if those cases are refused, and neither should be: rolling back to the current release is a genuine repair when configuration has drifted since, and an image missing from the registry is a platform failure rather than a caller error, so it belongs on the `internal` path where it already lives.

The `is_secret` gap was found by tracing what a rollback has to produce back to a source. The snapshot is `map[string]string`: values only, no flags. Restoring `app_config` from it therefore has no source for `is_secret`, and nothing in the design named one. Changing the format to `{value, secret}` per key gives new releases a real source, and treating an old snapshot's bare string as secret picks the failure direction that hides a value rather than leaking one. It needs no migration because the column is JSON text and the decode is decided per key.

## What the cross check changed

An independent read on a second model, against the real code, found seven things the design was quietly relying on. All seven are now named sources or named tasks in `index.md`; they are recorded here because each was a decision the build would otherwise have had to invent.

The load bearing one was the secret flag. The design said new snapshots record `{value, secret}` per key, and also said `MarkHealthy` keeps its existing contract. Those two cannot both be true: `ConfigForDeploy` drops the flag when it flattens `[]ConfigEntry` into a map, so `MarkHealthy` never sees one, and every "new" snapshot would have been written in the old shape and read back as all secret. Preserving the existing signature was the root cause. Widening both contracts to carry the flag fixes the encoder side, makes "a release records what actually ran" true for the flag as well as the value, and removes a second decode of the snapshot inside `MarkHealthy` that the original design needed and had not accounted for. The cost is that a rollback feature now touches the build deploy path.

Three were missing sources. The listing's `current` flag needs `apps.current_release_id`, which `mcp.App` does not carry. A rollback's workload image needs the repo half of `repo@digest`, which `resolveImage` fills on a build deploy and a rollback skips, and which no release row holds; recomputing it from the permanent slug is deterministic and needs no column. And the reconcile loop cannot tell a rollback from a build deploy at all, because `reconcile.Deployment` has no field for it and both are `queued` when claimed.

Two were control flow the design had put in the wrong place. `Drive` resolves the upload and removes the tarball outside `run`, so "recognise a rollback in `run`" would have failed every rollback as `upload_invalid` before any phase check ran. And `run` dispatches on state alone, so entering at `deploying` is a new branch inside the `queued` block rather than a phase being skipped. The resume sweep rebuilds the struct from the store, so it needs the new field too, or a restart mid rollback resumes it as a build deploy.

The last was a security claim that was not true. `ListReleasesByApp` is `SELECT *`, so reusing it would have loaded `config_snapshot` into the process and left AC-4 resting on the handler remembering not to serialize a field it was holding. A separate narrow query keeps the guarantee where the spec claimed it was.

The cross check also raised the concurrent `set_config` during the readiness wait. That one is not a bug to fix but a consequence to state: the snapshot winning is what a rollback means, and refusing `set_config` while a rollback is in flight would add a reason code for a rare case. It is now an acceptance criterion, a consequence, and a line in the tool description, so it is specified rather than discovered.

## Notes carried forward

- Spec 0002's `verify.md` already has `TestRollbackFidelity` asserting that a rollback carries its source release's digest from creation, walks `queued` to `deploying` to `healthy` in three events, and leaves the source release's snapshot untouched. The store half of AC-9, AC-11, and AC-16 is therefore already proven; this feature's tests cover the tool surface, the reconcile branch, and the configuration restore.
- The fake clientset resolves no names and execs nothing, so the pod template checksum actually rolling the pods is a unit test of the composed manifest, not proof of a rollout. Proving the rollout belongs to `/check verify` against the real cluster.
