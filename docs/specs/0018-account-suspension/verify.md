# Verify: account suspension · spec 0018 · updated 2026-08-14
_Steps derived from spec 0018 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

Run these against the real cluster with a second account you can suspend, holding
at least two live apps and one API token. Keep an admin session open in another
browser, because suspending yourself is refused and getting locked out of the
admin page mid run is the one way to strand this.

## Commands

- [x] Suspend the account from the admin page, then `kubectl -n app-<slug> get deploy` for each of its apps → `READY 0/0`, `UP-TO-DATE 0` → AC-3
- [x] `kubectl -n app-<slug> get svc,ingress,secret,networkpolicy` after the suspension → every object still present, unchanged → AC-22
- [x] `curl -s -o /dev/null -w '%{http_code}' https://<slug>.deploy.toyintest.org` → the ingress answer with no pods behind it, and the hostname still resolves → AC-22
- [ ] `SELECT disabled_at FROM accounts WHERE id = ?` → stamped; `SELECT COUNT(*) FROM sessions WHERE account_id = ? AND revoked_at IS NULL` → 0 → AC-2
- [x] Call any MCP tool with that account's token → `account_suspended: <the static line>`, delivered as a tool result the client reports as a tool error, not as an HTTP 401 and not as a dropped connection → AC-9, AC-10
- [x] Watch the same call at the HTTP level (`curl -i` against the MCP endpoint with the suspended token) → the response is a normal 200 JSON-RPC body carrying the error result, not a 401 → AC-9
- [ ] After that refusal, check `apps`, `deployments`, and `uploads` → no new row of any kind → AC-9
- [x] `curl -X POST -H "Authorization: Bearer <suspended token>" <upload endpoint>` → 403 with `account_suspended` in the body → AC-11
- [ ] Repeat with a made up token, a revoked token, and an expired token → all three answer 401 unauthorized, indistinguishable from each other → AC-12
- [ ] `SELECT action, outcome, reason, target_type, target_id FROM audit_log ORDER BY id DESC LIMIT 5` after suspending → one `admin` row against the account plus one row per app stopped, all with reason `suspend` → AC-16
- [x] Restore from the admin page, then `kubectl -n app-<slug> get deploy` → `READY 1/1` on each app → AC-4
- [ ] `SELECT COUNT(*) FROM deployments WHERE app_id = ?` and `SELECT COUNT(*) FROM releases WHERE app_id = ?` before and after the restore → both unchanged, and `kubectl -n app-<slug> get deploy -o jsonpath='{..image}'` is the same digest → AC-4
- [x] `kubectl get jobs -n <build namespace>` during the restore → no build Job was created → AC-4
- [x] Suspend, then `kubectl -n app-<slug> scale deploy <workload> --replicas=1` by hand, wait one `DEPLOYER_RECONCILE_INTERVAL` → back to 0 → AC-7, AC-8
- [x] With that account suspended, confirm an unrelated active account's apps stay at 1 replica across several ticks → AC-8
- [ ] Delete an app namespace by hand, then suspend and restore → both succeed, and the log names the missing workload rather than erroring → AC-5
- [x] Suspend, wait for a sweep tick to start, and restore during it (repeat a few times if the window is tight) → the app ends at 1 replica and stays there → AC-24
- [ ] Start a deploy, let it reach the building phase, then suspend the account → the deployment ends `failed` with `failure_reason = 'account_suspended'`, and `kubectl get jobs -n <build namespace>` shows its Job gone → AC-14
- [ ] `SELECT COUNT(*) FROM releases WHERE app_id = ?` after that → unchanged, so nothing was promoted → AC-14
- [x] Leave the account suspended for longer than one reconcile interval and re check → still suspended, apps still at 0, nothing restored it → AC-21
- [x] `delete_app` on a suspended account's app, as the admin through the database or after a restore → the app deletes and the reaper removes its namespace on schedule → AC-23
- [x] `git log --stat` on this change → no file added under `internal/store/migrations/`; run the previous image against the same database and confirm it starts → AC-1

## UI / manual

- [x] Admin page → the control reads Suspend, the column reads Suspended, and the restore control reads Restore → AC-20
- [x] The suspend confirmation says the account's apps will stop serving → AC-20
- [x] Type the wrong address into the confirmation → nothing changes, the apps keep serving, and the message says the address did not match → AC-18
- [x] Break one app's namespace first (delete the Deployment, keep the namespace), then suspend → the account is suspended and the page names that app as not stopped → AC-6
- [x] Sign in as the suspended person → the same answer a wrong password gives, with no hint the account exists or is suspended → AC-13
- [ ] Suspend a second admin account → allowed; try to suspend your own row → refused, and the control is absent from your row → AC-17
- [x] Suspend through the JSON admin route instead of the page → the same apps stop and the same audit rows appear → AC-19
- [ ] Open the failed deployment from step 14 on the app page → it renders a plain sentence, not the raw code → AC-15
- [x] Sign in as an ordinary account and POST to both admin routes → refused `admin_required` → AC-17

## Value sourcing coverage

One step per row of the spec's Value sourcing table, exercising the edge that
breaks if the source is wrong.

