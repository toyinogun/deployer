# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- First deploy end to end: an agent can upload a source tarball, call one MCP tool, and get back a working HTTPS hostname for the running app (see spec 0004).
- `deploy_app` MCP tool, taking `name` and `upload_id`. It builds the source with Cloud Native Buildpacks in a Kubernetes Job, pushes the image to the in cluster registry, composes the app's Deployment, Service and Ingress, waits for the app to answer, and returns the name, slug, URL and deployment id. The call does not wait for the build; `deployment_status` reports the outcome (see spec 0005 below).
- `POST /v1/uploads`, which accepts a gzipped tar body against a bearer token, rejects a body over `DEPLOYER_MAX_UPLOAD_BYTES` with 413 before writing it all to disk, records the size and SHA-256, and returns the upload id and its expiry.
- `GET /v1/uploads/{id}`, a single use redeem path used only by the build Job's init container to fetch the tarball.
- `fetch-source` subcommand, run as the build Job's init container. It verifies the recorded SHA-256 before unpacking and refuses any archive entry with an absolute path, a `..` component, a symlink, a hardlink or a device node, along with any archive over the file count or uncompressed size caps.
- Redeploying an app under the same name keeps the same hostname and increments the release number, so a link already handed to someone keeps working.
- A closed set of failure reason codes on every failed deploy (`upload_invalid`, `upload_expired`, `source_rejected`, `build_failed`, `build_no_digest`, `image_runs_as_root`, `app_never_ready`, `timeout`, `internal`), returned to the caller and stored on the deployment. Raw build output never reaches the response, the database, or the platform log at info level.
- A startup sweep that reconciles every non terminal deployment against the cluster, so a control plane restart during a build resumes a live Job and fails one that has vanished, rather than leaving a deployment stuck.
- An in cluster container registry (`distribution` v3) in `deployer-system`, backed by a Longhorn volume, with htpasswd auth held only as a `SealedSecret`, and no Ingress. Plus the `deployer-builds` namespace with `enforce: restricted` pod security.
- Bootstrap seeding at startup: when `DEPLOYER_BOOTSTRAP_TOKEN` is set, the platform ensures one `bootstrap` account and one token row holding that token's SHA-256 hash. Running it again changes nothing, and the raw token is never logged.
- New settings, all validated at startup so a missing or malformed one fails the boot naming the variable: `DEPLOYER_BOOTSTRAP_TOKEN`, `DEPLOYER_PUBLIC_URL`, `DEPLOYER_INTERNAL_URL`, `DEPLOYER_BUILDER_IMAGE`, `DEPLOYER_SELF_IMAGE`, `DEPLOYER_BUILD_UID`, `DEPLOYER_BUILD_GID`, `DEPLOYER_DEPLOY_TIMEOUT_SECONDS`, `DEPLOYER_BUILD_TIMEOUT_SECONDS`, `DEPLOYER_READY_TIMEOUT_SECONDS`, `DEPLOYER_RECONCILE_INTERVAL_SECONDS`, `DEPLOYER_APP_DEFAULT_CPU`, `DEPLOYER_APP_DEFAULT_MEMORY`, `DEPLOYER_APP_LIMIT_CPU`, `DEPLOYER_APP_LIMIT_MEMORY`, `DEPLOYER_MAX_UPLOAD_FILES`, `DEPLOYER_MAX_EXTRACTED_BYTES`.
- A CI step that reads the pinned builder image's own `CNB_USER_ID` and `CNB_GROUP_ID` and fails on drift from `DEPLOYER_BUILD_UID` and `DEPLOYER_BUILD_GID`.
- `testdata/sample-go`, a minimal sample app used to prove the whole path against the real cluster.
- Async deployment jobs and status: the deploy call stops waiting, so a client's request timeout has nothing to do with how long a build takes (see spec 0005).
- `deployment_status` MCP tool, taking exactly one of `deployment_id` or `name`. It reports the deployment's state and a timeline of every recorded transition, plus the release number and image digest once healthy, the reason code and its one line when failed, and `superseded` with the id that replaced it when cancelled. Polling it every few seconds is the intended shape, and its description says so.
- A watchdog inside the reconcile loop, which fails any deployment that runs past `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` with reason `timeout` and deletes its build Job. Every deployment now ends by itself, which the blocking call used to cover by giving up.
- Two more failure reason codes, `deployment_unknown` and `superseded`, taking the closed set from nine to eleven. `superseded` describes a cancellation rather than a failure.
- A fetch of an upload whose tarball has already been deleted answers `410 Gone` in the same words as an expired upload, rather than an internal error. That path became reachable once the watchdog could delete a tarball while a build was still fetching it.
- Application logs: an agent can read back the output of an app it deployed, so a misbehaving app can be debugged without anyone opening a terminal (see spec 0006).
- `get_logs` MCP tool, taking an app `name` and an optional `tail_lines`. It returns the newest lines still on the node, oldest first, each with the timestamp the kubelet recorded. It is a snapshot, not a stream: no follow, no search, no time window. `tail_lines` defaults to 200 and is capped at 1000, and a larger ask is clamped rather than refused, with the response echoing the value applied. The answer is capped by size as well as by line count; when the size cap is hit the oldest lines go, never the newest, and `truncated` and `dropped` say so.
- An app that crashed and restarted gets a second, smaller block of the dead container's output in `previous`, capped independently of the current block so restart noise can never squeeze the crash out of the answer. It is absent when there has been no restart.
- Secrets are blanked before the lines are returned, on a best effort basis the tool description states plainly: bearer and basic credentials, JWTs, passwords inside URLs, AWS style access key ids, and assignments named like a key, token, secret, or password. The registry credential the platform placed in the app's namespace is matched exactly, because that is the one value the platform knows for certain is secret. High entropy alone is deliberately not a trigger, because blanking on it destroys ordinary output.
- An app with no container started yet is a success with no entries, carrying the deployment's current state and one sentence saying why there is nothing to show. That is what every read during a build sees, and it now reads the same way whether the app's namespace is missing or merely not readable by the platform yet.
- One more reason code, `app_unknown`, taking the closed set from eleven to twelve. A log read for a name that does not exist and one for an app belonging to another account get the same answer, so the tool cannot be used to learn which app names exist. Only a refusal is audited; a successful read is not an access decision.
- No new settings. The log bounds are constants, because they are decisions about what fits an agent's context window rather than per deployment tuning, and nothing about a log read is stored: the platform reads from Kubernetes at the moment of the call and hands the lines back.

