# Verify: rollback & release history · spec 0011 · updated 2026-08-13

_Steps derived from spec 0011 acceptance criteria. `/check verify` runs these; `/test` locks the durable ones._

These run against the real cluster with a real token. The suite already covers the
logic; what only the cluster can prove is that the pods actually roll on an
unchanged image digest, that no build Job appears, and that a rollback finishes
inside the ordinary deploy budget.

Set up once, and keep the ids: deploy an app with `LOG_LEVEL=info` (plain) and
`API_KEY=old-key` (secret), wait for healthy, then deploy the same app again with
`LOG_LEVEL=debug` and `FEATURE_X=on`, and unset `API_KEY`. That leaves two
releases with genuinely different configuration.

## MCP / manual

- [x] `list_releases` on the app → two rows, newest first, release 2 then release 1 → AC-1
- [x] the same listing → exactly one row has `current` true, and it is release 2 → AC-2
- [x] the same listing → every row carries `release_number`, `image_digest`, `created_at`, `deployment_id`, and no key or value from the app's configuration anywhere in the payload → AC-1, AC-4
- [x] `list_releases` on a freshly created app that has never been healthy → an empty list, returned successfully, not a refusal → AC-3
- [x] `list_releases` on an app name that does not exist → `app_unknown` → AC-5
- [ ] `list_releases` from a second account's token, naming the first account's app → `app_unknown`, word for word the same message the missing name got → AC-5
- [x] `rollback_app` with `release_number` 1 → returns within a second or two with `deployment_id`, `state` `"queued"`, `name`, `slug`, `url`; it does not block on the rollout → AC-6
- [x] `deployment_status` on that deployment id → the timeline is exactly `queued`, `deploying`, `healthy`, with no `building` and no `pushing` → AC-11
- [x] `get_config` after that rollback reports healthy → `LOG_LEVEL` is `info`, `API_KEY` is present and secret, `FEATURE_X` is gone → AC-13, AC-14
- [x] `list_releases` again → release 3 exists, carries release 1's `image_digest`, and is the only `current` row; release 1's own row is unchanged → AC-16
- [x] `rollback_app` with `release_number` 99 → `release_unknown`, and `list_releases` shows no new release and `deployment_status` finds no new deployment → AC-7
- [x] `rollback_app` with `release_number` 0, and again with a negative number → `release_unknown`, the same message both times → AC-7
- [ ] `rollback_app` from a second account's token naming the first account's app, with a `release_number` that really exists → `app_unknown`, not `release_unknown`: ownership is decided before the number is looked at → AC-8
- [x] `rollback_app` naming the release that is already current → accepted, runs as an ordinary rollback, reaches healthy → AC-18
- [x] start a `deploy_app`, then immediately `rollback_app` on the same app → `deployment_status` on the deploy reports `cancelled` with reason `superseded` → AC-10
- [x] start a `rollback_app`, then immediately `deploy_app` on the same app → the rollback reports `cancelled` with reason `superseded` → AC-10
- [x] read both tool descriptions from the MCP tool list → `list_releases` states the newest twenty bound and that older releases are not reachable; `rollback_app` states that it does not wait, that it replaces image and configuration together, that it supersedes a deploy in flight, and that a `set_config` landing during it is reverted → AC-22, AC-25

## Commands

- [x] `kubectl -n deployer-builds get jobs` before and after a rollback → the same Jobs both times: a rollback creates none → AC-11
- [x] `kubectl -n app-<slug> get pods` across a rollback → the pods are replaced, even though the image digest did not change. This is the one the suite cannot prove → AC-12
- [x] `kubectl -n app-<slug> get deploy <name> -o jsonpath='{.spec.template.metadata.annotations}'` before and after → the configuration checksum differs while the image reference is identical → AC-12
- [x] `kubectl -n app-<slug> get secret config -o jsonpath='{.data}'` after the rollback → base64 decodes to release 1's configuration, and holds no `FEATURE_X` → AC-12
- [x] `kubectl -n app-<slug> get deploy <name> -o jsonpath='{.spec.template.spec.containers[0].image}'` → `<registry>/apps/<slug>@<digest>`, the repo recomposed from the slug rather than read off the row → AC-11
- [x] time one rollback end to end → well inside `DEPLOYER_DEPLOY_TIMEOUT`, and noticeably faster than a build deploy of the same app → AC-19
- [x] `env | grep DEPLOYER_` on the control plane pod → no new variable was added for this feature → AC-19
- [x] break a rollback deliberately (roll back to a release whose image no longer pulls, or scale the app so pods cannot schedule) → the deployment ends `failed` with its reason, `list_releases` shows no new release, the `current` row has not moved, and `get_config` is unchanged → AC-17
- [x] `sqlite3 <db> "select action, allowed, reason from audit_log order by rowid desc limit 5"` after an allowed and a refused rollback → one `rollback` row `allowed`, one `rollback` row denied carrying the reason code → AC-20
- [x] kill the control plane pod while a rollback is in `deploying`, let it restart → the sweep picks the row back up, still drives it as a rollback, and it finishes healthy without resolving an upload → AC-24
- [x] `sqlite3 <db> "select config_snapshot from releases order by rowid desc limit 1"` → the `{"KEY":{"value":...,"secret":...}}` shape, written by an ordinary deploy and not only by a rollback → AC-15
- [x] on an app whose newest release predates this feature (or one with its snapshot hand written to the bare string shape), roll back to it → every restored key reads as secret in `get_config` → AC-14
- [x] `set_config` on the app while a rollback is mid rollout, then let the rollback finish → the key you set is gone and `get_config` reports the snapshot, even though `set_config` returned success → AC-25

