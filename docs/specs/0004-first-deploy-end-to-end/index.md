# 0004. First deploy end to end: the tracer bullet from MCP call to a running app

**Date**: 2026-08-12
**Status**: Accepted

## Summary

This joins the four foundations into one working thread. An agent holding the platform's token uploads a source tarball over plain HTTP, calls a single MCP tool (`deploy_app`), and the platform builds that source into a container image with Cloud Native Buildpacks in a Kubernetes Job, pushes it to a registry running in the cluster, composes the app's workload field by field in Go, waits for it to answer on its port, and returns an HTTPS hostname. The call blocks for the whole thing, which is deliberate and temporary: slice 2 replaces the waiting with a job id and a status tool.

Deliberately narrow. One bootstrap account and one token, Buildpacks only, one fixed port, no configuration, no logs, no rollback, no listing, no delete. The point is that every segment of the pipe carries real traffic once, not that any segment is finished.

Reasoning, the options weighed, and the premise note: see [rationale.md](rationale.md). Hand run cluster steps: see [verify.md](verify.md).

## Requirements

**User stories**:

- As an AI coding agent, I want to deploy the app I just wrote with one tool call, so that I can hand the person a working link without them touching kubectl, Docker, or YAML.
- As an AI coding agent, I want a failed deploy to tell me a cause I can act on, so that I can fix my own app rather than guess.
- As an AI coding agent, I want redeploying the same app to keep its hostname, so that the link I already gave the person keeps working.
- As the operator, I want the platform to compose every workload itself, so that nothing an agent uploads can reach a pod spec.
- As the operator, I want a deploy interrupted by a platform restart to resolve one way or the other, so that no app is left half created and no row is stuck forever.

**Acceptance criteria**:

- **AC-1**: On startup, when `DEPLOYER_BOOTSTRAP_TOKEN` is set, the platform ensures one account named `bootstrap` exists and one `api_tokens` row holding that token's SHA-256 hash. Running it again with the same token changes nothing and creates no duplicate. The raw token is never logged, at any level.
- **AC-2**: `POST /v1/uploads` with a valid bearer token accepts a gzipped tar body, rejects a body over `DEPLOYER_MAX_UPLOAD_BYTES` with 413 before writing the whole thing to disk, writes the file under `DEPLOYER_UPLOAD_DIR`, records its size and SHA-256, and returns the upload id, its expiry, and nothing else. An absent, unknown, or revoked token gets 401 and writes an `audit_log` denial.
- **AC-3**: The MCP server exposes exactly one tool, `deploy_app`, taking `name` (required) and `upload_id` (required). Its description states the upload step, the exact endpoint, and that the call runs for minutes. An unknown or already redeemed `upload_id`, or one belonging to another account, fails the call without creating a deployment.
- **AC-4**: `deploy_app` resolves the app by `(account_id, name)`, creating it with a derived slug on first use and reusing the existing row afterwards. The hostname `<slug>.<DEPLOYER_APP_DOMAIN>` is therefore identical across every deploy of the same app.
- **AC-5**: The deployment walks `queued`, `building`, `pushing`, `deploying`, `healthy`, each transition written through the existing store `Transition` call, so each leaves one `deployment_events` row. No state is skipped and none is written twice.
- **AC-6**: The reconcile loop claims at most one deployment at a time and starts no second build until the claimed one reaches a terminal state.
- **AC-7**: The build Job is created in `DEPLOYER_BUILD_NAMESPACE`, which carries `enforce: restricted` pod security. Its pod runs non root with all capabilities dropped, `allowPrivilegeEscalation: false`, and `seccompProfile: RuntimeDefault`. It carries `activeDeadlineSeconds` from `DEPLOYER_BUILD_TIMEOUT_SECONDS` and `ttlSecondsAfterFinished` so completed Jobs are reaped without the platform deleting them.
- **AC-8**: The init container runs the deployer image's own `fetch-source` subcommand. It redeems the single use fetch token, verifies the recorded SHA-256 before unpacking, and rejects any entry with an absolute path, a `..` component, a symlink, a hardlink, or a device node, and any archive exceeding the file count or uncompressed size caps. A rejected archive fails the deployment and never reaches the builder.
- **AC-9**: The build container runs the pinned Paketo builder's `lifecycle -creator`, pushing to the tag the platform chose. The Job runs with `backoffLimit: 0`, so one Job is one attempt. On success the platform resolves that tag against the registry to get the digest and records it with `RecordBuildResult`. A Job that fails, or one that succeeds but whose tag resolves to nothing, fails the deployment with `build_failed` or `build_no_digest`.
- **AC-10**: Before composing any workload, the platform reads the pushed image's config from the registry. An image whose user is empty or `0` fails the deployment with `image_runs_as_root` and a line telling the caller to add a non root `USER`, and no Kubernetes object for that app is created or updated.
- **AC-11**: The platform creates the app namespace `app-<slug>` from spec 0003's template (ownership labels, restricted pod security labels, ResourceQuota, LimitRange) when it does not exist, and leaves it untouched when it does.
- **AC-12**: The platform mints the registry credential as a `kubernetes.io/dockerconfigjson` secret inside `app-<slug>` and references it from the Deployment's `imagePullSecrets`, refreshing it on every deploy. No registry credential in any form ever reaches an app container: the secret is consumed by the kubelet and is never mounted, projected, or exposed as an environment variable in the app pod.
- **AC-13**: The Deployment, Service, and Ingress are composed field by field in Go, with no string templating and no user supplied value anywhere in the pod spec except the image digest and the app's own slug. All of them are created or updated in place on every deploy, never deleted and recreated. The container references the image by digest, never by tag. The pod carries spec 0003's required security context, `PORT=8080` as its only environment variable, container port 8080, and the resource requests and limits from configuration. The Service is named `app`, listens on port 80, and targets 8080. The Ingress matches spec 0003's shape, with no `tls` block.
- **AC-14**: Readiness is a TCP socket probe on port 8080. The platform marks the deployment healthy only after the Deployment reports an updated, available replica, and does so through `MarkHealthy`, so exactly one `releases` row and the `apps.current_release_id` update land in the same transaction.
- **AC-15**: `deploy_app` returns, on success, the app name, slug, `https://<slug>.<DEPLOYER_APP_DOMAIN>`, the deployment id, the release number, and the image digest.
- **AC-16**: Every failure returns one of a closed set of reason codes with one short sanitized line: `upload_invalid`, `upload_expired`, `source_rejected`, `build_failed`, `build_no_digest`, `image_runs_as_root`, `app_never_ready`, `timeout`, `internal`. The same code is stored in `deployments.failure_reason`. Raw build output never appears in the response, the database, or the platform log at info level.
- **AC-17**: The overall call fails with `timeout` at `DEPLOYER_DEPLOY_TIMEOUT_SECONDS`, the build phase at `DEPLOYER_BUILD_TIMEOUT_SECONDS`, and the readiness wait at `DEPLOYER_READY_TIMEOUT_SECONDS`, each with its own reason. Every one of them, plus every other setting this feature adds, is validated in `internal/config` at startup, and a missing or malformed one fails the boot with an error naming it.
- **AC-18**: A control plane restart during a build leaves no deployment stuck. On start, the sweep reconciles every non terminal deployment against the cluster: a live Job is resumed, a Job that no longer exists fails the deployment with a reason.
- **AC-19**: Every `deploy_app` call writes exactly one `audit_log` row with action `deploy`, the resolved account or null, the allowed or denied outcome, and the app as target when one was resolved. The upload is validated before the app is touched, so a call failing on a bad `upload_id` audits with a null target and creates no app row.
- **AC-22**: The uploaded tarball is deleted from `DEPLOYER_UPLOAD_DIR` once its deployment reaches a terminal state, whichever state that is, and the existing retention sweep removes redeemed or expired uploads older than 24 hours (spec 0001, spec 0002 **AC-17**).
- **AC-20**: The registry (`distribution` v3) runs in `deployer-system` from this repo's `deploy/`, backed by a Longhorn volume, with htpasswd auth whose credential exists in the cluster only as a `SealedSecret`. It has no Ingress and is reachable only in cluster.
- **AC-21**: From a fresh agent session, uploading `testdata/sample-go` and calling `deploy_app` returns a hostname that answers HTTP 200 from a tailnet device, and a second deploy of the same app returns the same hostname and a release number one higher.

