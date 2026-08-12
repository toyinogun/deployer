# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- First deploy end to end: an agent can upload a source tarball, call one MCP tool, and get back a working HTTPS hostname for the running app (see spec 0004).
- `deploy_app` MCP tool, taking `name` and `upload_id`. It builds the source with Cloud Native Buildpacks in a Kubernetes Job, pushes the image to the in cluster registry, composes the app's Deployment, Service and Ingress, waits for the app to answer, and returns the name, slug, URL, deployment id, release number and image digest. The call blocks for the whole deploy, which can take minutes on a cold build; a status tool replaces the waiting in the next slice.
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

### Changed

- Deployments now walk `queued`, `building`, `pushing`, `deploying`, `healthy`, one recorded transition at a time, with a single reconcile loop as the only writer of state after `queued`. Only one build runs at a time, platform wide.
- Every app workload is composed field by field in Go and referenced by image digest, never by tag. The only caller derived values anywhere in a pod spec are the platform derived slug and the digest of an image the platform itself built.
- The uploaded tarball is deleted once its deployment reaches a terminal state, whichever state that is.

### Security

- Both surfaces, the upload endpoint and MCP, authenticate with the same bearer token, and every `deploy_app` call writes one audit row with the outcome.
- An image whose user is empty or `0` is refused with `image_runs_as_root` before any Kubernetes object for that app is created or updated.
- The build pod runs as the builder image's own non root user with all capabilities dropped, no privilege escalation, `seccompProfile: RuntimeDefault`, `backoffLimit: 0` and a deadline from `DEPLOYER_BUILD_TIMEOUT_SECONDS`.
- The registry credential reaches an app namespace only as an `imagePullSecret` the kubelet consumes; it is never mounted, projected, or exposed as an environment variable in an app pod.
- Known limits, stated rather than implied: auth is one seeded token with no revocation path (spec 0004 follow-up, feature 8 owns real minting), and the registry's write credential is mounted into the build container beside code the platform did not author, so a hostile buildpack could push any tag in the registry. Running apps are unaffected because every deploy is by digest.
