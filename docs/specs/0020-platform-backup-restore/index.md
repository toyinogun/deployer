# 0020. Platform backup and restore: a snapshot the platform takes of itself, encrypted to a key it does not hold

**Date**: 2026-08-15
**Status**: In Progress

The decision record (context, options considered, rationale) is in [rationale.md](rationale.md).

## Summary

Right now one lost volume is every account, every app, and every secret anyone ever set, because nothing copies the platform's database anywhere. This spec makes the control plane back itself up: once a day it takes a consistent snapshot of its own database with one SQLite statement, checks the snapshot is sound, encrypts it with age (a small encryption library that scrambles data to a public key), uploads it to a Cloudflare R2 bucket, then reads the object back to confirm the bytes arrived intact. The private key that could decrypt any of it never enters the cluster, so the day the cluster is gone is the day you still have your backups. Every run is recorded in a new table, shown on the admin page, and a failure mails you. The registry volume is covered separately by Longhorn, and restore is a subcommand of the same binary that you run on your own machine.

## Requirements

**User stories**:
- As the platform owner, I want the database copied somewhere that is not this cluster every day, so that a volume that does not come back is an afternoon rather than the end of the platform.
- As the platform owner, I want the backup encrypted to a key that only I hold, so that a bucket credential leaking does not hand somebody every password hash, token, and app secret on the platform.
- As the platform owner, I want to be told when a backup fails, so that I never discover on restore day that it stopped six weeks ago.
- As the platform owner, I want to see when the last backup ran and fire one by hand before a risky change, so that I am not reading logs to answer a question the page could answer.
- As the platform owner, I want a restore I have actually performed rather than one I have written down, so that the procedure is proven before it matters.

**Acceptance criteria** (the contract, each criterion is IDed and independently checkable):

*The snapshot*

- **AC-1**: The snapshot is produced by `VACUUM INTO` executed through the running process's own database handle, never by copying or reading the file from outside. Nothing is quiesced, no request is blocked, and the platform keeps serving throughout.
- **AC-2**: Every temporary file the run makes lives under a single fixed directory, `/data/backup-tmp/`, never the `/tmp` mount. Both files are deleted on every exit path, success and failure alike, including the paths that return early on error. A cleanup that itself fails is logged at warn and does not change the run's outcome.
- **AC-2a**: `/data/backup-tmp/` is emptied at startup, in the same step as the stranded row sweep in AC-9. A hard kill runs no exit path, so a crash mid run otherwise leaves a full unencrypted copy of every password hash, session, token, and app secret sitting on the volume indefinitely, visible to no surface this spec builds. The sweep is what closes that, and it is the reason the temp directory is fixed and exclusive rather than a random path.
- **AC-3**: Before anything is encrypted, the snapshot is opened as a separate read only database and checked: `PRAGMA integrity_check` must return exactly `ok`, and the `accounts` table must hold at least one row. Either check failing ends the run failed and uploads nothing.

*Encryption and upload*

- **AC-4**: The snapshot is encrypted with age to the recipient in `DEPLOYER_BACKUP_AGE_RECIPIENT` before a single byte leaves the pod. The platform holds only the public recipient. No configuration value, no Secret, and no code path gives the cluster the private identity, so the platform cannot read its own backups and neither can anything that compromises it.
- **AC-4a**: age is the Go library `filippo.io/age`, linked into the binary. It is never a CLI invoked as a subprocess: `ko` builds a minimal image with no shell and no extra binaries, so a subprocess would be a change to how the image is built as well as a second thing to pin.
- **AC-4b**: Encryption streams from the snapshot file into a second file under `/data/backup-tmp/`, computing the SHA-256 as it writes. The ciphertext is never held whole in memory. The run therefore knows the exact length and hash of the object before the upload begins, and the upload is a plain file upload of known size.
- **AC-4c**: After encrypting, the age header of the produced file is parsed and confirmed to carry a stanza for the configured recipient. This needs no private key and it is the only check that catches encrypting to the wrong recipient, which otherwise produces a well formed object that passes every other check and decrypts for nobody.
- **AC-5**: The encrypted object is uploaded to the configured bucket under the key `db/<run started at, as YYYYMMDDTHHMMSSZ in UTC>-<run id>.age`. The compact timestamp is deliberate: RFC3339 carries colons, which are legal in an S3 key and awkward in a shell argument, and this key is typed by hand into `deployer restore`. The key is composed from values the run already holds, so two runs cannot collide and no listing is needed to choose one.
- **AC-6**: After upload the object is read back from the bucket and its byte length and SHA-256 compared against what was written. A mismatch ends the run failed with the verify reason. The object is left in place, not deleted: a partly readable backup is worth more than none, and the retention rule in AC-6a would refuse the delete anyway.
- **AC-6a**: The bucket carries an object retention rule of 7 days, so no caller can delete a backup in its first week regardless of what its credential permits. This replaces what an earlier draft asked for, a credential scoped to read and write but not delete, which R2 cannot express: its token presets are Admin Read and Write, Admin Read only, Object Read and Write, and Object Read only, and the Object Read and Write preset includes `DeleteObject`. Retention is the stronger property in any case, because it holds against a token somebody re-issues later rather than depending on nobody doing so. Seven days and not thirty, because retention and the 30 day expiry rule have to coexist and a lock as long as the expiry leaves the two fighting at the boundary.