## Decision

**Chosen option**: Option 1: A reconcile loop driving Kubernetes Job builds, with a blocking handler that waits on committed state.

`deploy_app` writes a `queued` deployment and waits for it to reach a terminal state. The single reconcile loop does the work: claim, build Job, digest readback, image inspection, workload composition, readiness wait, `MarkHealthy`. The handler observes; it never acts. Builds run the Paketo `lifecycle -creator` in one unprivileged container, with the deployer's own image as the init container that fetches and safely unpacks the tarball.

**Implementation skills**: `senior-kubernetes-engineer` (`.claude/skills/senior-kubernetes-engineer/`) · `golang-patterns` (`.claude/skills/golang-patterns/`) · `golang-testing` (`.claude/skills/golang-testing/`) · `mcp-server-patterns` (`.claude/skills/mcp-server-patterns/`) · `mcp-builder` (`anthropics/skills`, `.agents/skills/mcp-builder/`) · `docker-patterns` (`.claude/skills/docker-patterns/`) · `security-patterns` (`.claude/skills/security-patterns/`)

## Rationale

The options weighed, why the loop rather than an inline deploy, and the premise note on holding a build inside a blocking call: see [rationale.md](rationale.md).

## Feature design

**Data model sketch**: no new tables, columns, or migrations. Spec 0002's schema already carries everything this feature writes. The rows it touches: `accounts` and `api_tokens` (bootstrap seeding), `uploads` (the tarball and its single use fetch token), `apps` (get or create by name), `deployments` and `deployment_events` (the state walk), `releases` (written by `MarkHealthy`), `audit_log` (one row per call). Step 8 of spec 0002's build plan, the narrow per domain store interfaces, is still open and is a prerequisite here, because the use case packages this feature adds must not import `internal/store`.

**State transitions**: spec 0002's machine, walking the build path only:

```
queued ──▶ building ──▶ pushing ──▶ deploying ──▶ healthy
   └───────────┴────────────┴────────────┴────▶ failed
```

Who moves each arrow, and what has been committed by the time it moves:

| Transition | Trigger | Committed before the action |
|---|---|---|
| (create) to `queued` | `deploy_app`, after the upload resolves | the row and its first event |
| `queued` to `building` | the loop claims the row and creates the build Job | the claim, then the Job name via `RecordBuildResult` |
| `building` to `pushing` | the Job reports `Complete` | nothing yet; the digest is resolved next |
| `pushing` to `deploying` | the pushed tag resolves to a digest, which is recorded, and the image passes the non root check | `image_repo` and `image_digest` |
| `deploying` to `healthy` | the Deployment reports an available updated replica | `MarkHealthy` writes the transition, the event, the release, and `current_release_id` in one transaction |
| any to `failed` | any reason code below, or the sweep finding no live Job | the reason, before any cleanup |

`cancelled` is reached only through supersession, which `DeploymentStore.Create` already owns.

**API surface**:

| Endpoint or tool | Method | Key inputs | Key outputs | Auth | Key errors |
|---|---|---|---|---|---|
| `/v1/uploads` | POST | gzipped tar body, `Content-Length` | `upload_id`, `expires_at` | bearer | 401 unknown token, 413 over `DEPLOYER_MAX_UPLOAD_BYTES`, 400 not gzip |
| `/v1/uploads/{id}` | GET | `Authorization: Bearer <fetch token>` | the tarball bytes | single use fetch token | 401 unknown, 409 already redeemed, 410 expired |
| `deploy_app` (MCP) | tool | `name` (string, required), `upload_id` (string, required) | `name`, `slug`, `url`, `deployment_id`, `release_number`, `image_digest` | bearer | every reason code in **AC-16** |
| `/healthz`, `/readyz` | GET | none | liveness, readiness | none | already built, spec 0003 |

