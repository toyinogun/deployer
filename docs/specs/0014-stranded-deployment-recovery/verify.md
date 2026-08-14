# Verify: stranded deployment recovery · spec 0014 · updated 2026-08-14
_Steps derived from spec 0014 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

## Cluster (the fake clientset cannot prove these: it resolves nothing and fails nothing on its own)
- [ ] Deploy an app, wait until its deployment reaches `building`, then `kubectl delete job -n deployer-builds build-<deployment-id>` mid build → within a tick or two `get_status` reports the deployment `failed` with `build_failed`, not `timeout`, and well inside the deploy budget → AC-1, AC-2
- [ ] Immediately after that failure, `delete_app` on the same app → it is accepted, because no deploy is in flight any more → AC-2
- [ ] Strand a row whose build is still genuinely running (kill the control plane pod mid build, let it restart, then watch the row while the Job keeps going) → the deployment stays in `building` and is not failed while the Job is alive → AC-4a
- [ ] Let that same restarted build finish → the row is picked up and driven on to `healthy` rather than thrown away → AC-3, AC-5
- [ ] `kubectl get deploy deployer -n deployer-system -o jsonpath='{.spec.replicas}{.spec.strategy.type}'` → `1Recreate`. The check is only correct while this holds → AC-7

## Commands
- [ ] `go test -race ./internal/reconcile/ ./internal/store/` → passes, covering the failed, gone, succeeded, running and unreadable branches, the ordering, the fairness and the supersession race → AC-1 to AC-7, AC-9
- [ ] `kubectl get cm deployer-config -n deployer-system -o yaml | grep -c STRAND` → `0`, and `git diff` on `internal/store/migrations/` is empty: no new setting, no migration → AC-8

## Value sourcing
- [ ] Strand a Dockerfile path deployment (an upload with a root `Dockerfile`) rather than a Buildpacks one → it is still ended with `build_failed`, which proves the Job is looked up in `deployer-builds-dockerfile`, the namespace the row's `build_path` derives, not the default one → Value sourcing, Check a row
- [ ] Read the deployment back after a release and check `claimed_at` is null in the database, not only `claimed_by` → the claim query tests `claimed_at`, so clearing only the other one would look right and never be adopted → AC-3, Value sourcing, Release a row
- [ ] Queue a fresh deploy for app B while a stranded row for app A is waiting to be adopted → app B's deploy starts first → AC-5, Value sourcing, Adopt a released row
- [ ] Take a row that is both stranded with a failed Job and past its deploy budget → the recorded reason is `build_failed` → AC-6
- [ ] Take a row whose Job succeeded but which never finishes → it is still ended at the deploy budget measured from `created_at`, with no fresh window per adoption → AC-9

## Acceptance-criteria coverage
- AC-1 covered by the killed Job step and the test command · AC-2 by the killed Job and delete_app steps · AC-3 by the resumed build and the claimed_at read · AC-4 by the test command (read error) · AC-4a by the live build step · AC-5 by the resumed build and the fairness step · AC-5a by the test command (supersession race) · AC-5b by the test command · AC-6 by the stranded and overdue step · AC-7 by the replica check · AC-8 by the config and migration check · AC-9 by the budget step
