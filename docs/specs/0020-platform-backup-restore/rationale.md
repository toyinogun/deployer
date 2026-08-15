# 0020. Platform backup and restore: the decision record

The build spec is in [index.md](index.md).

## Context

> ⚠️ Premise note: the scope row frames this as backing up metadata, and it is not. `deployer.db` holds every account's password hash, every live session, every API token, and, in `releases.config_snapshot`, a clear text copy of every configuration value every app has ever been given. Since slice 7 those are real third party credentials. Backing that file up does not reduce that exposure, it multiplies it: on the day this ships there are thirty copies of every secret anyone ever set, sitting on somebody else's disk. That is why the encryption decision here is load bearing rather than a checkbox, and why the key custody question is the one that actually decides whether this feature is safe. The deeper fix, encrypting `config_snapshot` at rest inside the database so the platform holds ciphertext rather than plaintext, is a separate decision this spec does not make. It is enrolled in Follow-up.

The control plane is one pod with one SQLite file on one Longhorn volume. Nothing copies that file anywhere. A volume that does not come back takes every account, every app record, every release, and every secret with it, and there is no path back other than everybody registering again and every app being rebuilt from source nobody kept.

Three forces shape what can be done about it.

The database is in WAL mode and has exactly one writer, the running process. Copying the file off the volume while that process is live is not a backup, it is a coin flip on whether the copy lands mid transaction. Anything that reads the file from outside, a Longhorn volume snapshot included, inherits that problem. The one thing that does not is the process itself, which can produce a consistent snapshot in one statement.

The volume is ReadWriteOnce and the Deployment is `Recreate`, deliberately, because two writers on one SQLite file is corruption and spec 0001 makes that an invariant. So the usual answer, a CronJob mounting the volume beside the running pod, is not available here. Whatever takes the backup either is the running process or is not on that volume at all.

The restore has to work when the cluster does not. That rules out any design where the thing needed to read a backup lives only inside the thing being backed up. It is the constraint that decides key custody, and it is the one most easily got wrong, because putting the private key in a SealedSecret looks careful right up until the day the sealed secrets controller is also gone.

There is a second and a third store at risk, and they are not the same problem. The registry volume holds every image ever pushed, which are immutable content addressed blobs with no consistency requirement and a lot of size. The sealed secrets controller key lives in `kube-system`, is nearly static, and without it the GitOps repo cannot boot a rebuilt cluster at all, because nothing can decrypt a single SealedSecret in it.

## Options considered

### Option 1: Longhorn recurring volume backups for everything

Longhorn 1.11 is already running and already knows how to back a volume up to an S3 target on a schedule. Point it at `deployer-data` and `deployer-registry-data`, set a retention count, done. No Go code, no new dependency, one configuration change in the GitOps repo.

**Pros**:
- By far the least work, and none of it in this repo.
- Covers both volumes with one mechanism, one credential, one place to look.
- Restores a whole volume, not just a file, so the uploads directory comes back too.

**Cons**:
- It snapshots the live WAL file. A restored database can land mid transaction, and you find out at restore time. Quiescing the pod first turns a backup into a scheduled outage.
- The platform cannot tell you whether a backup ran or worked. Nothing in the product knows, so nothing can alert.
- Longhorn's own encryption is at the volume level, not the backup, so the objects in the bucket are plaintext unless the whole volume is switched to an encrypted StorageClass.

### Option 2: Litestream replicating the WAL continuously

Litestream streams the write ahead log to object storage as it is written, giving a recovery point measured in seconds and a restore that is one command. It runs as a sidecar next to the control plane.

**Pros**:
- Near zero data loss, far better than any scheduled snapshot.
- Purpose built for exactly this shape: one SQLite file, one writer, object storage.
- Restore is a solved, well travelled path rather than something written here.

**Cons**:
- Another process and another pinned image in a deployment whose whole design is one binary in one pod.
- It aims at a recovery point far tighter than a homelab platform with a handful of accounts needs, and the cost of that precision is paid every day in operational surface.
- Encryption is not its concern, so age or an encrypted bucket has to be bolted on anyway.
- The platform still cannot report on its own backups.

### Option 3: The control plane backs itself up, Longhorn covers the registry

The running process takes its own snapshot with `VACUUM INTO`, checks it, encrypts it to a public key it cannot reverse, uploads it, reads it back to confirm, and records the run in its own database. The registry volume, which has no consistency problem, is left to a Longhorn recurring job. The sealed secrets key, which is nearly static and lives somewhere the platform's RBAC does not reach, is exported by hand.

**Pros**:
- The snapshot is consistent by construction, because the only writer is the one taking it. No quiescing, no downtime, no coin flip.
- The platform knows whether its own backups work, so it can record them, show them, and mail you when they stop.
- Each of the three stores gets the mechanism that fits it rather than one mechanism stretched over three.
- Encryption happens before the bytes leave the pod, with a key the pod does not hold, so the bucket and its provider are never trusted.

**Cons**:
- Real Go code in the control plane, with a new dependency for object storage and another for encryption, in a module that currently has two.
- Three mechanisms means three things to keep working, and two of them live outside this repository where nothing tests them.
- The recovery point is a day. A crash an hour before the run loses that hour.

### Option 4: A documented manual procedure

Write down the commands and run them when you remember.

**Pros**:
- No code, no dependencies, no new failure modes.
- Trivially correct, because a human is checking each step.

**Cons**:
- It is not a backup, it is an intention. The scope row asks for a schedule specifically because this is what the alternative decays into.

## Rationale

Option 3, because the consistency constraint decides it before anything else does. The database is in WAL mode with one writer, so every option that reads the file from outside the process is trading correctness for convenience, and the thing being traded away is the only property a backup has. `VACUUM INTO` costs one statement and removes the entire class of problem. Once the process is taking the snapshot anyway, it already has a database to record the run in, a mail path to alert through, and an admin surface to show it on, so the reporting that Options 1 and 2 structurally cannot provide comes almost for free.

Litestream is the better engineering answer to a problem this platform does not have. A recovery point of seconds instead of a day is worth a sidecar when the data is transactional and continuous. Here the data changes when somebody registers or sets a configuration value, which is a handful of events a week, and the cost of the precision is a second process in a deployment whose stated invariant is one binary in one pod.

The registry goes to Longhorn precisely because Option 1's weakness does not apply to it. Registry blobs are immutable and content addressed, so a live snapshot is safe, and the argument against Longhorn evaporates the moment consistency stops being a question. Using Longhorn there and not for the database is not inconsistency, it is matching the mechanism to the property of the data.

Key custody is where this could still fail while looking finished. age encrypts to a public recipient, so the pod needs only the public half and cannot decrypt anything, including its own backups. That is why the read back check compares ciphertext rather than opening the database, and why the integrity check runs on the plaintext snapshot before encryption instead. Both properties are checked, neither needs the private key in the cluster, and a compromise of the control plane pod yields no historical backup. The identity lives with you, off cluster, next to the exported sealed secrets key, because the failure this whole feature exists for is the one where the cluster is gone.

The credential the pod holds can read and write but not delete. Retention is a bucket lifecycle rule, so nothing inside the cluster has the permission to destroy backup history. That is deliberate: the attack that ends a platform is not the one that reads the backups, it is the one that deletes them.
