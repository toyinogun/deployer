# 0010. App environment configuration: values an app is given, and the ones it is never shown

**Date**: 2026-08-13
**Status**: Accepted

## Summary

Today a deployed app is told one thing, its port, and nothing else, so anything needing an API key or a database address cannot run here. This adds three MCP tools (`set_config`, `unset_config`, `get_config`) that store an app's configuration as key and value pairs, and a deploy then hands every value to the container as environment variables through one Kubernetes Secret (the cluster's object for values you do not want readable in a plain workload dump). A caller marks a value secret, and a secret value never comes back in a response and is blanked out of the app's logs. Changing a value does not touch the running app: the change lands on the next deploy, and the response says so.

## Requirements

**User stories**:

- As an AI coding agent, I want to set configuration for an app I deployed so that the app can reach the API and database it was written against.
- As an AI coding agent, I want to read back which keys are set so that I can tell whether the app is missing something before I blame my own code.
- As the person owning the cluster, I want a value I called secret to stay out of MCP responses, platform logs, and the app's own log output, so that a transcript in an agent's context window is not a place my credentials live.
- As an AI coding agent, I want a brand new app to be deployable with its configuration in one call so that its first run is not a guaranteed crash on a missing variable.

**Acceptance criteria**:

- **AC-1**: `set_config` on an app the caller owns writes the given keys in one transaction, merging them with whatever is already set, and returns the resulting configuration. All of the keys are written or none are.
- **AC-2**: A key marked secret is returned as its key plus the secret flag, with no value, in every MCP response without exception, including the response to the call that just set it.
- **AC-3**: `unset_config` removes the given keys in one transaction and returns the resulting configuration. If any key in the call is not set, the whole call is refused with `config_key_unknown` and nothing is removed.
- **AC-4**: A key that does not match `[A-Z_][A-Z0-9_]*` is refused with `config_key_invalid` before any write happens, so the database constraint is never the thing that reports the error.
- **AC-5**: `PORT` and `APP_URL` are refused with `config_key_reserved`. The platform keeps injecting both itself.
- **AC-6**: Configuration is bounded. Over 64 keys for one app is refused with `config_too_many_keys`; a value over 4 KB, or a total over 32 KB, is refused with `config_too_large`. A refused call writes nothing.
- **AC-7**: A deploy composes one Kubernetes Secret in the app's namespace holding every configured key and value, and the container receives them with `envFrom`, alongside `PORT` and `APP_URL`, which the platform sets and which the Secret never contains.
- **AC-8**: Setting or unsetting a value does not change the running app and starts no deployment. The response states that the change reaches the app on its next deploy.
- **AC-9**: `deploy_app` accepts an optional `config` map, validated by exactly the same rules as `set_config` and merged into the app's configuration before the deployment row is created. A `deploy_app` call carrying no map leaves the configuration untouched.
- **AC-10**: The release cut when a deploy becomes healthy snapshots the configuration that deploy actually ran with.
- **AC-11**: `get_logs` blanks any literal occurrence of a secret value that is eight characters or longer, matched against the union of the app's current secret values and the values the current release ran with for keys that are secret today, so a rotated secret the running pod already printed is still blanked. A value shorter than eight characters is not literal matched, and the logs are otherwise unchanged.
- **AC-12**: Every configuration change writes an audit row with `target_type` of `app_config` and `target_id` of the app id and the key joined by a slash, so both survive in the one pair the table has. No row ever contains a value.
- **AC-13**: A caller who does not own the app receives the same `app_unknown` refusal every other tool gives, for all three tools, so no tool tells a caller whether someone else's app exists.
- **AC-14**: No build receives configuration. Neither the Buildpacks path nor the Dockerfile path is given build arguments or build secrets.
- **AC-15**: An empty string is a valid value, stored and injected as an empty variable, and is not the same thing as the key being unset.
- **AC-16**: The secret flag is required on every key of every `set_config` and `deploy_app` call. A key sent without it is refused with `config_flag_missing`, so re setting an existing key can never quietly turn a secret into a plain value.
- **AC-17**: The pod template carries an annotation holding a checksum of the configuration it was composed with, so a deploy whose image digest is unchanged still rolls the pods and the new values actually reach the container.

## Decision

**Chosen option**: Option 2: A stored configuration surface, applied at the next deploy, injected through one Kubernetes Secret per app.

