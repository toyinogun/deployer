# Verify: async deployment jobs & status · spec 0005 · updated 2026-08-12

_Steps derived from spec 0005 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

## Agent session against the real cluster

- [x] Upload `testdata/sample-go`, then call `deploy_app` and time it → returns in well under a second, and the time does not vary with build time → AC-1, AC-19
- [x] Read the `deploy_app` response → carries `deployment_id`, `name`, `slug`, `url` (`https://<slug>.<DEPLOYER_APP_DOMAIN>`), `state` = `queued`, and no `release_number` or `image_digest` → AC-2
- [x] Poll `deployment_status` with that `deployment_id` every few seconds → reports `queued`, then a building state, then `healthy` → AC-6, AC-19
- [x] Read the final healthy payload → carries `release_number` and `image_digest`, and the `url` matches the one `deploy_app` returned → AC-7, AC-19
- [x] Read the healthy payload's `timeline` → five entries, `queued` → `building` → `pushing` → `deploying` → `healthy`, in `occurred_at` order, no `detail` field anywhere → AC-8
- [x] Open the app's `url` in a browser on the tailnet → the sample app answers → AC-19
- [x] Call `deployment_status` with `name` instead of the id → reports the same deployment → AC-6
- [x] Deploy the same app again, then status the first deployment id → `cancelled`, reason `superseded`, `superseded_by` = the second id; the second runs to `healthy` → AC-12, AC-20
- [x] Check the first deployment's row: `select failure_reason from deployments where id = '<first>'` → `superseded` → AC-12
- [x] Call `deployment_status` with neither argument, and with both → both fail with `deployment_unknown`, byte identical → AC-5
- [x] Call `deployment_status` with a made up id → the same `deployment_unknown` wording as a real id belonging to another account → AC-9
- [x] Query `audit_log` after a successful status call → no new row; after a refused one → exactly one row, action `status`, denied, reason `deployment_unknown` → AC-10
- [x] Queue enough deploys to hold the loop past `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` (an app that never becomes ready fails on `READY_TIMEOUT_SECONDS` first, so it never reaches the budget) → the deployments that age past the budget end `failed` with reason `timeout`, and any build Job they had is gone from `DEPLOYER_BUILD_NAMESPACE` → AC-14, AC-15
- [x] Read `deploy_app`'s tool description from the MCP client → states the call returns immediately with a `deployment_id`, names `deployment_status` as how to learn the outcome, gives a rough first build time, and still carries the upload contract, the `PORT` rule, and the non root rule → AC-4

## Commands

- [x] `go test -race ./...` → passes → AC-11, AC-13
- [x] `grep -rn "PollInterval\|DeployBudget" internal/mcp/` → no matches: the MCP package no longer reads the reconcile interval → AC-3
- [x] `git status --short internal/store/migrations` → empty: no migration was added → AC-13
- [x] `sqlc generate && git diff --stat internal/store/sqlcgen` → no diff beyond the two new read queries → AC-13
- [x] `grep -c "Reason = \"" internal/domain/reason.go` → eleven codes, all `Valid()`, all with a message (the reason table test) → AC-11

## Value sourcing, one step per row

- [x] `url`: deploy an app whose name needs slugging (mixed case, spaces) → `deploy_app` and `deployment_status` return the same `https://<slug>.<domain>`, and it is never read from a stored column
- [x] `state` on deploy: stop the reconcile loop (no cluster access), then `deploy_app` → still returns `queued` in under a second, proving the handler reads nothing back → AC-1, AC-3
- [x] status by id vs by name: create two apps, ask by each name → each reports its own most recent deployment, never the other's
- [ ] `reason`/`message`: fail a deploy with a root image → status carries `image_runs_as_root` and its one line, and nothing from the build log
- [x] `release_number`/`image_digest`: status the same deployment before and after it turns healthy → absent, then present, matching the `releases` row
- [x] `superseded_by`: deploy three times in a row → deployment 1 points at 2, deployment 2 points at 3, ordered by id and not by `created_at`
- [x] `timeline`: write an event with a `detail` holding a raw cluster message, then status → the message appears in no field of the payload → AC-8
- [x] Watchdog deadline: a deployment that sat `queued` behind a long build past the budget is failed with `timeout` once the drive ahead of it returns → AC-14
- [ ] Watchdog budget on resume: restart the control plane mid build → the resumed deployment gets what is left of its budget, not a fresh one → AC-14a

## Acceptance-criteria coverage

- AC-1, AC-2, AC-19 · the timed deploy and the poll to healthy
- AC-3 · the grep plus the loopless deploy
- AC-4 · the description read
- AC-5, AC-9, AC-10 · the argument and permission steps
- AC-6, AC-7, AC-8 · the status payload and timeline steps
- AC-11, AC-13 · the command steps
- AC-12, AC-20 · the supersession steps
- AC-14, AC-14a, AC-15, AC-16, AC-17, AC-18 · the watchdog steps
