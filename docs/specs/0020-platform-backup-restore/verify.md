# Verify: platform backup and restore · spec 0020 · updated 2026-08-15

_Steps derived from spec 0020 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

The steps that matter most here cannot be done by a unit test: the fake object
store resolves no names, the test process owns every file it writes, and nothing
in the suite holds an age identity. Everything below runs against the real
cluster and the real bucket unless it says otherwise.

## Commands

- [x] `kubectl -n deployer-system logs deploy/deployer | grep backup` right after a rollout → the schedule started, naming the interval; no line names a bucket, a key, or a credential → AC-12, AC-14
- [x] `kubectl -n deployer-system exec deploy/deployer -- ls -la /data/backup-tmp/` while no run is going → the directory exists and is empty → AC-2, AC-2a
- [ ] `kubectl -n deployer-system exec deploy/deployer -- ls -la /data/backup-tmp/` mid run, immediately after pressing the button → two files, both owned by the pod's uid, mode 600 → AC-2, AC-4b
- [ ] Delete the pod mid run (`kubectl -n deployer-system delete pod <pod>`), let it come back, then list `/data/backup-tmp/` → empty, and the run that was going is `failed: stranded` on the page → AC-2a, AC-9
- [x] `rclone ls`/`aws s3 ls` the bucket under `db/` → the object is there, its name is `db/<YYYYMMDDTHHMMSSZ>-<run id>.age`, and its size matches the page → AC-5, AC-6
- [x] `head -c 30 <the downloaded object>` → it begins `age-encryption.org/v1`, and nothing in it is readable SQLite → AC-4
- [x] `go run ./cmd/deployer restore -key <key> -identity <file> -out ./restored.db` on your own machine → it writes the file and reports success → AC-23
- [x] Run that same command again with the same `-out` → refused because the destination exists, and the existing file is untouched → AC-24
- [x] Run it with `-identity` pointing at a file that is not an age identity → refused, and the error echoes no part of the file → AC-23
- [x] `sqlite3 ./restored.db "SELECT COUNT(*) FROM accounts"` → matches what the platform held when the backup was taken → AC-3, AC-23
- [ ] `ps aux | grep restore` while a restore runs → no age identity appears in the process listing → AC-23
- [x] Set only five of the six `DEPLOYER_BACKUP_*` values on a scratch deploy → the pod refuses to start, naming the missing one → AC-16
- [x] Remove all six on a scratch deploy → the pod starts, logs one warning naming all six, and every other page works → AC-15

## UI / manual

- [ ] Sign in as an admin, visit `/admin/backups` → the runs list with started, finished, outcome, size, object key, and a reason where there is one → AC-17
- [ ] Visit `/admin/backups` signed out, and signed in as a non admin → exactly what the existing admin gate gives on `/admin/accounts`, and no hint the page exists → AC-17
- [ ] With backups unconfigured, visit `/admin/backups` → a stated "not configured" panel, not an empty table, and no run button → AC-18
- [x] Press **Back up now** → the run appears as `succeeded`, `by hand`, with a size and an object key → AC-19, AC-21
- [x] Press **Back up now** twice quickly → the second is refused, the page says a backup is already running, and the bucket gains one object rather than two → AC-8, AC-20
- [x] `SELECT * FROM audit_log WHERE target_type='backup'` after both presses → two rows, the allowed one and the refused one → AC-22
- [x] `SELECT trigger, triggered_by FROM backup_runs` → the pressed run is `manual` with your account id, the scheduled one is `schedule` with null → AC-21
- [x] Break the credential (a wrong secret key on a scratch deploy), press the button → the run ends `failed: upload_failed`, one mail arrives naming only that code, and the mail carries no bucket name, object key, or configuration value → AC-13, AC-14
- [x] Fix the credential, press again → one recovery mail arrives. Press a third time → no mail at all → AC-13
- [ ] Read the whole `/admin/backups` page source as a non admin and as a signed out visitor → no object key, bucket name, endpoint, or credential appears anywhere → AC-14
- [x] Check the bucket's retention rule in the Cloudflare dashboard → 7 days, and try deleting a fresh object with the platform's own token → refused → AC-6a
- [x] Check the lifecycle rule → 30 day expiry scoped to the `db/` prefix, so registry backups are not swept → AC-6a
- [x] `kubectl -n longhorn-system get backuptarget default -o jsonpath='{.status.available}'` → `true`, so the target and the credential are good → AC-27
- [x] `kubectl -n longhorn-system get recurringjob registry-backup -o yaml` → task `backup`, retain 30, group `registry` and **not** `default` → AC-27
- [x] List every Longhorn volume with its recurring job labels → the registry volume is in the `registry` group, and the volume behind `deployer-data` is in no group that has a job → AC-27
- [ ] `kubectl -n longhorn-system get backups` the morning after → a completed backup of the registry volume, and none of the SQLite volume → AC-27
- [x] Confirm the sealed secrets controller key export is in your password manager, and re-run the export command → it produces a key matching what you hold → AC-28
- [ ] Confirm no Secret, ConfigMap, environment variable, or file in the cluster holds an age identity: `kubectl -n deployer-system get secret,cm -o yaml | grep -i "AGE-SECRET-KEY"` → nothing → AC-4

## The rehearsal

- [x] **AC-25, the one that makes this feature real.** Take a real backup object, restore it with `deployer restore`, follow the runbook in `deploy/README.md` end to end into a scratch instance (scale down, place the file plus removing the stale `-wal` and `-shm`, scale up), then sign in to that instance with an account that existed when the backup was taken. Signing in is the check; everything before it only proves the file arrived → AC-25, AC-26
- [x] While there, confirm `/data/uploads` did not come back and nothing depends on it having → AC-29

## Acceptance-criteria coverage

- AC-1 covered by the unit thread (`internal/backup/run_test.go`) and by the platform still serving during a manual run
- AC-2, AC-2a covered by the temp directory listings and the mid run pod delete
- AC-3 covered by the restored account count and by `TestRunEmptyDatabaseUploadsNothing`
- AC-4, AC-4a, AC-4b covered by the object header check and the absent identity sweep
- AC-4c covered by unit test only, and note the deviation: an X25519 header names no recipient, so the run compares the stanzas the configured recipient produced against the finished file
- AC-5, AC-6 covered by the bucket listing and the size comparison
- AC-6a covered by the retention and lifecycle rule checks
- AC-7, AC-8, AC-8a, AC-10, AC-11 covered by `internal/store/backups_test.go` and the double press
- AC-9 covered by the mid run pod delete
- AC-12, AC-12a covered by the startup log and the double press
- AC-13, AC-14 covered by the broken credential, the recovery, and the page source read
- AC-15, AC-16 covered by the five value and no value scratch deploys
- AC-17 through AC-22 covered by the page and audit steps
- AC-23, AC-24 covered by the restore commands
- AC-25, AC-26 covered by the rehearsal
- AC-27, AC-28, AC-29 covered by the three checks outside this repository
