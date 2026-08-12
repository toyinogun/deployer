# Verify: application logs · spec 0006 · updated 2026-08-12
_Steps derived from spec 0006 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

The unit suite already pins the pure logic and the tool's branching against fakes. What is left here is what the fake clientset cannot prove: real names on cluster DNS, a real kubelet's timestamp format, a real crash loop, and the RBAC the control plane actually holds. Run these against the deployed platform with `DEPLOYER_TOKEN` set.

## Commands

- [x] `kubectl get clusterrole deployer-app -o yaml | grep -A2 'pods/log'` → `get` is granted on `pods/log`, and `pods` carries `list`. No manifest change was needed → AC-14
- [x] Deploy a small app that prints a line a second, wait for `healthy`, then call `get_logs` with `{"name":"<app>"}` → entries come back oldest to newest, each with a real RFC3339 timestamp the kubelet wrote, and `tail_lines` reads `200` → AC-1, AC-2
- [x] Call `get_logs` with `{"name":"<app>","tail_lines":5000}` → `tail_lines` reads `1000` and `clamped` is `true`. Repeat with `tail_lines: 0` and `tail_lines: -1` → both read `200` with no `clamped` field → AC-2
- [x] Let the app print well past 64 KiB, then call `get_logs` with `tail_lines: 1000` → `truncated` is `true`, `dropped` is above zero, the last entry is the app's newest line, and the whole `entries` block measures under 64 KiB → AC-3
- [ ] Deploy an app that starts, prints a panic, and exits, so the pod crash loops. Call `get_logs` → `previous` carries the panic, and it is still there after the current container has printed enough to fill its own block → AC-4
- [ ] Redeploy the crash looping app so two pods exist for a moment, and call `get_logs` during the roll → the entries are the newest pod's, and `note` says an older pod may still be serving → AC-5
- [x] Have the app print, on separate lines: `Authorization: Bearer <a long token>`, a JWT, `postgres://u:pw@host/db`, `AKIAIOSFODNN7EXAMPLE`, `API_TOKEN=<value>`, and a 300 character line of ordinary text. Call `get_logs` → each of the first five is blanked, the plain long line is untouched → AC-6
- [ ] Call `get_logs` immediately after `deploy_app` returns, while the state is `building` → the call succeeds, `entries` is empty, `state` reads `building`, and `note` says no container has started yet. No error → AC-7, AC-10
- [x] Scale the app's Deployment to zero replicas while its latest deployment is `healthy`, then call `get_logs` → `state` reads `healthy` and `note` says the output is no longer available → AC-7
- [ ] Call `get_logs` with a name that was never deployed, and with the name of an app belonging to a second account → both return `app_unknown` with the identical message, and neither reveals that the second app exists → AC-8
- [ ] After the two refusals above, read the audit table → exactly two rows, action `logs`, against the calling account. Then make one successful read and re read the table → no new row → AC-9
- [ ] Call `get_logs` for an app whose pod is running, then delete the pod mid call if you can force it → the answer is either complete or `internal`, never a half block presented as the app's output → AC-10
- [ ] Compare `get_logs` output against `kubectl logs -n <build namespace>` for a build Job running at the same time → no build output, no control plane line, and nothing outside `app-<slug>` appears in the response → AC-11
- [ ] `sqlite3 <db> '.schema'` before and after a run of log reads → identical. The only write on this path is the audit row → AC-12
- [x] Read the `get_logs` description an MCP client shows → it states the snapshot semantics, `200` and `1000`, that the oldest lines are dropped, the previous container block, and that redaction is best effort → AC-13

## Value sourcing

One step per row of the spec's Value sourcing table, exercising the edge that breaks if the source is wrong.

- [ ] Rename an app's display name in the database while the slug stays put, then call `get_logs` with the new name → the read still lands in `app-<original slug>`, proving the namespace comes from the slug and not the name → derived namespace
- [ ] Create a pod in the app's namespace carrying a different `app.kubernetes.io/name` label → it is never picked, proving the selector is the app's own → which pod
- [ ] Add a second container to the app's pod by hand → `get_logs` still reads the `app` container and does not error on ambiguity → which container
- [ ] Force the app container into `Waiting` (a bad image pull) while an old terminated container status is still present → the empty case fires and the log API is not called → whether the empty case applies
- [ ] Restart the app exactly once and confirm `previous` appears; delete the pod so a fresh one starts with `restartCount` zero and confirm `previous` is absent → whether a previous block exists
- [ ] Rotate `REGISTRY_PASSWORD`, restart the control plane, have the app print the new value → the new value is blanked and the old one is not → platform placed secret redaction

## Acceptance-criteria coverage

- AC-1 · AC-2 · AC-3 · AC-4 · AC-5 · AC-6 · AC-7 · AC-8 · AC-9 · AC-10 · AC-11 · AC-12 · AC-13 · AC-14 all covered above.
