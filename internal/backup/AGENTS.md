# internal/backup

Takes the platform's own SQLite database out of the cluster, once a day and on
demand, encrypted to a key the cluster does not hold, and reads one back again.
Governing spec: [docs/specs/0020-platform-backup-restore/index.md](../../docs/specs/0020-platform-backup-restore/index.md).

## Layout

- [run.go](run.go): the `Service`, the `Runs` interface it declares over the
  store, and `Run`, one backup end to end.
- [snapshot.go](snapshot.go): the SQLite snapshot, its integrity check, the age
  encryption, and `EmptyTempDir`. `DefaultTempDir` lives here.
- [objectstore.go](objectstore.go): the `ObjectStore` interface, the minio backed
  `S3Store` behind it, and the read back that confirms what landed.
- [schedule.go](schedule.go): the startup sweep and the ticker.
- [alert.go](alert.go): the failure and recovery mail, over the existing mailer.
- [restore.go](restore.go): `Restore`, the half that runs outside the cluster,
  behind `deployer restore` in [cmd/deployer/restore.go](../../cmd/deployer/restore.go).

## Conventions

- A nil `*Service` is the unconfigured platform, not an error. `New` returns nil
  when there is no object store, every method on nil is safe, and the control
  plane boots, warns once, and works in every other respect. Keep new methods nil
  safe, and keep the nil crossing an interface boundary as nil (`webBackups` in
  [cmd/deployer/main.go](../../cmd/deployer/main.go) exists only for that).
- The store is reached through the `Runs` interface declared here, never by
  importing the store package, which is the layering rule in the root
  [AGENTS.md](../../AGENTS.md).
- One run at a time is decided by the partial unique index in the migration, not
  by a read followed by a write. `ErrInFlight` is that refusal surfaced. A read
  that finds nothing is `ErrNoRun`, a state rather than a fault.
- Every temporary file goes in one fixed directory, mode 0700, and both files are
  removed whether the run worked or not. The directory is emptied again at
  startup, because a pod killed mid run leaves its files behind. `Sweep` is a
  package function rather than a `Service` method for that reason: it takes the
  record and the directory, and it runs whether or not backups are configured,
  because a platform someone switched them off on still has the plaintext and
  the row a killed run left behind. The path is a
  constant rather than configuration, so it does not follow `DEPLOYER_DB_PATH`:
  off cluster, where `/data` is not writable, the startup sweep logs an error and
  everything else still works.
- The snapshot's mode is set after the vacuum, never assumed. `VACUUM INTO` makes
  the file itself, at 0666 minus the umask, so the plaintext copy of every
  password hash, session and app secret lands world readable unless `snapshot`
  tightens it to 0600. SQLite's rollback journal beside it cannot be reached that
  way and is gone before `snapshot` returns, so the directory's own 0700 is what
  bounds that window. A process umask would cover both in one line and is
  deliberately not used: it changes every file the platform writes, and the build
  path hands a tree between three different uids on purpose.
- The plaintext never leaves the volume. The snapshot is checked, encrypted, and
  only the `.age` file is uploaded, then read back and compared before the row is
  closed. An upload that succeeds and reads back short is a failed run and the
  object is left alone.
- Nothing here can decrypt anything. The recipient is a public key, the private
  identity is off cluster by design, and no code path in the cluster takes one.
  That is why `confirmRecipient` compares the stanzas the configured recipient
  produced against the finished file: an X25519 header names no recipient, so
  this is the only check available that the object is readable by the key you
  hold.
- A failure sends one mail, the first success after a failure sends one, and a
  success after a success sends nothing, so silence means healthy. The mail
  carries the closed reason code from
  [internal/domain/backupreason.go](../../internal/domain/backupreason.go) and
  nothing else: no bucket, no object key, no configuration value.
- The ticker measures from the end of each run, and at startup it runs one
  immediately only when the newest success is older than the interval, so a pod
  that restarts often still gets backed up without backing up on every restart. A
  tick arriving while a run is in flight skips with a log line rather than
  attempting an insert it knows the index will refuse.

## Tests

[run_test.go](run_test.go) drives the whole thread against a real SQLite file and
an in memory object store, with a real age key pair generated in the test, so the
round trip is genuinely encrypted and genuinely decrypted. What the fake cannot
reach is what `/check verify` covers instead: file ownership, cluster DNS, the
real bucket, and anything needing the private identity.

[snapshot_internal_test.go](snapshot_internal_test.go) is in package `backup`
rather than `backup_test`, because the temp files are deleted on every exit path
and nothing outside the package can see their mode. It exists because `/check
verify` found the snapshot at 0644 on the cluster, which is the root
[AGENTS.md](../../AGENTS.md) rule about pinning whatever the fakes let through.

_Drafted by /sync from the introducing change, worth a quick human pass._
