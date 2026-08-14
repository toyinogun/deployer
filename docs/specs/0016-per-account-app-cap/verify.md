# Verify: per account app cap · spec 0016 · updated 2026-08-14
_Steps derived from spec 0016 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

Run these against the real cluster with an account you can fill. Set
`DEPLOYER_MAX_APPS_PER_ACCOUNT` to a small number first (2 or 3) so reaching the
ceiling costs two deploys rather than ten, then put it back.

## Commands

- [ ] `kubectl -n deployer-system get configmap deployer-config -o yaml | grep MAX_APPS` → shows `DEPLOYER_MAX_APPS_PER_ACCOUNT` → AC-7
- [ ] Set the variable to `0` and restart the pod → the pod fails to start and the log names `DEPLOYER_MAX_APPS_PER_ACCOUNT`; repeat with `-1` and `banana` → AC-7
- [ ] Unset the variable and restart → the pod starts and the apps page reads `N of 10 apps` → AC-7
- [ ] With the cap at 2, deploy two apps, then `deploy_app` a third new name → the refusal reads exactly `app_limit_reached: this account is at its limit for apps, so delete one you no longer need (2 of 2 used)` → AC-1, AC-3
- [ ] Immediately after that refusal, retry the same call with the **same** `upload_id` after deleting one app → it is accepted, so the refused call never spent the upload → AC-2, AC-5
- [ ] Check the database after the refusal: `SELECT COUNT(*) FROM apps WHERE account_id = ?` is unchanged, and no new `deployments` row exists → AC-1
- [ ] `SELECT action, outcome, reason, target_id FROM audit_log ORDER BY id DESC LIMIT 1` → `deploy` / `denied` / `app_limit_reached` / null target → AC-9
- [ ] At the cap, `deploy_app` a name the account already holds → accepted, returns a `deployment_id` → AC-4
- [ ] At the cap, `set_config` and `rollback` on an app the account already holds → both accepted → AC-4
- [ ] Two `deploy_app` calls fired at once on two new names, with the account one slot below the cap → exactly one app row is created and the other call is refused `app_limit_reached` → AC-6
- [ ] Lower the cap below the count and restart → every existing app still serves, `kubectl get ns` shows no namespace removed, and each existing app still redeploys → AC-14
- [ ] From a second account's token, confirm its own count is unaffected by the first account's apps, and that two tokens of one account share one allowance → AC-15
- [ ] `git log --stat` on this change → no file added under `internal/store/migrations/`; run the previous image against the same database and confirm it starts → AC-17

## UI / manual

- [ ] Sign in → visit `/apps` → the header reads `<n> of <cap> apps`, matching the count the refusal reported → AC-10
- [ ] With the account below the cap → no at cap notice is shown → AC-11
- [ ] With the account at or over the cap → the notice reads `You are at your limit of <cap> apps. Deploying a new app will be refused until you delete one.` → AC-11
- [ ] Sign in as an admin → visit `/admin/accounts` → each account row shows its live app count, and an account with no apps reads `0 apps` → AC-12
- [ ] Sign in as an ordinary account → visit `/admin/accounts` → 403 with an `admin_required` audit row, so per account counts stay admin only → AC-12
- [ ] Present an API bearer token to `/admin/accounts` → refused, because the page is session only → AC-12
- [ ] Ask an MCP client to list tools → `deploy_app`'s description states the limit, the refusal, and that deleting frees a slot, and names no number → AC-13
- [ ] Delete an app on the apps page, then reload → the usage line drops by one and the notice disappears → AC-5, AC-10, AC-11

## Value sourcing coverage

One step per row of the spec's Value sourcing table, exercising the edge that
breaks if the source is wrong.

- [ ] Deploy a name the account already holds while at the cap → proves the `ByName` lookup, not the count, decides which branch runs → AC-4
- [ ] Soft delete an app and immediately read `/apps` → the count drops, proving it comes from `deleted_at IS NULL` rather than a stored counter → AC-5, AC-10
- [ ] Change the configured cap and reload `/apps` without redeploying anything → the displayed ceiling changes, proving it reads `MaxAppsPerAccount` and not a cached number → AC-10
- [ ] Trigger the race path (two simultaneous creates) → the refusal still reports `<cap> of <cap> used`, which is exactly true at that point → AC-3, AC-6
- [ ] Create an account, give it no apps, open `/admin/accounts` → it reads `0 apps` rather than being absent or blank, proving the grouped read's missing key is treated as zero → AC-12

## Acceptance-criteria coverage

- AC-1 covered by the refusal and the database check · AC-2 upload retry · AC-3 exact refusal line · AC-4 redeploy at the cap · AC-5 delete frees a slot · AC-6 concurrent deploys · AC-7 the three configuration steps · AC-8 pinned in `internal/domain/reason_test.go` · AC-9 audit row read · AC-10 apps page usage line · AC-11 at cap notice · AC-12 admin page and its two refusals · AC-13 tool listing · AC-14 lowered cap · AC-15 second account · AC-16 the refusal arrives through a real MCP client session in every step above · AC-17 no migration and the previous binary
