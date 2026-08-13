# Verify: app lifecycle, list and delete · spec 0012 · updated 2026-08-13
_Steps derived from spec 0012 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

## MCP session, against the deployed platform

- [ ] Call `list_apps` with no arguments as an account holding several apps → the caller's live apps come back newest first, each with `name`, `slug`, `url`, `created_at` → AC-1, AC-2
- [ ] Check the `url` on any row against what `deploy_app` reported for the same app → the two are the same address, composed from the slug → AC-2
- [ ] List an account holding an app that has never had a healthy deploy → that row has no `serving` object at all, rather than release zero → AC-3
- [ ] Start a deploy, then call `list_apps` while it is still running → that app's `last_deployment` reports the live state (`queued` or `building`) with no `reason` → AC-4
- [ ] Break an app's build so a deploy fails on an app already serving a release, then list → the row carries both a `serving.release_number` and a `failed` `last_deployment` with its reason code → AC-5
- [ ] Compare `last_deployed_at` on that row against the last successful deploy's finish → it is the newest finish, and it is absent on an app nothing has finished for → AC-6
- [ ] Read the whole `list_apps` response for an app with configuration set, secret and plain → no configuration key and no value appears anywhere in it → AC-7
- [ ] Call `list_apps` on an account with no apps → `{"apps": []}`, a success rather than a refusal → AC-9
- [ ] Call `list_apps` as a second account, and as the admin account, for an app belonging to the first → neither sees it → AC-10
- [ ] Count `audit_log` rows for the account before and after a successful `list_apps` → unchanged; a refused call leaves one `denied` row with action `app_list` → AC-11
- [ ] Read `list_apps`'s tool description from the session → it says at most the newest 50 and that there is no way to page past them → AC-12
- [ ] Call `delete_app` with the app's `name` → the response carries `name`, `slug`, and `deleted` true, and returns without waiting → AC-13, AC-17
- [ ] Start a deploy and call `delete_app` for that app while it is in flight → refused with `deployment_in_flight`; the app still lists, and its namespace is still on the cluster → AC-15
- [ ] `delete_app` on a name that does not exist, on another account's app, and on an app already deleted → all three refused with `app_unknown`, word for word the same message → AC-20
- [ ] `delete_app` an app that was registered but never deployed, so it has no namespace → success, `deleted` true → AC-18
- [ ] After a delete, call `deployment_status` (by name and by the old deployment id), `get_logs`, `get_config`, `list_releases` and `rollback_app` for that app → each answers `app_unknown` or `deployment_unknown` → AC-32
- [ ] Read `delete_app`'s description → it says the delete cannot be undone, the hostname is never reused, it does not wait, a deploy in flight refuses it, and history and configuration are kept → AC-30
- [ ] Create an app under the deleted app's name again → it gets a new slug and therefore a new hostname; the old slug is never handed out → AC-22

## Cluster

- [ ] `kubectl get ns app-<slug>` right after a delete → gone or `Terminating` → AC-16
- [ ] `kubectl get deploy,svc,ing,secret,netpol,quota,limitrange,rolebinding -n app-<slug>` once it has terminated → nothing left; nothing was deleted object by object → AC-16
- [ ] Inspect the control plane log for the delete → any cluster error is at error level there, and the caller saw only the reason code → AC-19
- [ ] `kubectl create ns app-orphan-test` with labels `app.kubernetes.io/managed-by=deployer` and `deployer.internal/app-slug=orphan-test`, wait past `DEPLOYER_ORPHAN_GRACE_SECONDS`, then wait for a reaper pass → the namespace is gone and the log carries `orphan app namespace reaped` with the slug → AC-23, AC-25, AC-27
- [ ] Create the same namespace and check it within the grace → it is still there; a live app's own namespace is never touched by a pass → AC-26, AC-24
- [ ] Create a namespace named `app-something` carrying neither label → the reaper never touches it → AC-25
- [ ] Restart the control plane pod → one reaper pass runs at startup, before the ticker's first firing → AC-23

## Database

- [ ] After a delete, read the app's rows → `apps.deleted_at` is set, `apps.current_release_id` is unchanged, and its `deployments`, `deployment_events`, `releases` and `app_config` rows are all still there → AC-14, AC-21
- [ ] Read `audit_log` after a successful delete and after a `deployment_in_flight` refusal → one `allowed` and one `denied` row, both action `app_delete`, both carrying the app id; an `app_unknown` refusal carries no target → AC-29

## Commands

- [ ] `go test -race ./...` → green → AC-31
- [ ] Boot with `DEPLOYER_REAP_INTERVAL_SECONDS=0` or `DEPLOYER_ORPHAN_GRACE_SECONDS=soon` → the boot is refused, naming the variable → AC-26
- [ ] Boot with neither set → the reaper runs every ten minutes with a fifteen minute grace → AC-23, AC-26

## Acceptance-criteria coverage

- AC-1, AC-2 covered by the first two listing steps · AC-3, AC-4, AC-5, AC-6 by the four fact steps · AC-7 by the configuration read · AC-8 covered by the store test rather than by hand, the query is one statement · AC-9, AC-10, AC-11, AC-12 by the listing steps · AC-13, AC-17 by the delete step · AC-14, AC-21 by the database steps · AC-15, AC-18, AC-20, AC-22 by the delete refusals · AC-16, AC-19 by the cluster steps · AC-23 to AC-27 by the reaper steps and the boot checks · AC-28 by the `deployment_in_flight` refusal carrying the code · AC-29 by the audit step · AC-30 by the description read · AC-31 by the suite · AC-32 by the after delete step
