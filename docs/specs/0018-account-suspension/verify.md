# Verify: account suspension · spec 0018 · updated 2026-08-14
_Steps derived from spec 0018 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

Run these against the real cluster with a second account you can suspend, holding
at least two live apps and one API token. Keep an admin session open in another
browser, because suspending yourself is refused and getting locked out of the
admin page mid run is the one way to strand this.

## Commands

- [ ] Suspend the account from the admin page, then `kubectl -n app-<slug> get deploy` for each of its apps → `READY 0/0`, `UP-TO-DATE 0` → AC-3
- [ ] `kubectl -n app-<slug> get svc,ingress,secret,networkpolicy` after the suspension → every object still present, unchanged → AC-22
- [ ] `curl -s -o /dev/null -w '%{http_code}' https://<slug>.deploy.toyintest.org` → the ingress answer with no pods behind it, and the hostname still resolves → AC-22
- [ ] `SELECT disabled_at FROM accounts WHERE id = ?` → stamped; `SELECT COUNT(*) FROM sessions WHERE account_id = ? AND revoked_at IS NULL` → 0 → AC-2
- [ ] Call any MCP tool with that account's token → `account_suspended: <the static line>`, delivered as a tool result the client reports as a tool error, not as an HTTP 401 and not as a dropped connection → AC-9, AC-10
- [ ] Watch the same call at the HTTP level (`curl -i` against the MCP endpoint with the suspended token) → the response is a normal 200 JSON-RPC body carrying the error result, not a 401 → AC-9
- [ ] After that refusal, check `apps`, `deployments`, and `uploads` → no new row of any kind → AC-9
- [ ] `curl -X POST -H "Authorization: Bearer <suspended token>" <upload endpoint>` → 403 with `account_suspended` in the body → AC-11
- [ ] Repeat with a made up token, a revoked token, and an expired token → all three answer 401 unauthorized, indistinguishable from each other → AC-12
- [ ] `SELECT action, outcome, reason, target_type, target_id FROM audit_log ORDER BY id DESC LIMIT 5` after suspending → one `admin` row against the account plus one row per app stopped, all with reason `suspend` → AC-16
- [ ] Restore from the admin page, then `kubectl -n app-<slug> get deploy` → `READY 1/1` on each app → AC-4
- [ ] `SELECT COUNT(*) FROM deployments WHERE app_id = ?` and `SELECT COUNT(*) FROM releases WHERE app_id = ?` before and after the restore → both unchanged, and `kubectl -n app-<slug> get deploy -o jsonpath='{..image}'` is the same digest → AC-4
- [ ] `kubectl get jobs -n <build namespace>` during the restore → no build Job was created → AC-4
- [ ] Suspend, then `kubectl -n app-<slug> scale deploy <workload> --replicas=1` by hand, wait one `DEPLOYER_RECONCILE_INTERVAL` → back to 0 → AC-7, AC-8
- [ ] With that account suspended, confirm an unrelated active account's apps stay at 1 replica across several ticks → AC-8
- [ ] Delete an app namespace by hand, then suspend and restore → both succeed, and the log names the missing workload rather than erroring → AC-5
- [ ] Suspend, wait for a sweep tick to start, and restore during it (repeat a few times if the window is tight) → the app ends at 1 replica and stays there → AC-24
- [ ] Start a deploy, let it reach the building phase, then suspend the account → the deployment ends `failed` with `failure_reason = 'account_suspended'`, and `kubectl get jobs -n <build namespace>` shows its Job gone → AC-14
- [ ] `SELECT COUNT(*) FROM releases WHERE app_id = ?` after that → unchanged, so nothing was promoted → AC-14
- [ ] Leave the account suspended for longer than one reconcile interval and re check → still suspended, apps still at 0, nothing restored it → AC-21
- [ ] `delete_app` on a suspended account's app, as the admin through the database or after a restore → the app deletes and the reaper removes its namespace on schedule → AC-23
- [ ] `git log --stat` on this change → no file added under `internal/store/migrations/`; run the previous image against the same database and confirm it starts → AC-1

## UI / manual

- [ ] Admin page → the control reads Suspend, the column reads Suspended, and the restore control reads Restore → AC-20
- [ ] The suspend confirmation says the account's apps will stop serving → AC-20
- [ ] Type the wrong address into the confirmation → nothing changes, the apps keep serving, and the message says the address did not match → AC-18
- [ ] Break one app's namespace first (delete the Deployment, keep the namespace), then suspend → the account is suspended and the page names that app as not stopped → AC-6
- [ ] Sign in as the suspended person → the same answer a wrong password gives, with no hint the account exists or is suspended → AC-13
- [ ] Suspend a second admin account → allowed; try to suspend your own row → refused, and the control is absent from your row → AC-17
- [ ] Suspend through the JSON admin route instead of the page → the same apps stop and the same audit rows appear → AC-19
- [ ] Open the failed deployment from step 14 on the app page → it renders a plain sentence, not the raw code → AC-15
- [ ] Sign in as an ordinary account and POST to both admin routes → refused `admin_required` → AC-17

## Value sourcing coverage

One step per row of the spec's Value sourcing table, exercising the edge that
breaks if the source is wrong.

- [ ] Give the account an app that has never deployed successfully (no `current_release_id`), then suspend and restore → it is skipped both ways with no error and no audit row, proving the live app predicate → AC-3
- [ ] Soft delete an app, then suspend → the deleted app is not scaled and not audited, proving `deleted_at IS NULL` is in the query → AC-3
- [ ] Restore and read the Deployment's replica count → exactly 1, the same number a fresh deploy composes, proving the shared constant → AC-4
- [ ] Suspend with two apps where one namespace is unreachable → the reachable one still stops, proving failures are collected rather than returned on the first → AC-6
- [ ] Add a second suspended account, then watch one sweep tick → both accounts' apps are held at zero from the single joined read → AC-7
- [ ] Present a valid token for an active account immediately after the `ResolveToken` change → it still works, proving only the disabled filter moved → AC-12