`deploy_app`'s tool description is part of the contract, not decoration. It must state: upload first with `curl -sS -X POST $DEPLOYER_PUBLIC_URL/v1/uploads -H "Authorization: Bearer $TOKEN" --data-binary @- < <(tar czf - .)`, pass the returned id here, the call runs for minutes on a first build, and the app must listen on the port given in `PORT` and run as a non root user.

**Value sourcing**:

| Action | Value produced | Source |
|---|---|---|
| Bootstrap seed | account and token rows | `DEPLOYER_BOOTSTRAP_TOKEN`, hashed with SHA-256 by the auth layer; the account name is the constant `bootstrap` |
| Upload | `path` | `DEPLOYER_UPLOAD_DIR` joined with the generated `upl_` id, never with anything the caller sent |
| Upload | `sha256`, `size_bytes` | computed by the platform while streaming the body to disk |
| Upload | `fetch_token_hash` | at upload, the hash of a random value that is discarded immediately, so the column's NOT NULL and UNIQUE constraints hold and no usable token exists yet |
| Build Job | the raw fetch token | minted by the reconcile loop when it composes the Job, with `uploads.fetch_token_hash` and `redeemed_at` reset to its hash and null in the same write. The raw value goes straight onto the init container as an environment variable and is never persisted or logged. A resumed or retried build mints a fresh one, so a restart between upload and build loses nothing |
| Build Job | the expected `sha256` | `uploads.sha256`, passed to `fetch-source` as an environment variable when the Job is composed, so the init container verifies against the platform's record rather than against anything the archive carries |
| Upload | `expires_at` | upload time plus one hour (spec 0001) |
| `deploy_app` | the resolved account | `AccountStore.ResolveToken` on the SHA-256 of the presented bearer token |
| `deploy_app` | `app_id`, `slug` | existing row for `(account_id, name)`, else `AppStore.Create`, which derives the slug (spec 0002) |
| `deploy_app` | the returned URL | `"https://" + slug + "." + DEPLOYER_APP_DOMAIN` |
| `deploy_app` | `release_number`, `image_digest` in the response | the `Release` returned by `MarkHealthy`, never recomputed |
| Build Job | name | `"build-" + deployment id`, so the Job is findable from the row after a restart |
| Build Job | builder image | `DEPLOYER_BUILDER_IMAGE`, a digest pinned Paketo `jammy-base` reference |
| Build Job | init container image | the control plane's own image, read from `DEPLOYER_SELF_IMAGE` (downward API cannot supply it; the manifest sets it beside the same digest CI pins) |
| Build Job | target image reference | `DEPLOYER_REGISTRY_HOST + "/apps/" + slug + ":" + deployment id` (spec 0001) |
| Build Job | registry credential | `DEPLOYER_REGISTRY_USER` and `DEPLOYER_REGISTRY_PASSWORD`, written by the loop into a per Job `dockerconfigjson` secret in `DEPLOYER_BUILD_NAMESPACE`, owner referenced to the Job so Kubernetes collects it when the Job is reaped |
| Build result | `image_digest`, `image_repo` | resolved by the platform from the registry, by the tag it chose for this deployment, after the Job reports `Complete`. The build container reports nothing back |
| Build failure | the reason | the Job's failed condition, mapped to `build_failed`. The pod's own output stays in its logs and is never read into the platform |
| Image check | the image's user | the image config blob fetched from the registry, in the same call that resolved the digest |
| App workload | Service name and ports | platform constants: name `app`, port 80, target port 8080 |
| App namespace | name and labels | `"app-" + slug`, `apps.id`, `apps.slug` (spec 0003) |
| App namespace | quota and limit values | `DEPLOYER_APP_QUOTA_*` (spec 0003) |
| App workload | container image | the digest, as `repo@sha256:...`, never the tag it was pushed under |
| App workload | environment | exactly `PORT=8080`, a platform constant. `app_config` is not read on this path (slice 7) |
| App workload | security context | fixed in spec 0003, composed by the platform |
| App workload | requests and limits | `DEPLOYER_APP_DEFAULT_CPU`, `_MEMORY` and their limit counterparts, within spec 0003's LimitRange maxima |
| App Ingress | host, class | `slug` plus `DEPLOYER_APP_DOMAIN`, and `DEPLOYER_INGRESS_CLASS_NAME` (spec 0003) |
| Pull secret | contents | `DEPLOYER_REGISTRY_HOST`, `_USER`, `_PASSWORD`, encoded as a docker config by the platform |
| Failure | `failure_reason` | the closed reason code set in **AC-16**, mapped from the internal error; never a wrapped error string |
| Upload instructions | the base address | `DEPLOYER_PUBLIC_URL`, never a request header. The tool description names the environment variables the agent must already hold; the platform never puts a token into a string it hands out |
| Handler wait | the poll interval | `DEPLOYER_RECONCILE_INTERVAL_SECONDS`, reused rather than given a setting of its own, so the handler can never poll faster than state can change |

