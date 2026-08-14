# Verify: stranded deployment recovery · spec 0014 · updated 2026-08-14

_Steps derived from spec 0014 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

_Rewritten after the 2026-08-14 verify run. The two cluster steps that used to lead this file were dropped: both faults that strand a row are internal, so neither can be induced from outside, and the steps as written were answered by `awaitBuild` and `Sweep` rather than by the check. The reasoning is in [rationale.md](rationale.md), "What the gates found". Fault injection tests carry AC-1 and AC-2 instead._

## Commands
- [x] `go test -race ./internal/reconcile/ ./internal/store/` → passes, covering the failed, gone, succeeded, running and unreadable branches, the ordering, the fairness, the supersession race, and the two fault injection cases below → AC-1 to AC-7, AC-9, AC-10
- [x] The write fault: a store whose `Transition` fails leaves a row in `building` after its drive, and the next tick ends it with the reason its Job gives, not `timeout`. This is the fault the spec exists for → AC-1, AC-2
- [x] The sweep fault: a store whose `ListNonTerminal` fails on the startup `Sweep` and succeeds afterwards leaves in flight rows unattended, and the tick recovers them → AC-1
- [x] A release whose guard matches no row returns `false` rather than an error, and the loop logs it as not released rather than as a success → AC-10
- [x] `kubectl get cm deployer-config -n deployer-system -o yaml | grep -c STRAND` → `0`, and `git diff` on `internal/store/migrations/` is empty: no new setting, no migration → AC-8

## Cluster
- [x] `kubectl get deploy deployer -n deployer-system -o jsonpath='{.spec.replicas}{.spec.strategy.type}'` → `1Recreate`. Confirmed 2026-08-14. This is a precondition, not a detail: under two processes the check can end a deployment the other one is driving → AC-7, AC-11
- [x] A normal deploy is unaffected by the check sitting in the tick: `queued` → `healthy`, confirmed on the cluster 2026-08-14 in 43 seconds on the branch build → AC-7
- [x] A Dockerfile path deploy puts its build Job in `deployer-builds-dockerfile`, confirmed 2026-08-14, which is the namespace derivation the stranded lookup uses through the same `buildNamespace` helper → Value sourcing, Check a row

## Acceptance-criteria coverage
- AC-1 covered by the two fault injection steps and the test command · AC-2 by the write fault step · AC-3 by the test command (release and adopt, asserting `claimed_at`) · AC-4 by the test command (read error) · AC-4a by the test command (running Job) · AC-5 by the test command (fairness and resume) · AC-5a by the test command (supersession race) · AC-5b by the test command · AC-6 by the test command (stranded and overdue ordering) · AC-7 by the replica check and the normal deploy step · AC-8 by the configuration and migration check · AC-9 by the test command (budget from `created_at`) · AC-10 by the release logging step · AC-11 by the replica check

_No step here proves the check against a live cluster, and none can. Both triggering faults are internal, so fault injection is the only available proof. That limitation is recorded in the spec's Consequences rather than hidden here._