*The record*

- **AC-7**: Every run is a row in `backup_runs`, inserted as `running` before the snapshot is taken and updated exactly once when the run ends. The state write lands before the action it describes, per the project rule.
- **AC-8**: A partial unique index permits at most one `backup_runs` row with `outcome = 'running'`. A second run is refused by the database rather than by a check in Go, the same way `deployments_one_in_flight` already works.
- **AC-8a**: The refusal is recognised by matching the unique constraint violation specifically, never by treating any insert failure as one. A full volume, a locked database, or any other write fault is `internal` and is surfaced and alerted as such. Reporting a real fault to an admin as a benign concurrency refusal is how this feature would lie about its own health.
- **AC-9**: At startup, before the process serves anything, any `backup_runs` row still `running` is ended `failed` with the stranded reason. One replica and a `Recreate` strategy make such a row definitionally dead, so there is no sweeper, no grace period, and no timer.
- **AC-10**: A backup failure reason is one of a closed set defined in `internal/domain`, as its own type, separate from the deploy `Reason`. The codes are `snapshot_failed`, `integrity_failed`, `encrypt_failed`, `upload_failed`, `verify_failed`, `stranded`, and `internal`. The wrapped error never reaches the row, the page, or the platform log at info level.
- **AC-11**: Run rows are never pruned, deliberately, matching how releases already accumulate. One row a day is a cost the database can carry indefinitely, and a prune is code that can delete the wrong thing.

*Schedule and alerting*

- **AC-12**: A ticker in the control plane process fires a backup every `DEPLOYER_BACKUP_INTERVAL_SECONDS`, defaulting to 86400. At startup the process runs one immediately if the most recent successful run is older than that interval, so a pod that restarts daily still gets backed up. The interval is measured from the end of each run, catch up run included, so the schedule never lands a tick on the heels of a run that just finished.
- **AC-12a**: The ticker checks for a run already in flight and skips its turn with a log line rather than attempting an insert it knows will be refused. A backup is already happening, which is what the tick wanted, and AC-12's catch up covers the case where that run then fails. The refusal path in AC-8 exists for the button, which has a caller to answer to; a scheduled tick has none, so it must not be able to vanish into a swallowed constraint error.
- **AC-13**: A failed run sends one email to `DEPLOYER_BACKUP_ALERT_EMAIL` through the existing Resend path, naming the reason code and nothing else. The first success after any failure sends a recovery email. A run that succeeds after a success sends nothing, so silence means healthy. The very first run the platform ever takes has no predecessor, and is never a recovery.
- **AC-14**: The mail carries no object key, no bucket name, and no configuration value. A failure notification is not a place to leak where the backups are.

*Not configured*

- **AC-15**: With no bucket configured the platform boots, logs one warning at startup naming exactly which values are missing, takes no backups, records no runs, and works in every other respect. This matches how the Resend key and the bootstrap token already behave.
- **AC-16**: `internal/config` validates the backup values at startup, never at first use. The interval is optional and parses as a positive integer. The region is optional and defaults to `auto`. Six values form one all or nothing group: the age recipient, the endpoint, the bucket, the access key, the secret key, and the alert email. Present them all and backups are on; present none and backups are off; present some and the process refuses to start. The alert email is inside the group deliberately, because backups configured with nowhere to complain to is the precise failure this feature exists to prevent. The recipient is parsed here, once, not at first use.