**Key invariants**:

- Nothing the caller supplies reaches a pod spec. The only caller derived values in any composed manifest are the slug (platform derived from the name, spec 0002) and the image digest (platform computed from a build the platform ran).
- Every manifest is composed field by field in Go. No templating, no YAML strings, no merge of a user map into a spec.
- The image is referenced by digest everywhere. The push tag exists only so the registry has something to name the manifest.
- The reconcile loop is the only writer of deployment state after `queued`. The handler polls committed state and never transitions a row.
- One build at a time, platform wide.
- No registry credential ever reaches an app container. It exists in `deployer-system` (the platform's own), in `deployer-builds` (per Job, collected with the Job), and in `app-<slug>` as an `imagePullSecret` the kubelet consumes and the pod cannot read.
- App objects are created or updated, never deleted and recreated. A deploy that fails partway leaves what it made; the next deploy reconciles it. Deleting anything is slice 9.
- A tarball entry never escapes its extraction root, and a rejected archive stops the deployment before the builder image ever starts.
- The app namespace is created from spec 0003's template or left alone. The platform never edits an existing app namespace's quota, limits, or pod security labels.
- No deployment reaches `healthy` without a digest and a release, because `MarkHealthy` owns that transaction (spec 0002).
- No deployment stays non terminal without a live Kubernetes object behind it (spec 0001), which is what the startup sweep enforces.

**Security model**:

- One account and one token in this slice, seeded from a sealed environment variable and stored only as a SHA-256 hash. It is the platform's whole auth model until feature 8, and it is not a real one: there is no revocation path, no minting, and no per app ownership check beyond the fact that only one account exists. Say so out loud rather than let the single token read as finished.
- Both surfaces authenticate the same way, a bearer token on `Authorization`. The upload endpoint is not exempt because it is not MCP.
- The upload fetch token is a second, separate credential: single use, one hour, hashed at rest, and redeemed by a conditional update (spec 0002). It reaches only the build Job's init container, never the builder and never a log.
- The registry credential grants read and write everywhere it exists, because distribution's htpasswd auth has no per user ACL. A second credential would grant exactly the same rights, so minting one for pulls would be theatre.
- The sharpest consequence, stated plainly: that write credential is mounted into the build container, which is the one place running Buildpacks phases over source the platform did not author. A buildpack that reads its own docker config can push any tag in the registry, including another app's. Nothing in this slice prevents that. What limits it is that the registry has no Ingress, the credential is scoped to one registry with nothing else behind it, the per Job secret is collected with the Job, and every app is deployed by digest, so overwriting a tag does not change a running app. The real close is a registry token service issuing a per build, per repository, push only credential, which is deferred as a follow-up rather than pretended away.
- The pull secret in an app namespace is consumed by the kubelet and is never mounted, projected, or exposed to the app container, so an app cannot read it even though it grants push.
- The build namespace executes code an AI wrote against a tarball the platform did not author. It runs `restricted`, non root, no shell, with a hardened extractor, and holds no cluster credentials beyond the one registry credential it needs to push.
- Untrusted input is validated at both boundaries: the upload body by size and gzip framing before it is stored, and the archive by entry path, type, count, and uncompressed size before anything is written to disk.
- Failure text is a closed set of codes. Build output stays in the Job's pod logs, which nothing exposes until slice 3.
- No regulated data class is in scope, so no compliance regime applies.

**Configuration required** (all validated at startup, **AC-17**):

- `DEPLOYER_BOOTSTRAP_TOKEN`: the single API token, from the platform `SealedSecret`. Required in the cluster, optional locally; when unset, no seeding runs and the platform boots with no usable token, which is logged as a warning.
- `DEPLOYER_PUBLIC_URL`: the platform's own reachable base address, used in the tool description. Required.
- `DEPLOYER_INTERNAL_URL`: the same platform, as reached from inside the cluster, used for the build Job init container's fetch address. Required, and separate from `DEPLOYER_PUBLIC_URL` because the build pod resolves names on cluster DNS, which knows nothing of the tailnet hostname the public address carries.
- `DEPLOYER_BUILDER_IMAGE`: digest pinned Paketo builder. Required.
- `DEPLOYER_SELF_IMAGE`: the control plane's own image reference, reused as the build Job's init container. Required.
- `DEPLOYER_BUILD_NAMESPACE`: already in spec 0001, now given a real value, `deployer-builds`.
- `DEPLOYER_DEPLOY_TIMEOUT_SECONDS`: overall budget for one `deploy_app` call. Default 600.
- `DEPLOYER_BUILD_TIMEOUT_SECONDS`: the build Job's `activeDeadlineSeconds`. Default 480.
- `DEPLOYER_READY_TIMEOUT_SECONDS`: how long to wait for an available replica. Default 90.
- `DEPLOYER_RECONCILE_INTERVAL_SECONDS`: loop tick. Default 2 (spec 0001).
- `DEPLOYER_APP_DEFAULT_CPU`, `DEPLOYER_APP_DEFAULT_MEMORY`: app container requests. Defaults `100m` and `128Mi`.
- `DEPLOYER_APP_LIMIT_CPU`, `DEPLOYER_APP_LIMIT_MEMORY`: app container limits. Defaults `500m` and `512Mi`, inside spec 0003's LimitRange maxima.
- `DEPLOYER_MAX_UPLOAD_FILES`, `DEPLOYER_MAX_EXTRACTED_BYTES`: extraction caps. Defaults 20000 and 512Mi.
- Already validated and unchanged: `DEPLOYER_REGISTRY_HOST`, `_USER`, `_PASSWORD`, `DEPLOYER_APP_DOMAIN`, `DEPLOYER_MAX_UPLOAD_BYTES`, `DEPLOYER_UPLOAD_DIR`, `DEPLOYER_INGRESS_CLASS_NAME`, `DEPLOYER_APP_QUOTA_*`.

**Critical test scenarios**:

- Happy path, no cluster: a fake clientset plus a real SQLite file drives one deployment from `queued` to `healthy` with a stubbed Job completion and digest, producing five event rows in order, one release numbered 1, and a composed Deployment whose container image is a digest reference. Verifies **AC-5**, **AC-13**, **AC-14**.
- Happy path, real cluster: `testdata/sample-go` uploaded and deployed answers 200 on its hostname from a tailnet device, and a second deploy returns the same hostname with release 2. Verifies **AC-21**, **AC-4**.
- Manifest composition: golden tests over the composed Deployment, Service, Ingress, and pull secret assert the security context, `PORT=8080`, the digest reference, the absence of a `tls` block, and that no field carries a caller supplied string. Verifies **AC-12**, **AC-13**.
- Failure case, hostile archive: tarballs containing `/etc/passwd`, `../../escape`, a symlink to `/`, 30000 files, and a gzip bomb are each rejected by the extractor with `source_rejected`, and nothing is written outside the extraction root. Verifies **AC-8**, **AC-16**.
- Failure case, root image: an image config reporting user `0` or empty fails with `image_runs_as_root` and the fake clientset records no Deployment create for that app. Verifies **AC-10**.
- Failure case, build: a Job that ends failed, and a Job that ends complete but whose tag resolves to nothing, produce `build_failed` and `build_no_digest`, and neither leaks build output into `failure_reason`. Verifies **AC-9**, **AC-16**.
- Failure case, partial deploy: a composition that fails after the namespace and pull secret exist leaves them in place, and the next deploy of the same app succeeds without any manual cleanup. Verifies **AC-13**.
- Cleanup: a deployment reaching `healthy` and one reaching `failed` both leave no file behind in `DEPLOYER_UPLOAD_DIR`. Verifies **AC-22**.
- Failure case, never ready: a Deployment that never reports an available replica fails with `app_never_ready` after `DEPLOYER_READY_TIMEOUT_SECONDS`. Verifies **AC-16**, **AC-17**.
- Failure case, restart: with a non terminal row on disk and its Job absent from the fake clientset, the startup sweep fails it with a reason; with the Job present, it resumes. Verifies **AC-18**.
- Concurrency: two `deploy_app` calls for the same app supersede correctly (spec 0002's `Create`), and the loop still runs one build at a time. Verifies **AC-6**.
- Auth and permission: an absent, unknown, or revoked token gets 401 on `/v1/uploads` and a denial from `deploy_app`, each writing one `audit_log` row with `account_id` null. An `upload_id` belonging to another account fails without creating a deployment. Verifies **AC-2**, **AC-3**, **AC-19**.
- Bootstrap: seeding twice with the same token leaves one account and one token row, and the raw token appears in no log line at any level. Verifies **AC-1**.
- Startup: booting with `DEPLOYER_BUILD_TIMEOUT_SECONDS=soon` fails naming that variable. Verifies **AC-17**.

## Build plan

Tracer Bullet, so the thread is threaded before any segment is thickened. Steps 1 to 7 are the thinnest thing that can carry one real deploy; 8 onward harden it. No migration: spec 0002's schema already covers this feature.

1. [x] Close spec 0002's open step 8 first: define the narrow per domain store interfaces in the consuming packages and have `internal/store` satisfy them, so nothing this feature adds imports the store. Prerequisite, satisfies no AC of its own.
2. [x] Add the registry to `deploy/`: `distribution` v3, a Longhorn PVC, a ClusterIP Service, and the htpasswd `SealedSecret`, plus the `deployer-builds` namespace with `enforce: restricted`. Prove a push and a pull by hand from inside the cluster. Satisfies **AC-20**, and the namespace half of **AC-7**.
3. [x] Add every new setting to `internal/config` with validation and table tests, and the bootstrap seeding at startup. Satisfies **AC-1**, **AC-17**.
4. [x] Build the upload endpoint and its auth middleware: bearer resolution, the size capped streaming write, the SHA-256, the fetch token, and the audit row. Satisfies **AC-2**, **AC-19**.
5. [x] Add the `fetch-source` subcommand with the hardened extractor and its hostile archive tests, and the `GET /v1/uploads/{id}` redeem path. Satisfies **AC-8**.
6. [x] Compose and create the build Job: the per Job registry secret owner referenced to it, the init container with its freshly minted fetch token and expected `sha256`, the pinned builder running `lifecycle -creator`, the security context, `backoffLimit: 0`, and the deadlines. Satisfies **AC-7**, **AC-9**.
7. [x] Compose the app side: namespace from the template, pull secret, Deployment, Service, Ingress, all field by field and all create or update, with golden tests. Satisfies **AC-11**, **AC-12**, **AC-13**.
8. [x] Wire the reconcile loop: the serial claim, the tick, the phase transitions, the readiness wait, `MarkHealthy`, and deleting the tarball on any terminal state. Satisfies **AC-5**, **AC-6**, **AC-14**, **AC-22**.
9. [x] Add the registry client: resolve the pushed tag to a digest, and read the image config for the non root check, before any workload is composed. Satisfies **AC-9**, **AC-10**.
10. [x] Add the MCP server and the one `deploy_app` tool, with its description, its wait on committed state, and its success response. Satisfies **AC-3**, **AC-4**, **AC-15**.
11. [x] Map every internal error to the closed reason code set, at both the response and the `failure_reason` boundary, and confirm no build output crosses either. Satisfies **AC-16**.
12. [x] Add the startup sweep for non terminal deployments, resuming a live Job and failing a vanished one. Satisfies **AC-18**.
13. Add `testdata/sample-go` (landed) and run the real deploy from an agent session, twice, confirming the stable hostname and release 2. Satisfies **AC-21**.

## Consequences

**Positive**:

- The pipe exists. Every later slice thickens a segment that has carried real traffic rather than one that is still theoretical.
- The deploy path is restart safe from the first day, so slice 2 adds a status tool rather than rewriting how deploys run.
- Spec 0002's schema and spec 0003's namespace template both get exercised by the code that will use them forever, which is where their design errors will surface if there are any.
- The platform composes every manifest itself from the start, so slice 5 hardens fields rather than retrofitting a boundary.
- Nothing new joins the cluster except the registry, which is a container with a volume and no control loop.

**Negative / tradeoffs**:

- The blocking call will time out on some clients before a cold build finishes. See the premise note in [rationale.md](rationale.md); this is the sharpest edge in the slice and slice 2 is the fix.
- Auth is one seeded token with no revocation. Anyone holding it is the platform's only account, and until feature 8 the ownership checks in the schema are enforcing a rule with one subject.
- The registry's write credential is mounted into the build container, beside code the platform did not author. This is the sharpest security tradeoff in the slice, it is not fully mitigated, and the Security model says so rather than dressing it up.
- No build cache means every deploy is a cold build, so every deploy is minutes rather than seconds. Fine at one caller, and the reason the timeout budget is generous.
- One build at a time is a platform wide serialisation point, and because the call blocks, a second caller's request sits open for up to the full deploy budget just waiting in the queue. Correct, and a ceiling a second user would feel immediately.
- The tool description is now load bearing documentation: if the upload instructions in it drift from the endpoint, agents fail in a way no test catches.
- Two images are pinned by digest (the builder and the platform's own, reused as the init container), so a builder bump is a deliberate commit, and a stale pin is invisible until something breaks.

**Neutral**:

- The `pushing` state is a moment rather than a phase, since `lifecycle -creator` builds and pushes in one process. It is entered when the Job completes and left when the digest resolves, which is honest enough and keeps spec 0002's machine unchanged.
- The digest comes from the registry rather than from the build container, which deviates from spec 0001's build handoff contract. It removes a wrapper image, a `report.toml` parser, and a 4096 byte truncation risk, and it costs nothing, because the platform already contacts the registry for the non root check. Spec 0001 gets a follow-up note.
- `deployer-builds` is set to `restricted`, which is where spec 0003's open question is expected to land. It is not proven until the lifecycle actually runs there; if it does not fit, that is a finding for build step 6 and a spec update, never a right granted in advance.
- The sample app lives in `testdata/`, so CI can tar it without shipping it in the image.

## Follow-up

- [ ] Slice 2 is not optional polish. Until a status tool exists, a client timeout leaves the agent with no way to learn the outcome of a deploy that is still running. Treat it as the next slice, not a later one.
- [ ] Feature 8 owns real token minting and revocation. The bootstrap token has neither, and nothing stops it being reused forever. Do not treat the platform as authenticated until that lands.
- [ ] Close the build container's push credential properly. A registry token service issuing a per build, per repository, push only credential is the real fix; until then a compromised buildpack can push any tag in the registry. Weigh it against the same rejection spec 0001 made of extra infrastructure, now that the exposure is concrete rather than theoretical.
- [ ] Spec 0001's build handoff contract says the digest returns through `terminationMessagePath`. This spec resolves it from the registry instead, because `lifecycle -creator` writes `report.toml` and not that file. Note the deviation on spec 0001, or supersede that row when the build proves it.
- [ ] Confirm the Paketo `jammy-base` builder tag and its digest at build time. The reference in this spec is from knowledge, not a verified fetch, and builder tags move.
- [ ] `DEPLOYER_SELF_IMAGE` duplicates the digest CI already pins into `deploy/deployment.yaml`. Decide whether the Kustomize build derives it from the same place, so the two cannot drift.
- [ ] Agent Skill and MCP discovery for Cloud Native Buildpacks and the distribution registry was declined. Record the decline in root `AGENTS.md` so it is not offered again.
- [ ] `AGENTS.md` does not yet record the reason code set or the rule that the tool description carries the upload contract. Both are project wide once this lands.
- [ ] The registry has no garbage collection. Every deploy pushes a new image and nothing ever deletes one, so the registry volume grows without bound. Decide a retention or `registry garbage-collect` story before it fills.