Configuration is a small set of key and value pairs owned by the app row, written by three tools, read once when a deploy composes the workload, injected through one Secret, and snapshotted onto the release.

**Implementation skills**: `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`) · `security-patterns` (`~/.claude/skills/security-patterns/`) · `golang-patterns` (`~/.claude/skills/golang-patterns/`) · `mcp-server-patterns` (`~/.claude/skills/mcp-server-patterns/`)

## Rationale

Reasoning and options: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

No migration. Spec 0002 designed the storage already and it fits this decision without a change:

| Table | Column | Notes |
|---|---|---|
| `app_config` | `app_id` | references `apps(id)`, part of the primary key |
| | `key` | `CHECK (key GLOB '[A-Z_][A-Z0-9_]*')`, part of the primary key |
| | `value` | required, an empty string is valid (AC-15) |
| | `is_secret` | 0 or 1, default 0 |
| | `created_at`, `updated_at` | required |
| `releases` | `config_snapshot` | JSON of every key and value at the moment the release was cut, secrets in clear (AC-10) |

The queries exist too: `SetConfig` upserts, `UnsetConfig` deletes and reports the row count, `ListConfigForResponse` nulls a secret value, `ListConfigForDeploy` returns everything. What the store lacks is a transactional way to write several keys at once, so `internal/store` gains `SetConfigBatch` and `UnsetConfigBatch`, both wrapping the existing single key queries in the store's `inTx` helper, which is what makes AC-1 and AC-3 buildable. Beyond that only `internal/domain`, `internal/mcp`, `internal/deploy` and `internal/logs` gain code.

**State transitions**: none. Configuration is a set of rows, not a machine. The deployment state machine is untouched, because a configuration change starts no deployment (AC-8).

**API surface**:

| Tool | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|
| `set_config` | `name` (req), `config`: map of key to `{value, secret}` (req, at least one entry, `secret` required on every entry) | `config`: the resulting list, `applies_on_next_deploy: true` | account token | `app_unknown`, `config_key_invalid`, `config_key_reserved`, `config_flag_missing`, `config_too_many_keys`, `config_too_large` |
| `unset_config` | `name` (req), `keys`: list of key (req, at least one) | `config`: the resulting list, `applies_on_next_deploy: true` | account token | `app_unknown`, `config_key_unknown`, `config_key_invalid` |
| `get_config` | `name` (req) | `config`: the list, each entry `{key, secret, value}` with `value` null when secret | account token | `app_unknown` |
| `deploy_app` | existing inputs plus `config` (opt, same shape as `set_config`) | existing outputs, unchanged | account token | the existing set plus every `config_*` code above |

A refusal is one of the closed reason codes in `internal/domain/reason.go`, never a wrapped error string, and the codes above are new members of that set.

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| Compose the app's container | `PORT` | The constant `deploy.ContainerPort`, as today. Never from `app_config` (AC-5) |
| Compose the app's container | `APP_URL` | `"https://" + Input.Host`, built in the same function that builds the Ingress rule, so the two can never disagree (AC-7) |
| Compose the app's container | every other variable | `envFrom` the Secret named by the new `deploy.ConfigSecretName` constant (value `config`), sitting beside the existing `PullSecretName`, whose data is `ListConfigForDeploy` read at compose time |
| Compose the app's Secret | the values | `ListConfigForDeploy(app_id)`, read once when the workload is composed, so a `set_config` landing mid build affects the next deploy, not this one |
| Compose the pod template | the config checksum annotation | A SHA-256 of the Secret's data, sorted by key, written as an annotation on the pod template so an unchanged image digest still rolls the pods (AC-17) |
| Cut a release | `config_snapshot` | The existing `configSnapshot` helper, inside the release transaction |
| `get_config`, `set_config`, `unset_config` response | the listed values | `ListConfigForResponse`, which nulls a secret value in SQL rather than in Go, so no code path can forget (AC-2) |
| `get_logs` redaction | the literal strings to blank | The union of two sets, both filtered to values of `logs.minLiteral` characters or more: the app's current secret values from `ListConfigForDeploy(app_id)`, and the values in the current release's `config_snapshot` for keys that `app_config` marks secret today. Passed as the `literals` argument `logs.Redact` already takes (AC-11) |
| Every refusal | the reason code | The closed set in `internal/domain/reason.go`, gaining `config_key_invalid`, `config_key_reserved`, `config_key_unknown`, `config_flag_missing`, `config_too_many_keys`, `config_too_large` |
| Every audit row | the target | `target_type` is the literal `app_config` and `target_id` is `app.ID + "/" + key`, because the table has one target pair and this change has two things worth naming. Never the value (AC-12) |