- [ ] Give the account an app that has never deployed successfully (no `current_release_id`), then suspend and restore → it is skipped both ways with no error and no audit row, proving the live app predicate → AC-3
- [ ] Soft delete an app, then suspend → the deleted app is not scaled and not audited, proving `deleted_at IS NULL` is in the query → AC-3
- [x] Restore and read the Deployment's replica count → exactly 1, the same number a fresh deploy composes, proving the shared constant → AC-4
- [x] Suspend with two apps where one namespace is unreachable → the reachable one still stops, proving failures are collected rather than returned on the first → AC-6
- [x] Add a second suspended account, then watch one sweep tick → both accounts' apps are held at zero from the single joined read → AC-7
- [x] Present a valid token for an active account immediately after the `ResolveToken` change → it still works, proving only the disabled filter moved → AC-12

## Added after the build · 2026-08-14

Steps the implementation makes checkable that the design could not name yet.

### Commands

- [x] `go test -race ./internal/suspend/ ./internal/kube/ ./internal/reconcile/ ./internal/mcp/ ./internal/auth/` → green; these hold the sweep re-read, the missing Deployment case, the mid build suspension, and the wire level refusal → AC-5, AC-9, AC-14, AC-24
- [ ] `git log --oneline --name-only | grep internal/store/migrations` → nothing from this change, and `SELECT MAX(version_id) FROM goose_db_version` on the live database still reads 3 → AC-1
- [x] `kubectl -n app-<slug> get deploy app -o jsonpath='{.spec.replicas}'` right after a suspend → `0`; after a restore → `1` → AC-3, AC-4
- [x] `kubectl -n app-<slug> get ingress,svc,secret,networkpolicy` after a suspend → all still present and unchanged → AC-22

### On the cluster

- [x] Suspend, then `kubectl -n app-<slug> scale deploy app --replicas=1` by hand → within one `DEPLOYER_RECONCILE_INTERVAL_SECONDS` tick it is back at 0, and the platform log carries no error → AC-7, AC-8
- [ ] While a real build is running (watch for the build Job), suspend the owning account → the deployment ends `failed` with `account_suspended`, its build Job is gone, and no release was minted → AC-14
- [x] Restore an account while a sweep tick is in flight (restore repeatedly during one interval) → the apps come up and stay up → AC-24
- [x] With a suspended account's token: `curl -sD- -X POST $DEPLOYER_PUBLIC_URL/v1/uploads -H "Authorization: Bearer $TOKEN" --data-binary @app.tar.gz` → `403` with `account_suspended`, not `401` → AC-11
- [x] Same token against `/mcp` from a real agent → the tool answers `account_suspended` as a tool result and the connection stays open, rather than the client reporting a transport failure → AC-9, AC-10

### Notes from the build

- The two store reads shipped as `ListDeployedAppsByAccount` and
  `ListDeployedAppsOfSuspendedAccounts`, not the `ListLiveApps*` names the build
  plan used. `LiveAppSlugs` already exists on a different predicate (soft delete
  only), so reusing `Live` for a predicate that also requires a release would
  have made two queries with the same word mean two things.
- AC-17's self refusal was a rendering choice before this change, not a refusal:
  the page rendered no control on your own row, but a direct form post went
  through. Both surfaces now refuse it in the handler. Worth exercising by hand.

## What the run on 2026-08-14 found

Driven against the real cluster on branch image `sha256:6ee0c07f`, target accounts
`rbverify` (two live apps) and the bootstrap account (seventeen apps, nine of them
with dead namespaces).

All three code defects below were fixed by `/debug` the same day, each with a
test that fails without the fix. They still owe a re run against the real
cluster, because the cases that produced them are exactly the ones the fake
clientset cannot make.

- **AC-5 failed.** `ScaleWorkload` in `internal/kube/kube.go` accepted only
  `IsNotFound` as a Deployment that is not there. The platform's RBAC is granted
  per app namespace, so a namespace that is gone answers `Forbidden`, not
  `NotFound`. Nine apps were reported as not stopped, and the sweep logged an
  error for each of them every tick for as long as the account stayed suspended,
  thirty seven errors a minute. The fake clientset answers `NotFound` for a
  missing namespace, which is why every unit test passed. Fixed: forbidden is
  success once `namespaceGone` confirms the namespace really is absent, and stays
  an error inside a namespace that is still there, because an app the platform
  failed to stop must never read as stopped.
- **AC-14 failed while a build is running.** `awaitBuild` and `awaitReady` poll
  without calling `blocked`, so the check only runs at the phase boundaries
  either side of the wait. A deployment held in `building` stayed there with its
  Job alive for more than three minutes after its account was suspended, and only
  failed `account_suspended` once the build finished. `wait` consults `ctx.Err()`
  on every poll, which is the place the suspension read belongs by AC-14's own
  wording. Fixed: `wait` takes the app id and runs the same `blocked` check per
  tick, so both loops and any later one inherit it.
- **The restore message was wrong.** A partial restore said "The platform keeps
  retrying on its own", but nothing retries a restore: the sweep only scales down
  by AC-8. True on suspend, false on restore. Fixed: the restore direction now
  says to restore again to retry.
- **An account with no email has no confirmation.** The typed email gate is an
  empty string for the bootstrap account, so its Suspend control is enabled by
  default and one click stops seventeen apps. AC-18 holds for every account that
  has an address.
- The JSON admin routes authenticate with an admin **session**, not the admin
  bearer token the spec's API surface table claims.

Left unproved: everything needing the SQLite file (AC-16 audit rows, AC-2's
`disabled_at` stamp, AC-4's deployment and release counts, AC-9's no rows
written, AC-1's goose version), plus the expired token shape of AC-12 and the
failed deployment's rendered sentence, AC-15.