### Changed

- Deployments now walk `queued`, `building`, `pushing`, `deploying`, `healthy`, one recorded transition at a time, with a single reconcile loop as the only writer of state after `queued`. Only one build runs at a time, platform wide.
- Every app workload is composed field by field in Go and referenced by image digest, never by tag. The only caller derived values anywhere in a pod spec are the platform derived slug and the digest of an image the platform itself built.
- The uploaded tarball is deleted once its deployment reaches a terminal state, whichever state that is.
- `deploy_app` returns as soon as the queued deployment is written, carrying `deployment_id`, `name`, `slug`, `url` and `state`. It no longer carries `release_number` or `image_digest`, because neither exists yet; read them from `deployment_status` once the deployment is healthy.
- `DEPLOYER_DEPLOY_TIMEOUT_SECONDS` keeps its name, its default and its startup validation, and now bounds a deployment from creation to a terminal state rather than bounding one MCP call. Queue time counts against it. No new setting was added.
- A deploy waiting its turn now sits in `queued` where a caller can see it. One build at a time, platform wide, was always true; it used to be invisible inside a hung connection.
- Redeploying an app writes `superseded` onto the cancelled deployment's row as well as its event, and the deployment that replaced it is reported as `superseded_by`, derived from the app's next deployment by id rather than by timestamp.
- A drive whose deployment was cancelled underneath it by a redeploy now stops quietly instead of writing over a row something else correctly ended.

### Security

- Both surfaces, the upload endpoint and MCP, authenticate with the same bearer token, and every `deploy_app` call writes one audit row with the outcome.
- An image whose user is empty or `0` is refused with `image_runs_as_root` before any Kubernetes object for that app is created or updated.
- The build pod runs as the builder image's own non root user with all capabilities dropped, no privilege escalation, `seccompProfile: RuntimeDefault`, `backoffLimit: 0` and a deadline from `DEPLOYER_BUILD_TIMEOUT_SECONDS`.
- The registry credential reaches an app namespace only as an `imagePullSecret` the kubelet consumes; it is never mounted, projected, or exposed as an environment variable in an app pod.
- `deployment_status` is scoped to the caller's account before any field is read, and an id or name that does not exist gets the same answer as one belonging to someone else, so status cannot be used to learn which deployments exist. A refused read writes one audit row; an allowed one writes none.
- A deployment's internal event detail, which can hold a raw cluster message, is dropped at the one projection that builds the status timeline, so no write site can leak it into a response.
- Known limits, stated rather than implied: auth is one seeded token with no revocation path (spec 0004 follow-up, feature 8 owns real minting), and the registry's write credential is mounted into the build container beside code the platform did not author, so a hostile buildpack could push any tag in the registry. Running apps are unaffected because every deploy is by digest.