**Key invariants**:

- A secret value leaves the process in exactly two directions: into the Kubernetes Secret, and into the release snapshot. Never into a response, never into a platform log line, never into an audit row.
- `PORT` and `APP_URL` are composed by the platform and cannot be stored, so what the container sees for them never depends on what a caller sent.
- Validation happens in `internal/domain` before any write, so the database `CHECK` is a backstop rather than the error reporter.
- A refused call is atomic: a `set_config` carrying five keys where one is invalid writes none of the five. Validation runs over the whole call first, then one transaction writes it.
- A key never loses its secret flag by omission, because the flag is required on every write (AC-16).
- The values a container is running are always the values the pod template's checksum was taken from, so a Secret rewrite that does not change the checksum cannot leave a pod running stale values (AC-17).
- A `deploy_app` call carrying configuration writes that configuration before it creates the deployment row, and a failure creating the row leaves the configuration written. The two are deliberately not one transaction, because a stored value that does not yet run is the normal state of this feature anyway (AC-8).
- No user supplied string is ever merged into a pod spec. Values live in a Secret's data, which the Deployment references by name, so the pod spec still holds only platform derived strings.

**Security model**:

- All three tools resolve the app by name within the calling account, the same path `deployment_status` and `get_logs` already use. A miss and another account's app are the same `app_unknown` refusal (AC-13).
- The Secret lives in the app's own namespace. The app has no Kubernetes token at all (`AutomountServiceAccountToken` is false), so it cannot read the Secret object, only the environment it was started with.
- The Secret counts against the app namespace's existing `ResourceQuota` allowance of ten secrets, alongside the image pull secret. Two used of ten.
- The control plane already creates Secrets in an app namespace for the image pull credential, so its RBAC needs no widening.
- Secrets sit unencrypted at rest in the platform's SQLite file and, by k3s default, in etcd. That is the same exposure the release snapshot already carries, and closing it is the deferred backup and restore item, not this feature.

**Configuration required**: none. The bounds are Go constants in `internal/domain`, not `DEPLOYER_*` variables, for the same reason the logs bounds are constants: they are product decisions about what an app should carry, not knobs for whoever runs the platform.

**Critical test scenarios**:

- Happy path: set two keys, one secret, deploy, and the container's environment holds both plus `PORT` and `APP_URL`, verifies **AC-1**, **AC-7**.
- Secrecy: the secret key comes back with a null value from `set_config` and from `get_config`, verifies **AC-2**.
- Reserved: setting `PORT` or `APP_URL` is refused with `config_key_reserved` and the stored configuration is unchanged, verifies **AC-5**.
- Atomicity: a `set_config` with three valid keys and one over the size bound writes nothing, verifies **AC-6**.
- Failure case: `unset_config` on a key never set is refused with `config_key_unknown`, verifies **AC-3**.
- Redaction: an app that prints its own secret value has that line blanked, and an app whose secret is `dev` has its logs left alone, verifies **AC-11**.
- Rotation: a secret set, deployed, printed by the app, then changed, is still blanked from the running pod's logs, verifies **AC-11**.
- Flag omission: re setting an existing secret key without the secret flag is refused with `config_flag_missing` and the key stays secret, verifies **AC-16**.
- Rollout: two deploys of the same source with different configuration produce different pod template checksums, verifies **AC-17**.
- Timing: a `set_config` between two deploys shows in the second and not the first, verifies **AC-8**, **AC-10**.
- Auth: another account calling all three tools on this app gets `app_unknown`, verifies **AC-13**.

## Build plan

Tracer Bullet, so the first slice is one value travelling the whole way from an MCP call to a running container, and everything after it thickens that thread.