*The surfaces*

- **AC-17**: `GET /admin/backups` renders the most recent runs: started at, finished at, outcome, size, object key, and reason where there is one. It goes through the same `adminSession` gate every other admin page uses, so a signed in non admin and a signed out visitor get exactly what that gate already gives them and no hint the page exists.
- **AC-18**: The page renders a clear not configured state when no bucket is set, rather than an empty table that reads as healthy.
- **AC-19**: `POST /admin/backups/run` starts a backup outside the schedule. It carries the same session bound CSRF token the other admin write actions carry, and a post without a valid one is refused before anything runs.
- **AC-20**: A manual run while one is already in flight is refused, the caller is told why with the closed reason code, and nothing is written beyond the audit row. The refusal comes from the unique index in AC-8, not from a read followed by a write.
- **AC-21**: A manual run writes `trigger = 'manual'` and `triggered_by` set to the admin's account id. A scheduled run writes `trigger = 'schedule'` and leaves `triggered_by` null.
- **AC-22**: Every press of the button writes an `audit_log` row, both when it is allowed and when it is refused, alongside the existing admin actions.

*Restore*

- **AC-23**: `deployer restore` is a subcommand of the same binary, intended to run outside the cluster. Given an object key, the bucket configuration, and a path to an age identity file, it fetches the object, decrypts it, runs `PRAGMA integrity_check` on the result, and writes the plaintext database to a local path. The identity is read from a file named by a flag, never from an environment variable and never from an argument, so it does not land in a process listing.
- **AC-24**: `deployer restore` refuses to write to a destination path that already exists. It restores to a file and nothing more; getting that file onto the volume is a documented step, not something the subcommand does. There is no attempt to detect whether it is running inside a cluster, because no reliable signal for that exists and a wrong guess either blocks a legitimate restore or fails to block anything.
- **AC-25**: The restore has been rehearsed, not assumed: a real backup object is restored into a scratch instance and you sign in to it with an account that existed when the backup was taken. This is a `/check verify` criterion against the real cluster, not a unit test.
- **AC-26**: A runbook covers the whole path end to end in `deploy/README.md`: scaling the control plane down, placing the restored file, scaling back up, and what to check afterwards.

*The other two stores*

- **AC-27**: The registry volume is backed up by a Longhorn recurring job targeting the same bucket, configured in the `k3sprox-gitops` repository, with a retention of 30. No Go code in this repository touches it. It is listed here so the feature is not called done with the images missing.
- **AC-28**: The sealed secrets controller key is exported by hand and stored off cluster beside the age identity. The runbook names the exact command and says to repeat it whenever the controller rotates its key. The platform is granted no new cluster permission for this, and its RBAC stays scoped per app namespace exactly as it is.
- **AC-29**: `/data/uploads` is explicitly out of scope. Source tarballs are short lived and replaceable by the agent that uploaded them, and including them would put unbounded user supplied content in the daily object.

## Decision

**Chosen option**: Option 3: the control plane backs itself up, Longhorn covers the registry, the sealed secrets key is exported by hand.

The control plane takes a consistent snapshot of its own database with `VACUUM INTO`, verifies it while it is still plaintext on disk, encrypts it with age to a public recipient whose private half never enters the cluster, uploads it to Cloudflare R2, reads it back to confirm the bytes, and records the run in a new `backup_runs` table that the admin page reads and a failure mails about.

**Implementation skills**: `security-patterns` (`~/.claude/skills/security-patterns/`) · `database-migrations` (`~/.claude/skills/database-migrations/`) · `senior-kubernetes-engineer` (`~/.claude/skills/senior-kubernetes-engineer/`)

## Rationale

See [rationale.md](rationale.md).

## Feature design

**Data model sketch**:

One new table. A backup is of the whole database, not of anything in it, so it relates to nothing except the admin who fired it by hand.

