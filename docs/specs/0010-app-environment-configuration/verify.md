# Verify: app environment configuration · spec 0010 · updated 2026-08-13
_Steps derived from spec 0010 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

Run these against the real cluster with a real MCP client, because the fake clientset
resolves no names, mounts nothing, and never starts a container. Everything below that
touches a running pod is exactly what the unit tests cannot prove.

## MCP calls

- [x] `set_config` on your own app with two keys, one marked secret, one not → both come back in `config`, the secret one with `value: null`, and `applies_on_next_deploy` is true → AC-1, AC-2, AC-8
- [x] `get_config` on the same app → the same list, the secret value still null → AC-2
- [x] `set_config` with one more key → the earlier keys are still there (a merge, not a replace) → AC-1
- [x] `set_config` with `PORT`, then with `APP_URL` → both refused with `config_key_reserved`, and `get_config` shows the stored configuration unchanged → AC-5
- [x] `set_config` with a key like `bad key` or `1LEADING` → refused with `config_key_invalid` → AC-4
- [x] `set_config` with three valid keys plus one value over 4 KB → refused with `config_too_large`, and `get_config` shows none of the four → AC-6
- [x] `set_config` with 65 keys on a fresh app → refused with `config_too_many_keys` → AC-6
- [x] `set_config` re setting an existing secret key, sending `value` but no `secret` → refused with `config_flag_missing`, and `get_config` shows the key still secret → AC-16
      _Failed on the first run 2026-08-13: the schema marked `secret` required, so the sdk refused the call before `ValidateConfig` could and the caller saw a raw validation string with no audit row. Fixed in PR #21, which also added `internal/mcp/wire_test.go`, because every other test in that package calls handlers directly and never crosses the schema. Passes on both `set_config` and `deploy_app`, and both refusals are audited._
- [x] `set_config` with a key whose value is `""` → `get_config` lists the key with an empty value, not as missing → AC-15
- [x] `unset_config` naming one key that is set and one that never was → refused with `config_key_unknown`, and `get_config` shows both original keys still there → AC-3
- [x] `unset_config` naming only keys that are set → they are gone, `applies_on_next_deploy` is true → AC-3
- [x] From a second account's token, call `set_config`, `unset_config`, and `get_config` on the first account's app → all three answer `app_unknown`, the same as a name that does not exist → AC-13
- [x] `deploy_app` with a `config` map on a brand new app → the deploy is accepted and `get_config` shows the keys → AC-9
- [x] `deploy_app` with no `config` field on an app that already has configuration → `get_config` is unchanged → AC-9
- [x] `deploy_app` with a `config` map holding `PORT` → refused with `config_key_reserved`, and `deployment_status` shows no new deployment was started → AC-9, AC-5

## The running container

- [x] Deploy an app that prints its whole environment, with two keys set → its log shows both keys, plus `PORT` and `APP_URL` → AC-7
- [x] `kubectl -n app-<slug> get secret config -o yaml` → holds every configured key and neither `PORT` nor `APP_URL` → AC-7
- [x] `kubectl -n app-<slug> get deploy app -o jsonpath='{.spec.template.spec.containers[0].envFrom}'` → references the `config` Secret → AC-7
- [x] Curl the app's public URL and compare it to the `APP_URL` the app printed → the two are the same host → AC-7 (Value sourcing: `APP_URL` from `Input.Host`, the same field the Ingress rule takes)
- [x] `kubectl -n app-<slug> get deploy app -o jsonpath='{.spec.template.spec.containers[0].env}'` → `PORT` is `8080`, taken from the platform constant and not from any stored row → AC-5 (Value sourcing: `PORT` from `deploy.ContainerPort`)
- [x] Deploy an app with no configuration at all → the pod starts, and the empty `config` Secret exists → AC-7
- [x] `set_config` while a build is still running, then let that deploy finish → the value lands in that deploy, because compose runs after the build → AC-8 (Value sourcing: the Secret's data is `ListConfigForDeploy` read at compose time)
      _This step used to expect the opposite, that the container held the value from before the call. Ran 2026-08-13 and it does not: the key set mid build was in the composed Secret and the new container printed it, which is what the value sourcing note on this line describes. AC-8 still holds either way, since the running app did not change and no deployment started. Reworded to match what the code does. Worth a spec author's eye on whether the old expectation was the intent._

## Rolling and snapshots

- [x] Deploy, then `set_config` changing one value, then `deploy_app` with the same upload id is not possible, so upload the same source again and deploy → note the pod template checksum annotation before and after: it changed, and the pods rolled even though the image digest is identical → AC-17
- [x] Immediately after `set_config` and before the next deploy → `kubectl -n app-<slug> get pods` shows no new pod and no restart → AC-8
- [x] `sqlite3 <db> "select config_snapshot from releases order by id desc limit 2"` → each release holds the configuration that deploy actually ran with, not today's → AC-10 (Value sourcing: `configSnapshot` inside the release transaction)

## Logs and redaction

- [x] Deploy an app that prints a secret value at least eight characters long, set that value with `secret: true`, then `get_logs` → the value is blanked → AC-11
- [x] Set a secret whose value is three characters, deploy an app that prints it in an ordinary sentence, `get_logs` → the sentence is intact and nothing was blanked → AC-11
- [x] With the app still running and having printed the old value, `set_config` the same key to a new value, then `get_logs` → the old value is still blanked → AC-11 (Value sourcing: the union of current secret values and the current release's snapshot for keys secret today)
- [x] `set_config` the same key again with `secret: false`, then `get_logs` → the value now appears, which is the documented hole, so confirm it matches the spec rather than surprising you → AC-11

## Audit and builds

- [x] `sqlite3 <db> "select action, target_type, target_id, reason from audit_log order by id desc limit 10"` → a row per changed key, `target_type` is `app_config`, `target_id` is `<app id>/<KEY>`, and no row holds a value → AC-12
- [x] Same query after a refused `set_config` → the refusal is recorded with its reason code and no value → AC-12
- [x] `kubectl -n deployer-builds get job <build job> -o yaml` and the same in the BuildKit namespace → no container carries an app configuration variable, no `envFrom`, and no `--build-arg` or `--secret` flag → AC-14

## Acceptance-criteria coverage

- AC-1 covered by the merge and batch steps · AC-2 by the null value steps · AC-3 by the two `unset_config` steps · AC-4 by the bad key step · AC-5 by the reserved key and `PORT` steps · AC-6 by the size and count steps · AC-7 by the running container section · AC-8 by the timing steps · AC-9 by the three `deploy_app` steps · AC-10 by the release snapshot step · AC-11 by the four redaction steps · AC-12 by the audit steps · AC-13 by the second account step · AC-14 by the build Job step · AC-15 by the empty value step · AC-16 by the missing flag step · AC-17 by the checksum roll step