## Value sourcing coverage

One step per row of the spec's Value sourcing table, so a mis sourced value is
caught behaviourally and not only at design time.

- [x] `release_number`, `image_digest`, `created_at` come from the release row: compare `list_releases` against `sqlite3 <db> "select release_number, image_digest, created_at from releases"` → AC-1
- [x] `deployment_id` in the listing is the deployment that minted the release, not the app's latest: check it against `releases.deployment_id` for a release that is not the newest → AC-1
- [x] `current` comes from `apps.current_release_id`: set it to null by hand and confirm no row reports current, rather than the newest row doing so → AC-2
- [x] the twenty row bound is the Go constant: an app with more than twenty releases returns exactly twenty and no `limit` argument is accepted → AC-1
- [x] the empty list comes from a query with no rows: a never healthy app returns success, not `app_unknown` → AC-3
- [x] `source_release_id` is resolved from (`app_id`, `release_number`): with two apps that each have a release 1, roll back app A to release 1 and confirm the deployment names A's release, not B's → AC-7
- [ ] `image_digest` on the rollback is copied from the source release at creation: read `deployments.image_digest` while it is still `queued`, before anything ran → AC-9
- [x] `url` comes from `DEPLOYER_APP_DOMAIN`: the rollback's url matches `deploy_app`'s for the same app, character for character → AC-6
- [x] `state` is always the literal `queued`: it reads `queued` even when the app already has a healthy deployment → AC-6
- [x] the deployment's kind comes from `source_release_id` and not from its state: a queued rollback and a queued build deploy are the same state, and only the rollback skips the build → AC-24
- [x] the image repo is recomputed from the slug: `deployments.image_repo` is null on the rollback row while the running pod's image still resolves → AC-11
- [x] the Secret comes from the source release's snapshot and not `app_config`: change a key without deploying, roll back, and confirm the container got the snapshot's value → AC-12
- [x] each key's `is_secret` on a build deploy's release comes from `app_config`: deploy with one plain and one secret key and read the snapshot back → AC-15
- [x] each restored key's flag comes from the snapshot: a new shape snapshot restores the recorded flags, an old shape one restores everything secret → AC-14
- [x] the rollback's own release records the map it deployed with: its snapshot equals the source release's snapshot, not the table's state at the time → AC-16
- [x] a refusal's message is `domain.Reason.Message`: `release_unknown` and `app_unknown` read as their one line, never as a wrapped error → AC-21

## Acceptance-criteria coverage

- AC-1 listing shape and bound · AC-2 exactly one current · AC-3 empty list · AC-4 no snapshot in the payload · AC-5 unknown and another account's app · AC-6 rollback returns queued · AC-7 bad release number · AC-8 ownership before the number · AC-9 digest copied at creation · AC-10 supersession both directions · AC-11 the short path · AC-12 Secret and checksum from the snapshot · AC-13 app_config rewritten · AC-14 old shape snapshots · AC-15 new snapshot shape on every deploy · AC-16 the rollback's own release · AC-17 a failed rollback changes nothing · AC-18 rolling back to current · AC-19 the same budget, no new configuration · AC-20 audit rows · AC-21 the closed reason code · AC-22 both descriptions · AC-23 both tools over a real session · AC-24 resume mid rollback · AC-25 set_config reverted
- AC-23 is proved by the suite (`internal/mcp/releases_test.go`, through a real client and server session) rather than by a step here, since it is about the argument schema rather than the cluster.