| Column | Type | Null | Notes |
|---|---|---|---|
| `id` | TEXT | no | primary key, the same id shape every other table uses |
| `started_at` | TEXT | no | RFC3339 UTC, also the timestamp in the object key |
| `finished_at` | TEXT | yes | null while running |
| `outcome` | TEXT | no | `CHECK (outcome IN ('running', 'succeeded', 'failed'))` |
| `object_key` | TEXT | yes | set once the object lands |
| `size_bytes` | INTEGER | yes | of the encrypted object |
| `checksum` | TEXT | yes | SHA-256 of the encrypted object, what the read back compares |
| `failure_reason` | TEXT | yes | one of the closed codes in AC-10, never a wrapped error |
| `trigger` | TEXT | no | `CHECK (trigger IN ('schedule', 'manual'))` |
| `triggered_by` | TEXT | yes | `REFERENCES accounts(id) ON DELETE RESTRICT`, set for manual runs only |

Indexes:
- `CREATE UNIQUE INDEX backup_runs_one_in_flight ON backup_runs(outcome) WHERE outcome = 'running'`
- `CREATE INDEX backup_runs_started ON backup_runs(started_at DESC)`

Table is `STRICT`, matching every existing table.

**State transitions**:

`running` → `succeeded` on a run that snapshotted, checked, encrypted, uploaded, and verified.
`running` → `failed` on any step failing, or at startup when the process finds the row left behind by a dead predecessor.

There is no other transition. A terminal row is never rewritten.

**API surface**:

| Endpoint | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/admin/backups` | GET | none | recent runs, configured state | session, admin | whatever `adminSession` already gives a non admin |
| `/admin/backups/run` | POST | CSRF token (req) | redirect with a message | session, admin | refused when a run is in flight; refused on a bad CSRF token |
| `deployer restore` | CLI | `-key` object key (req), `-identity` path to age identity file (req), `-out` destination path (req) | a plaintext database file | the identity file itself | destination exists; decrypt failed; integrity check failed |

**Value sourcing**:

| Action | Value produced / displayed | Source |
|---|---|---|
| scheduled run | the run id | generated the same way every other id in the platform is |
| scheduled run | `started_at`, and the timestamp half of the object key | the clock at insert, held once as one value, rendered RFC3339 for the column and `YYYYMMDDTHHMMSSZ` for the key, so the two can never disagree |
| scheduled run | the object key | composed from `started_at` and `id`, both already on the row |
| scheduled run | `size_bytes` | the byte length of the encrypted temp file, known before the upload starts because encryption streams to disk (AC-4b) |
| scheduled run | `checksum` | SHA-256 computed over the ciphertext as it is written to that temp file, not recomputed at upload |
| scheduled run | the age recipient | `DEPLOYER_BACKUP_AGE_RECIPIENT`, parsed once at startup |
| scheduled run | whether the ciphertext names the right recipient | the age header of the file just written, parsed back (AC-4c) |
| scheduled run | whether to run at all this tick | a read for a `backup_runs` row with `outcome = 'running'`, before any insert is attempted |
| startup catch up | whether a run is due | the newest `backup_runs` row with `outcome = 'succeeded'`, its `finished_at` compared against `DEPLOYER_BACKUP_INTERVAL_SECONDS` |
| failure mail | the recipient address | `DEPLOYER_BACKUP_ALERT_EMAIL`, required whenever backups are configured at all |
| failure mail | whether this is a recovery mail | the previous terminal run's `outcome`, read before the current row is written; absent on the first run ever, which is never a recovery |
| admin page | not configured state | the parsed config, not the absence of rows, so a configured platform that has never run yet reads as pending rather than unconfigured |
| manual run | `triggered_by` | the account id on the admin's session |
| restore | the plaintext database | the fetched object decrypted with the identity file at `-identity` |

**Key invariants**:

- At most one `backup_runs` row is `running`, enforced by the partial unique index, not by application code.
- A terminal row is never rewritten. A process that finds a row already terminal leaves it alone, matching how a drive that finds its deployment row terminal stops quietly.
- The private age identity exists nowhere inside the cluster: not in a Secret, not in a ConfigMap, not in an environment variable, not on the volume.
- A backup cannot be deleted in its first 7 days by anything, the platform included, because the bucket's retention rule refuses it. Cleanup after 30 days is a bucket lifecycle rule, so no code in the cluster ever issues a delete.
- Nothing is uploaded that has not first passed `PRAGMA integrity_check` as plaintext and named the configured recipient in its age header.
- No plaintext copy of the database outlives the run that made it, whether that run exited or was killed. `/data/backup-tmp/` is emptied on every startup, so the invariant survives a crash rather than depending on one.
- No app output, no build output, and no log content passes through this path. The backup is the database file and nothing else.

**Security model**:

Reading the backup surface is admin only, through the existing `adminSession` gate. Firing a backup is admin only and carries the same session bound CSRF token every other admin write action carries, and both outcomes land in `audit_log`. Nothing about backups is exposed over MCP: the agents that call this platform deploy apps, they do not operate it, and an API token's blast radius does not need widening.

The sensitive data is the backup object itself, which is a full copy of every password hash, session, API token, and clear text app configuration value on the platform. It is protected by encrypting to a public recipient before it leaves the pod, so the bucket, the provider, and anything that compromises the control plane all hold only ciphertext. The private identity is held off cluster by the operator.

The bucket credential is scoped to one bucket with R2's Object Read and Write preset, the least powerful one that can both upload and read back. That preset carries `DeleteObject`, which R2 gives no way to remove, so the protection against a compromised pod destroying backup history is the bucket's own 7 day retention rule (AC-6a) rather than the credential. Defence sits with the bucket, not with the token, which is where it survives somebody minting a fresher token later.

Reading old backups is a real residual risk and it is accepted: the alternative, dropping the read back verify, gives up the rehearsed rather than assumed requirement this feature exists to satisfy. What a compromised pod reads is ciphertext in any case, because it never holds the age identity.

No regulatory scope applies. This is a personal homelab platform with invite only registration.

**Configuration required**:

Optional, with defaults:
- `DEPLOYER_BACKUP_INTERVAL_SECONDS`: how often a backup runs. Defaults to 86400. Validated as a positive integer.
- `DEPLOYER_BACKUP_S3_REGION`: defaults to `auto`, which is what R2 wants.

The all or nothing group (all six, or none):
- `DEPLOYER_BACKUP_AGE_RECIPIENT`: the age public recipient backups are encrypted to. Parsed at startup. Never the private identity, which has no variable at all.
- `DEPLOYER_BACKUP_S3_ENDPOINT`: the R2 S3 compatible endpoint.
- `DEPLOYER_BACKUP_S3_BUCKET`: the bucket name.
- `DEPLOYER_BACKUP_S3_ACCESS_KEY_ID`: from a SealedSecret, marked optional on the Deployment the way the Resend key is.
- `DEPLOYER_BACKUP_S3_SECRET_ACCESS_KEY`: from the same SealedSecret.
- `DEPLOYER_BACKUP_ALERT_EMAIL`: where failure mail goes.

Prerequisites before coding begins, written out step by step in `deploy/README.md`: an age key pair with the private half stored off cluster and nowhere else, an R2 bucket, an API token scoped to that one bucket with the Object Read and Write preset, a 7 day object retention rule, and a lifecycle rule expiring objects under the `db/` prefix after 30 days. The prefix on the lifecycle rule is load bearing: the registry volume backups land in the same bucket and must not be swept by it.

**Critical test scenarios**:

- Happy path: a run against a real SQLite file in a temp dir snapshots, checks, encrypts, uploads to a fake object store, reads back, and lands a `succeeded` row carrying a size and a checksum, verifies **AC-1**, **AC-3**, **AC-4**, **AC-5**, **AC-6**, **AC-7**.
- Round trip: a backup taken by the code under test is decrypted by the restore path with the matching identity and opens as a database whose accounts match the source, verifies **AC-23**.
- Failure case: an upload that succeeds and reads back short ends the row `failed` with `verify_failed`, leaves the object alone, and sends one email, verifies **AC-6**, **AC-13**.
- Failure case: encrypting to a recipient other than the configured one is caught by the header check and uploads nothing, verifies **AC-4c**.
- Failure case: a manual run started while one is in flight is refused by the unique index and writes nothing beyond the audit row, verifies **AC-8**, **AC-20**, **AC-22**.
- Failure case: an insert that fails for a reason other than the unique constraint ends the run `internal` and alerts, rather than reading as a concurrency refusal, verifies **AC-8a**.
- Failure case: a tick arriving while a run is in flight skips without inserting, and the following tick runs normally, verifies **AC-12a**.
- Failure case: a process starting with a `running` row left behind ends it `failed` with the stranded reason before serving, and the next backup then succeeds, verifies **AC-9**.
- Failure case: a snapshot that fails integrity uploads nothing at all, verifies **AC-3**.
- Failure case: a partially configured platform refuses to start, including one configured for a bucket but given no alert email; an entirely unconfigured one starts, warns, and serves normally, verifies **AC-15**, **AC-16**.
- Cleanup: both temp files are gone after a successful and a failed run, verifies **AC-2**.
- Cleanup: a process starting with files left in `/data/backup-tmp/` by a killed predecessor empties it before serving, verifies **AC-2a**.
- Auth/permission: a signed in non admin and a signed out visitor get the existing admin gate's answer on both routes, and a post with no valid CSRF token is refused before any run starts, verifies **AC-17**, **AC-19**.
- Leak: no backup object key, bucket name, or credential appears in any rendered page a non admin can reach, nor in the failure mail, verifies **AC-14**.

## Build plan

Tracer Bullet, the project's approach: the first task carries a real backup all the way from the live database to a real object in the bucket and back to a row, so the whole pipe is proven before any of it is thickened. Everything after that adds a property to a path that already works end to end.

1. [x] The migration for `backup_runs`, its `CHECK` constraints, the partial unique index and the started index, plus the sqlc queries the later tasks read and write through, satisfies **AC-7**, **AC-8**, **AC-11**.
2. [x] The thin thread, end to end and nothing more: a manually invoked run that inserts the `running` row, takes the `VACUUM INTO` snapshot into `/data/backup-tmp/`, stream encrypts it to a second file there with `filippo.io/age` while hashing, uploads that file through minio-go, and closes the row `succeeded` with a key, a size and a checksum. Proved against a real SQLite file and a fake object store, then once against the real bucket, satisfies **AC-1**, **AC-2**, **AC-4**, **AC-4a**, **AC-4b**, **AC-5**, **AC-7**.
3. [x] The closed reason set as its own type in `internal/domain`, the failure paths through the thread ending the row with a code rather than an error string, and the insert failure path telling the unique constraint violation apart from every other write fault, satisfies **AC-8a**, **AC-10**.
4. [x] The three checks that make it a backup rather than an upload: `PRAGMA integrity_check` and the account count on the plaintext snapshot, the age header parsed back to confirm the recipient, and the read back comparing length and SHA-256 after upload with the object left in place on mismatch, satisfies **AC-3**, **AC-4c**, **AC-6**.
5. [x] Configuration: the two optional values with their defaults and the six value all or nothing group validated at startup in `internal/config`, the recipient parsed there, and the unconfigured platform booting with one warning and no runs, satisfies **AC-15**, **AC-16**.
6. [x] The schedule and the two sweeps: the ticker measured from the end of each run, its skip when a run is in flight, the startup catch up when the newest success is older than the interval, the startup sweep ending a stranded `running` row, and the startup emptying of `/data/backup-tmp/`, all before the process serves, satisfies **AC-2a**, **AC-9**, **AC-12**, **AC-12a**.
7. [x] Alerting through the existing Resend path: one mail on failure, one on the first success after a failure, silence otherwise, carrying the reason code and nothing else, satisfies **AC-13**, **AC-14**.
8. [x] The admin surfaces: the read only page behind `adminSession` with its not configured state, the run now post with the session CSRF token, the in flight refusal surfaced with its reason, the trigger and `triggered_by` columns written correctly, and the `audit_log` row on both outcomes, satisfies **AC-17**, **AC-18**, **AC-19**, **AC-20**, **AC-21**, **AC-22**.
9. [x] `deployer restore` as a subcommand: fetch, decrypt from an identity file named by a flag, integrity check, write to a destination that must not already exist, satisfies **AC-23**, **AC-24**.
10. The parts that live outside this repository, plus the leftovers: the bucket's 7 day retention rule and its prefixed 30 day lifecycle rule, the Longhorn recurring job for the registry volume in `k3sprox-gitops`, the SealedSecret carrying the bucket credential marked optional on the Deployment, the manual sealed secrets key export, and the runbook in `deploy/README.md` covering setup and restore end to end plus the uploads exclusion, satisfies **AC-6a**, **AC-26**, **AC-27**, **AC-28**, **AC-29**.
11. The rehearsal: a real object restored into a scratch instance and signed in to, against the real cluster, satisfies **AC-25**.

## Consequences

**Positive**:
- A lost volume stops being the end of the platform. Worst case is a day of registrations and configuration changes.
- The snapshot is consistent by construction. There is no quiescing, no downtime, and no class of restore that lands mid transaction.
- The platform can answer whether its own backups work, which is the property no external mechanism could have given it.
- Encrypting to a public recipient means the bucket, the provider, and a compromised control plane all hold ciphertext only.
- A compromised pod cannot delete a backup in its first week, because the bucket refuses it rather than because the credential lacks the verb. That protection survives somebody issuing a more powerful token later, which a credential scoped narrowly would not.

**Negative / tradeoffs**:
- The daily object is a full copy of every password hash, token, and clear text app configuration value on the platform. Thirty of them exist at any time. The encryption is the only thing standing between that and a bucket credential leak, and it is only as good as where you keep the identity.
- Losing the age identity means every backup you hold is permanently unreadable, and nothing in the platform can tell you that until you try. This is the single biggest new operational risk and it is entirely on the operator.
- Two new dependencies, minio-go and age, in a module that currently has two real ones.
- Three mechanisms across three places: Go code here, a Longhorn job in the GitOps repo, and a manual export you have to remember. Two of the three are not tested by anything in this repository, which is exactly the shape that rots.
- The recovery point is a day. A failure an hour before the run loses that hour.
- The read back proves the bytes arrived, and the header check proves the recipient is right. Neither proves the object decrypts, because nothing in the cluster can decrypt it. AC-25's rehearsal proves the mechanism once; it does not prove any particular later backup, and no design that keeps the private key out of the cluster can.
- The registry backup lands in the bucket unencrypted, because Longhorn's encryption is at the volume level, not the backup. Those blobs are user application images rather than platform secrets, but they are still user code sitting in plaintext off site.
- A run puts three copies of the database on the volume at once: the live one, the plaintext snapshot, and the ciphertext beside it. On top of that, `VACUUM INTO` holds a read transaction open for its duration, which pins the write ahead log and stops it being checkpointed, so the WAL grows during the run too. All of it is harmless at 10Gi against a database this size, and all of it is a real constraint the moment either number changes.

**Neutral**:
- One migration, purely additive: a new table and its indexes, nothing existing altered. A previous binary reads the schema unharmed.
- The binary grows a subcommand, which it did not have before. `cmd/deployer` stops being a single entry point.
- `backup_runs` accumulates one row a day forever, matching the existing choice not to prune releases.
- R2 is the first external service dependency in the deploy path other than mail. The control plane namespace policy is ingress only, so reaching it needs no policy change today. That also means nothing bounds the control plane's egress, which is a separate observation, not a change this spec makes.

## Follow-up

- [ ] `releases.config_snapshot` holds live third party credentials in clear text inside the database, and this feature multiplies the copies of them rather than reducing the exposure. Encrypting configuration values at rest so the platform stores ciphertext is its own decision and deserves its own spec. Consider `/architect configuration secrets at rest`.
- [ ] The control plane has no egress policy at all, which this spec now relies on to reach R2. Whether that should stay unbounded, or become an explicit allow for the bucket endpoint and the Kubernetes API, is worth deciding on its own rather than by omission.
- [ ] The Longhorn recurring job and the sealed secrets key export live outside this repository and nothing here tests or notices them. Decide whether the platform should check that a registry backup exists, or whether that stays a manual habit.
- [ ] The registry backup is unencrypted in the bucket. Longhorn can encrypt at the volume level with an encrypted StorageClass, which is a migration of the registry volume rather than a configuration flag. Worth weighing separately.
- [ ] Agent Skills and MCP servers for minio-go and age were offered and declined during design. Record the decline in `AGENTS.md` so nothing offers them again.