1. Add the `config_*` reason codes to the closed set in `internal/domain/reason.go`, and a `config.go` there holding key validation, the reserved names (`PORT`, `APP_URL`), the required secret flag rule, and the bound constants, written test first, satisfies **AC-4**, **AC-5**, **AC-6**, **AC-16**.
2. Add `SetConfigBatch` and `UnsetConfigBatch` to `internal/store`, both transactional over the existing single key queries, so a partly valid call writes nothing, satisfies **AC-1**, **AC-3**.
3. Compose the app's Secret in `internal/deploy`: a `Secret` function, the `ConfigSecretName` constant, a `Config` field on `Input`, `envFrom` on the container, the checksum annotation on the pod template, and `APP_URL` built from `Input.Host` beside `PORT`, satisfies **AC-7**, **AC-15**, **AC-17**.
4. Apply the Secret from the reconcile loop before the workload, reading the live rows with `ListConfigForDeploy` at compose time, satisfies **AC-7**, **AC-10**.
5. Add `set_config` and `get_config` as MCP tools, with ownership resolution, validation, the audit rows in the `app_config` target shape, and the `applies_on_next_deploy` line in the response. This closes the thread: a call sets a value and a deploy runs it, satisfies **AC-1**, **AC-2**, **AC-8**, **AC-12**, **AC-13**.
6. Add `unset_config`, including the refusal when any key in the call is not set, satisfies **AC-3**.
7. Thread the secret literals into `get_logs`, as the union of the current secret values and the running release's snapshot values for keys that are secret today, filtered by `logs.minLiteral`, using the `literals` argument `logs.Redact` already accepts, satisfies **AC-11**.
8. Add the optional `config` map to `deploy_app`, sharing one validation and write path with `set_config`, satisfies **AC-9**.
9. Update the tool descriptions: `deploy_app`'s gains the config argument and the reserved names, and the app contract line about `PORT` becomes `PORT` and `APP_URL`. The description is contract, not decoration, and nothing tests the drift, satisfies **AC-9**, **AC-5**.
10. Confirm in a test that no build path receives configuration, so the deliberate absence has something holding it in place, satisfies **AC-14**.

## Consequences

**Positive**:

- Apps that need credentials become deployable at all, which is most real apps.
- Exact log redaction finally becomes possible, because the platform now knows which strings are secret rather than guessing at shapes. Spec 0006 left this owed and it is paid here.
- The release snapshot stops being theoretical. A release now records a real image and a real configuration, which is what slice 8's rollback needs to re promote an exact prior state.
- No migration and no schema change, because spec 0002 designed the table for this a slice at a time ago.
- `APP_URL` removes a whole class of first run failure, where an app cannot build a link to itself.

**Negative and tradeoffs**:

- A configuration change does nothing until the next deploy, which will surprise somebody. The response says so, but a response line is weaker than the platform simply doing it. Making it automatic needs the no build redeploy path, which is slice 8's machinery, so this is a deliberate wait rather than a decision against.
- The platform is now unambiguously a secret store. Values sit in clear in the SQLite file, in every release snapshot, and in etcd, and releases are never pruned. The deferred backup and restore item now guards genuinely sensitive data.
- Marking a value secret is the caller's job, so an agent that forgets the flag puts a credential in a response and in the logs. Chosen over guessing from key names, because a heuristic that misses one key fails silently in exactly the same way while looking safe.
- A secret shorter than eight characters is not redacted from logs. That is a real hole, accepted because redacting a short string destroys the log without protecting anything worth protecting.
- Log redaction covers a rotated value only while its key is still marked secret. A key flipped from secret to plain stops protecting anything the running pod printed under the old value. Accepted because the alternative is recording secrecy per release, which the snapshot format does not carry today.
- Dockerfile builds still cannot reach a private package registry, because no build receives values. That is the trade spec 0009 set up and this spec confirms.

**Neutral**:

- One more Kubernetes object per app to compose, apply, and reason about. The pull secret already established the pattern.
- Three more tools in the agent's list, which is context an agent spends on every call. The alternative, one tool with an action argument, spends less context and buys more misuse.
- `deploy_app`'s description grows, and it is already the longest one.

## Follow-up

- [ ] The deferred backup and restore item should now name configuration explicitly. The database holds live credentials, not just metadata.
- [x] Slice 8's rollback must rewrite the app's Secret from the release snapshot, not only re promote the image digest. A rollback that restores an old image beside today's configuration is not a rollback. The pod template checksum this spec adds is what will make that rollback actually roll the pods. Closed by spec [0011](../0011-rollback-and-release-history/index.md): a rollback composes the Secret from the source release's snapshot, and the checksum is what rolls the pods when the digest is unchanged.
- [ ] The deferred egress by hostname item becomes expressible once an app can declare configuration. Worth revisiting after this ships.
- [ ] Decide, when a build cache or a private dependency need actually appears, whether BuildKit build secrets are worth the split behaviour between the two build paths. Not now.
